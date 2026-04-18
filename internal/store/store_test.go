package store

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := Open(t.TempDir(), logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestMemberCRUD(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Insert
	m := &Member{
		NfcUID: "AABB1122", CustomerID: "cust-1",
		FirstName: "Alex", LastName: "Smith",
		BadgeStatus: "ACTIVE", Active: true,
		CachedAt: "2025-01-01T00:00:00Z",
	}
	if err := s.UpsertMember(ctx, m); err != nil {
		t.Fatal(err)
	}

	// Lookup by NFC
	got, err := s.GetMemberByNFC(ctx, "AABB1122")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.CustomerID != "cust-1" {
		t.Fatalf("GetMemberByNFC: got %+v", got)
	}
	if !got.IsAllowed() {
		t.Error("expected IsAllowed() == true")
	}

	// Lookup by customer ID
	got, err = s.GetMemberByCustomerID(ctx, "cust-1")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.NfcUID != "AABB1122" {
		t.Fatalf("GetMemberByCustomerID: got %+v", got)
	}

	// Update
	m.BadgeStatus = "FROZEN"
	if err := s.UpsertMember(ctx, m); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetMemberByNFC(ctx, "AABB1122")
	if got.BadgeStatus != "FROZEN" {
		t.Errorf("badge not updated: %s", got.BadgeStatus)
	}
	if got.IsAllowed() {
		t.Error("FROZEN member should not be allowed")
	}

	// Stats
	stats, err := s.MemberStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Total != 1 || stats.Frozen != 1 {
		t.Errorf("stats: %+v", stats)
	}

	// Remove
	if err := s.RemoveMember(ctx, "AABB1122"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetMemberByNFC(ctx, "AABB1122")
	if got != nil {
		t.Error("member should be removed")
	}
}

func TestCustomerCRUD(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Upsert batch
	customers := []Customer{
		{RedpointID: "r1", FirstName: "Alice", LastName: "Johnson", Email: "alice@example.com", Active: true},
		{RedpointID: "r2", FirstName: "Bob", LastName: "Johnson", Email: "bob@example.com", Active: true},
		{RedpointID: "r3", FirstName: "Charlie", LastName: "Smith", Email: "charlie@example.com", Active: false},
	}
	if err := s.UpsertCustomerBatch(ctx, customers); err != nil {
		t.Fatal(err)
	}

	// Count
	count, err := s.CustomerCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Errorf("count = %d, want 3", count)
	}

	// Search by name
	results, err := s.SearchCustomersByName(ctx, "Alice", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].RedpointID != "r1" {
		t.Errorf("name search: %+v", results)
	}

	// Search by last name
	results, err = s.SearchCustomersByLastName(ctx, "Johnson")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Errorf("last name search: got %d, want 2", len(results))
	}

	// Search by email
	cust, err := s.SearchCustomersByEmail(ctx, "BOB@EXAMPLE.COM")
	if err != nil {
		t.Fatal(err)
	}
	if cust == nil || cust.RedpointID != "r2" {
		t.Errorf("email search: %+v", cust)
	}
}

func TestCheckInEvents(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Record events
	evt1 := &CheckInEvent{
		NfcUID: "AA11", CustomerID: "cust-1", CustomerName: "Alice Smith",
		DoorID: "door-1", DoorName: "Front Door", Result: "allowed",
	}
	id1, err := s.RecordCheckIn(ctx, evt1)
	if err != nil {
		t.Fatal(err)
	}
	if id1 == 0 {
		t.Error("expected non-zero ID")
	}

	evt2 := &CheckInEvent{
		NfcUID: "BB22", CustomerID: "", CustomerName: "",
		DoorID: "door-1", DoorName: "Front Door", Result: "denied", DenyReason: "unknown card",
	}
	_, err = s.RecordCheckIn(ctx, evt2)
	if err != nil {
		t.Fatal(err)
	}

	// Mark recorded
	if err := s.MarkRedpointRecorded(ctx, id1, "rp-123"); err != nil {
		t.Fatal(err)
	}

	// Recent
	events, err := s.RecentCheckIns(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	// Most recent first
	if events[0].Result != "denied" {
		t.Errorf("expected denied first, got %s", events[0].Result)
	}

	// Customer history
	history, err := s.CheckInsForCustomer(ctx, "cust-1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || !history[0].RedpointRecorded {
		t.Errorf("customer history: %+v", history)
	}

	// Stats
	stats, err := s.CheckInStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalAllTime != 2 || stats.TotalToday != 2 {
		t.Errorf("stats: %+v", stats)
	}
}

func TestDoorPolicies(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// No policy → nil
	p, err := s.GetDoorPolicy(ctx, "door-1")
	if err != nil {
		t.Fatal(err)
	}
	if p != nil {
		t.Error("expected nil for unconfigured door")
	}

	// Create policy
	policy := &DoorPolicy{
		DoorID:        "door-1",
		DoorName:      "Front Door",
		Policy:        "membership",
		AllowedBadges: "Monthly,Annual",
	}
	if err := s.UpsertDoorPolicy(ctx, policy); err != nil {
		t.Fatal(err)
	}

	// Evaluate - allowed badge
	member := &Member{BadgeStatus: "ACTIVE", Active: true, BadgeName: "Monthly"}
	allowed, _ := policy.EvaluateAccess(member)
	if !allowed {
		t.Error("Monthly member should be allowed")
	}

	// Evaluate - wrong badge
	member.BadgeName = "DayPass"
	allowed, reason := policy.EvaluateAccess(member)
	if allowed {
		t.Error("DayPass should be denied")
	}
	if reason == "" {
		t.Error("should have deny reason")
	}

	// Staff-only door
	staffPolicy := &DoorPolicy{DoorID: "door-2", Policy: "staff_only"}
	allowed, _ = staffPolicy.EvaluateAccess(member)
	if allowed {
		t.Error("staff_only should deny regular members")
	}

	// Open door
	openPolicy := &DoorPolicy{DoorID: "door-3", Policy: "open"}
	allowed, _ = openPolicy.EvaluateAccess(member)
	if !allowed {
		t.Error("open door should allow everyone")
	}
}

func TestJobLifecycle(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Create
	if err := s.CreateJob(ctx, "job-1", "ingest"); err != nil {
		t.Fatal(err)
	}

	// Active check
	active, err := s.ActiveJob(ctx, "ingest")
	if err != nil {
		t.Fatal(err)
	}
	if active == nil || active.ID != "job-1" {
		t.Fatal("expected active job")
	}

	// Update progress
	if err := s.UpdateJobProgress(ctx, "job-1", map[string]int{"matched": 10}); err != nil {
		t.Fatal(err)
	}

	// Complete
	if err := s.CompleteJob(ctx, "job-1", map[string]int{"total": 50}); err != nil {
		t.Fatal(err)
	}

	// No longer active
	active, _ = s.ActiveJob(ctx, "ingest")
	if active != nil {
		t.Error("should have no active job after completion")
	}

	// Get by ID
	j, err := s.GetJob(ctx, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != "completed" {
		t.Errorf("status = %s, want completed", j.Status)
	}
}

func TestMemberNotFound(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	m, err := s.GetMemberByNFC(ctx, "NONEXISTENT")
	if err != nil {
		t.Fatal(err)
	}
	if m != nil {
		t.Error("expected nil for nonexistent member")
	}
}
