package store

import (
	"context"
	"testing"
)

// TestRecentCheckIns_NewestFirstByEventTime pins the UI-overhaul sort
// contract: ORDER BY timestamp DESC, id DESC. Backfill/replay batches
// (reconnect sweeps, boot-time since-midnight replays) get insert ids
// that diverge from tap order, so the old `ORDER BY id DESC` rendered
// them oldest-or-arbitrary first. Rows are inserted with timestamps
// deliberately OUT of insert order to prove event time wins.
func TestRecentCheckIns_NewestFirstByEventTime(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Insert order: middle, newest, oldest. RFC3339 and SQLite space
	// format both appear, mirroring the two production writers. The
	// mixed formats are the sharp edge: raw string ORDER BY sorts
	// ' ' before 'T', which would sink the space-format NEW row below
	// both RFC3339 rows — datetime() normalization is what keeps this
	// ordering chronological.
	events := []CheckInEvent{
		{Timestamp: "2026-07-25T12:00:00Z", NfcUID: "MID", Result: "allowed"},
		{Timestamp: "2026-07-25 14:30:00", NfcUID: "NEW", Result: "allowed"},
		{Timestamp: "2026-07-25T09:00:00Z", NfcUID: "OLD", Result: "denied"},
	}
	for i := range events {
		if _, err := s.RecordCheckIn(ctx, &events[i]); err != nil {
			t.Fatalf("RecordCheckIn %s: %v", events[i].NfcUID, err)
		}
	}

	got, err := s.RecentCheckIns(ctx, 10)
	if err != nil {
		t.Fatalf("RecentCheckIns: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	want := []string{"NEW", "MID", "OLD"}
	for i, uid := range want {
		if got[i].NfcUID != uid {
			order := make([]string, len(got))
			for j, e := range got {
				order[j] = e.NfcUID + "@" + e.Timestamp
			}
			t.Fatalf("position %d: got %s, want %s; full order: %v",
				i, got[i].NfcUID, uid, order)
		}
	}
}

// TestRecentCheckIns_IDTiebreakSameSecond — two taps in the same second
// keep insert order (newest insert first) via the id DESC tiebreak.
func TestRecentCheckIns_IDTiebreakSameSecond(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	ts := "2026-07-25T12:00:00Z"
	for _, uid := range []string{"FIRST", "SECOND"} {
		if _, err := s.RecordCheckIn(ctx, &CheckInEvent{
			Timestamp: ts, NfcUID: uid, Result: "allowed",
		}); err != nil {
			t.Fatalf("RecordCheckIn %s: %v", uid, err)
		}
	}

	got, err := s.RecentCheckIns(ctx, 10)
	if err != nil {
		t.Fatalf("RecentCheckIns: %v", err)
	}
	if len(got) != 2 || got[0].NfcUID != "SECOND" || got[1].NfcUID != "FIRST" {
		t.Fatalf("same-second tiebreak wrong: got %+v", got)
	}
}

// TestDisagreementEvents_NewestFirstByEventTime — the shadow-decisions
// panel shares the event-time ordering contract.
func TestDisagreementEvents_NewestFirstByEventTime(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	events := []CheckInEvent{
		{Timestamp: "2026-07-25T12:00:00Z", NfcUID: "MID", Result: "denied", UnifiResult: "ACCESS"},
		{Timestamp: "2026-07-25T14:30:00Z", NfcUID: "NEW", Result: "denied", UnifiResult: "ACCESS"},
	}
	for i := range events {
		if _, err := s.RecordCheckIn(ctx, &events[i]); err != nil {
			t.Fatalf("RecordCheckIn: %v", err)
		}
	}

	got, err := s.DisagreementEvents(ctx, 10)
	if err != nil {
		t.Fatalf("DisagreementEvents: %v", err)
	}
	if len(got) != 2 || got[0].NfcUID != "NEW" {
		t.Fatalf("disagreements not newest-first: got %+v", got)
	}
}
