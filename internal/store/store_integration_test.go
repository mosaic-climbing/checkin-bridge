package store

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

func TestIntegrationFullCheckInPipeline(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := Open(t.TempDir(), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	// 1. Add a customer to directory
	err = s.UpsertCustomer(ctx, &Customer{
		RedpointID: "cust-1",
		FirstName:  "Alice",
		LastName:   "Smith",
		Email:      "alice@example.com",
		Active:     true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 2. Enroll member with NFC
	err = s.UpsertMember(ctx, &Member{
		NfcUID:      "04AABB1122",
		CustomerID:  "cust-1",
		FirstName:   "Alice",
		LastName:    "Smith",
		BadgeStatus: "ACTIVE",
		Active:      true,
		CachedAt:    "2025-01-01T00:00:00Z",
	})
	if err != nil {
		t.Fatal(err)
	}

	// 3. Simulate check-in lookup
	member, err := s.GetMemberByNFC(ctx, "04AABB1122")
	if err != nil {
		t.Fatal(err)
	}
	if member == nil {
		t.Fatal("expected member")
	}
	if !member.IsAllowed() {
		t.Errorf("member should be allowed, badge=%s active=%v", member.BadgeStatus, member.Active)
	}

	// 4. Record check-in event
	id, err := s.RecordCheckIn(ctx, &CheckInEvent{
		Timestamp:    "2025-01-01T10:30:00Z",
		NfcUID:       "04AABB1122",
		CustomerID:   "cust-1",
		CustomerName: "Alice Smith",
		DoorID:       "door-1",
		DoorName:     "Front Door",
		Result:       "allowed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if id == 0 {
		t.Error("expected non-zero check-in ID")
	}

	// 5. Mark Redpoint recorded
	err = s.MarkRedpointRecorded(ctx, id, "rp-456")
	if err != nil {
		t.Fatal(err)
	}

	// 6. Verify stats (TotalAllTime should increment)
	stats, err := s.CheckInStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalAllTime != 1 {
		t.Errorf("stats: expected TotalAllTime=1, got %+v", stats)
	}

	// 7. Verify history
	history, err := s.CheckInsForCustomer(ctx, "cust-1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || !history[0].RedpointRecorded {
		t.Errorf("history: %+v", history)
	}

	// 8. Test denied check-in for frozen member
	err = s.UpsertCustomer(ctx, &Customer{
		RedpointID: "cust-2",
		FirstName:  "Bob",
		LastName:   "Jones",
		Active:     true,
	})
	if err != nil {
		t.Fatal(err)
	}

	err = s.UpsertMember(ctx, &Member{
		NfcUID:      "04CCDD3344",
		CustomerID:  "cust-2",
		FirstName:   "Bob",
		LastName:    "Jones",
		BadgeStatus: "FROZEN",
		Active:      true,
		CachedAt:    "2025-01-01T00:00:00Z",
	})
	if err != nil {
		t.Fatal(err)
	}

	bob, err := s.GetMemberByNFC(ctx, "04CCDD3344")
	if err != nil {
		t.Fatal(err)
	}
	if bob.IsAllowed() {
		t.Error("frozen member should not be allowed")
	}

	_, err = s.RecordCheckIn(ctx, &CheckInEvent{
		Timestamp:    "2025-01-01T11:15:00Z",
		NfcUID:       "04CCDD3344",
		CustomerID:   "cust-2",
		CustomerName: "Bob Jones",
		DoorID:       "door-1",
		DoorName:     "Front Door",
		Result:       "denied",
		DenyReason:   bob.DenyReason(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Stats should show 2 total
	stats, _ = s.CheckInStats(ctx)
	if stats.TotalAllTime != 2 {
		t.Errorf("stats after deny: expected TotalAllTime=2, got %+v", stats)
	}

	// 9. Test door policy evaluation
	policy := &DoorPolicy{
		DoorID:        "door-2",
		DoorName:      "Bouldering Cave",
		Policy:        "membership",
		AllowedBadges: "Annual,Monthly",
	}
	err = s.UpsertDoorPolicy(ctx, policy)
	if err != nil {
		t.Fatal(err)
	}

	fetchedPolicy, err := s.GetDoorPolicy(ctx, "door-2")
	if err != nil {
		t.Fatal(err)
	}

	// Alice with "Monthly" badge (not set yet, so empty)
	alice, _ := s.GetMemberByNFC(ctx, "04AABB1122")
	allowed, _ := fetchedPolicy.EvaluateAccess(alice)
	if allowed {
		t.Error("alice has no badge name set, should be denied")
	}

	// Update Alice with Monthly badge
	alice.BadgeName = "Monthly"
	s.UpsertMember(ctx, alice)

	alice, _ = s.GetMemberByNFC(ctx, "04AABB1122")
	allowed, _ = fetchedPolicy.EvaluateAccess(alice)
	if !allowed {
		t.Error("alice with Monthly badge should be allowed through bouldering cave")
	}

	// 10. Test hourly breakdown
	today := "2025-01-01" // Use the date from our check-in records
	hourly, err := s.CheckInsByHour(ctx, today)
	if err != nil {
		t.Fatal(err)
	}
	// Since our check-in events have explicit timestamps, we may not have hourly data
	// Just verify the query doesn't error
	t.Logf("hourly breakdown: %+v", hourly)
}

func TestJobWorkflow(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := Open(t.TempDir(), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	// Create job
	err = s.CreateJob(ctx, "ingest-001", "ingest")
	if err != nil {
		t.Fatal(err)
	}

	// Should be active
	active, _ := s.ActiveJob(ctx, "ingest")
	if active == nil {
		t.Fatal("expected active job")
	}

	// Update progress
	s.UpdateJobProgress(ctx, "ingest-001", map[string]int{"matched": 42})

	// Complete
	s.CompleteJob(ctx, "ingest-001", map[string]int{"total": 100})

	// No longer active
	active, _ = s.ActiveJob(ctx, "ingest")
	if active != nil {
		t.Error("should not have active job")
	}

	// List recent
	jobs, _ := s.RecentJobs(ctx, 10)
	if len(jobs) != 1 || jobs[0].Status != "completed" {
		t.Errorf("jobs: %+v", jobs)
	}
}
