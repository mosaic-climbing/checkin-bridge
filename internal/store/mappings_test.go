package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestMappingUpsertAndLookup(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	m := &Mapping{
		UAUserID:         "ua-user-1",
		RedpointCustomer: "rp-cust-1",
		MatchedBy:        "auto:email",
	}
	if err := s.UpsertMapping(ctx, m); err != nil {
		t.Fatalf("UpsertMapping: %v", err)
	}
	if m.MatchedAt == "" {
		t.Error("UpsertMapping should populate MatchedAt when empty")
	}

	got, err := s.GetMapping(ctx, "ua-user-1")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.RedpointCustomer != "rp-cust-1" {
		t.Fatalf("GetMapping: %+v", got)
	}
	if got.MatchedBy != "auto:email" {
		t.Errorf("MatchedBy = %q", got.MatchedBy)
	}

	// Reverse lookup (drives the "customer already bound" refusal path).
	rev, err := s.GetMappingByCustomerID(ctx, "rp-cust-1")
	if err != nil {
		t.Fatal(err)
	}
	if rev == nil || rev.UAUserID != "ua-user-1" {
		t.Fatalf("GetMappingByCustomerID: %+v", rev)
	}

	// Absent keys must return (nil, nil), never sql.ErrNoRows (store convention).
	nope, err := s.GetMapping(ctx, "does-not-exist")
	if err != nil || nope != nil {
		t.Errorf("GetMapping(missing) = (%v, %v), want (nil, nil)", nope, err)
	}
	nope2, err := s.GetMappingByCustomerID(ctx, "does-not-exist")
	if err != nil || nope2 != nil {
		t.Errorf("GetMappingByCustomerID(missing) = (%v, %v), want (nil, nil)", nope2, err)
	}
}

func TestMappingUniqueCustomerBinding(t *testing.T) {
	// The UNIQUE (redpoint_customer_id) constraint is what enforces
	// "one UA-Hub user per Redpoint customer" — a concurrent match would
	// otherwise leave two UA-Hub users both claiming the same person.
	s := testStore(t)
	ctx := context.Background()

	if err := s.UpsertMapping(ctx, &Mapping{
		UAUserID: "ua-A", RedpointCustomer: "rp-X", MatchedBy: "auto:email",
	}); err != nil {
		t.Fatal(err)
	}
	err := s.UpsertMapping(ctx, &Mapping{
		UAUserID: "ua-B", RedpointCustomer: "rp-X", MatchedBy: "auto:email",
	})
	if err == nil {
		t.Fatal("expected UNIQUE violation when two UA users claim the same customer")
	}
}

func TestMappingUpsertPreservesEmailSyncedOnReupsert(t *testing.T) {
	// The ON CONFLICT clause preserves last_email_synced_at unless the new
	// row carries a non-empty value. This matters because the match-path
	// writes the mapping but not the email-synced timestamp; only a
	// successful UpdateUser(email=...) call should advance it.
	s := testStore(t)
	ctx := context.Background()

	if err := s.UpsertMapping(ctx, &Mapping{
		UAUserID: "ua-1", RedpointCustomer: "rp-1", MatchedBy: "auto:email",
		LastEmailSyncedAt: "2026-04-01T03:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}

	// A subsequent match-path upsert does NOT include a synced-at value.
	if err := s.UpsertMapping(ctx, &Mapping{
		UAUserID: "ua-1", RedpointCustomer: "rp-1", MatchedBy: "staff:chris",
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetMapping(ctx, "ua-1")
	if got.LastEmailSyncedAt != "2026-04-01T03:00:00Z" {
		t.Errorf("LastEmailSyncedAt = %q, want preserved", got.LastEmailSyncedAt)
	}
	if got.MatchedBy != "staff:chris" {
		t.Errorf("MatchedBy = %q, want staff:chris", got.MatchedBy)
	}

	// TouchMappingEmailSynced advances the timestamp explicitly.
	newTime := time.Date(2026, 4, 16, 3, 0, 0, 0, time.UTC)
	if err := s.TouchMappingEmailSynced(ctx, "ua-1", newTime); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetMapping(ctx, "ua-1")
	if got.LastEmailSyncedAt != "2026-04-16T03:00:00Z" {
		t.Errorf("LastEmailSyncedAt = %q, want advanced", got.LastEmailSyncedAt)
	}
}

func TestMappingDelete(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if err := s.UpsertMapping(ctx, &Mapping{
		UAUserID: "ua-1", RedpointCustomer: "rp-1", MatchedBy: "auto:email",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteMapping(ctx, "ua-1"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetMapping(ctx, "ua-1")
	if got != nil {
		t.Errorf("GetMapping after delete = %+v, want nil", got)
	}
	// Deleting again must be a no-op (idempotent).
	if err := s.DeleteMapping(ctx, "ua-1"); err != nil {
		t.Errorf("second DeleteMapping returned %v, want nil", err)
	}
}

func TestPendingLifecycle(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	grace := time.Now().Add(7 * 24 * time.Hour).UTC().Format(time.RFC3339)
	p := &Pending{
		UAUserID:   "ua-pending-1",
		Reason:     PendingReasonNoMatch,
		GraceUntil: grace,
		Candidates: "",
	}
	if err := s.UpsertPending(ctx, p); err != nil {
		t.Fatal(err)
	}
	if p.FirstSeen == "" || p.LastSeen == "" {
		t.Error("UpsertPending should populate FirstSeen/LastSeen on first insert")
	}
	firstSeen := p.FirstSeen

	// Re-upserting must preserve first_seen (the grace window's anchor) but
	// refresh reason/grace_until/candidates. This is the per-sync update
	// path: every time the syncer walks unmatched users it re-asserts the
	// pending row, without resetting how long they've been waiting.
	p2 := &Pending{
		UAUserID:   "ua-pending-1",
		Reason:     PendingReasonAmbiguousEmail,
		FirstSeen:  "1999-01-01T00:00:00Z", // deliberately wrong — must be ignored by SQL
		GraceUntil: grace,
		Candidates: "rp-X|rp-Y",
	}
	if err := s.UpsertPending(ctx, p2); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetPending(ctx, "ua-pending-1")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("GetPending returned nil for existing row")
	}
	if got.FirstSeen != firstSeen {
		t.Errorf("FirstSeen drifted: got %q, want %q (preserved from first insert)", got.FirstSeen, firstSeen)
	}
	if got.Reason != PendingReasonAmbiguousEmail {
		t.Errorf("Reason = %q, want %q", got.Reason, PendingReasonAmbiguousEmail)
	}
	if got.Candidates != "rp-X|rp-Y" {
		t.Errorf("Candidates = %q, want rp-X|rp-Y", got.Candidates)
	}

	// PendingCount and AllPending.
	if err := s.UpsertPending(ctx, &Pending{
		UAUserID: "ua-pending-2", Reason: PendingReasonNoEmail, GraceUntil: grace,
	}); err != nil {
		t.Fatal(err)
	}
	n, err := s.PendingCount(ctx)
	if err != nil || n != 2 {
		t.Errorf("PendingCount = %d, err=%v; want 2", n, err)
	}
	all, err := s.AllPending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("AllPending len = %d, want 2", len(all))
	}

	// Delete should be idempotent.
	if err := s.DeletePending(ctx, "ua-pending-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePending(ctx, "ua-pending-1"); err != nil {
		t.Errorf("second DeletePending returned %v, want nil", err)
	}
}

func TestPendingExpired(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	past := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	future := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)

	if err := s.UpsertPending(ctx, &Pending{
		UAUserID: "ua-expired", Reason: PendingReasonNoMatch, GraceUntil: past,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertPending(ctx, &Pending{
		UAUserID: "ua-still-waiting", Reason: PendingReasonNoMatch, GraceUntil: future,
	}); err != nil {
		t.Fatal(err)
	}

	expired, err := s.ExpiredPending(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 1 || expired[0].UAUserID != "ua-expired" {
		t.Errorf("ExpiredPending returned %+v, want exactly [ua-expired]", expired)
	}
}

func TestMatchAuditAppendAndList(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := s.AppendMatchAudit(ctx, &MatchAudit{
			UAUserID: "ua-X",
			Field:    "mapping",
			AfterVal: "rp-Y",
			Source:   "auto:email",
		}); err != nil {
			t.Fatal(err)
		}
	}
	// A different user's row should not leak into the list.
	if err := s.AppendMatchAudit(ctx, &MatchAudit{
		UAUserID: "ua-Z",
		Field:    "mapping",
		Source:   "staff:chris",
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := s.ListMatchAudit(ctx, "ua-X", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Errorf("ListMatchAudit len = %d, want 3", len(rows))
	}
	for _, r := range rows {
		if r.UAUserID != "ua-X" {
			t.Errorf("leaked row for %q", r.UAUserID)
		}
		if !strings.HasPrefix(r.Source, "auto:") {
			t.Errorf("Source = %q, want auto:*", r.Source)
		}
	}
	// Newest-first ordering: ids descend.
	if rows[0].ID <= rows[1].ID || rows[1].ID <= rows[2].ID {
		t.Errorf("ListMatchAudit not newest-first: ids %d, %d, %d", rows[0].ID, rows[1].ID, rows[2].ID)
	}

	// Limit respected.
	rows, _ = s.ListMatchAudit(ctx, "ua-X", 2)
	if len(rows) != 2 {
		t.Errorf("ListMatchAudit(limit=2) len = %d, want 2", len(rows))
	}
}
