package store

import (
	"context"
	"testing"
	"time"
)

// TestDisagreementEvents_PartitionsByAgreement covers the four shapes of
// tap result the shadow-decisions panel must classify:
//
//  1. UniFi ACCESS,  bridge allowed  → agree, NOT in disagreements
//  2. UniFi BLOCKED, bridge denied   → agree, NOT in disagreements
//  3. UniFi ACCESS,  bridge denied   → would-miss, IN disagreements
//  4. UniFi BLOCKED, bridge allowed  → would-admit, IN disagreements
//
// Plus rows with no UniFi verdict (pre-migration / events without Result)
// must never surface — there is nothing to compare against.
func TestDisagreementEvents_PartitionsByAgreement(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	now := time.Now().UTC().Format(time.RFC3339)

	// Seed all six shapes. ID order is newest-last so we can assert ordering.
	seeds := []*CheckInEvent{
		// 1. agree: ACCESS + allowed
		{Timestamp: now, NfcUID: "A1", CustomerName: "Agree Allow", DoorName: "Front",
			Result: "allowed", UnifiResult: "ACCESS"},
		// 2. agree: BLOCKED + denied
		{Timestamp: now, NfcUID: "A2", CustomerName: "Agree Deny", DoorName: "Front",
			Result: "denied", DenyReason: "not_found", UnifiResult: "BLOCKED"},
		// 3. would-miss: ACCESS + denied (paying member we'd lock out)
		{Timestamp: now, NfcUID: "D1", CustomerName: "Miss One", DoorName: "Front",
			Result: "denied", DenyReason: "stale_cache", UnifiResult: "ACCESS"},
		// 4. would-admit: BLOCKED + allowed (UniFi policy the bridge would override)
		{Timestamp: now, NfcUID: "D2", CustomerName: "Admit One", DoorName: "Bouldering",
			Result: "allowed", UnifiResult: "BLOCKED"},
		// also: recheck_allowed counts as allowed for UI purposes
		{Timestamp: now, NfcUID: "D3", CustomerName: "Admit Two", DoorName: "Bouldering",
			Result: "recheck_allowed", UnifiResult: "BLOCKED"},
		// Unknown: no UniFi verdict at all — must be ignored
		{Timestamp: now, NfcUID: "U1", CustomerName: "Unknown", DoorName: "Front",
			Result: "denied", DenyReason: "not_found", UnifiResult: ""},
	}
	for _, evt := range seeds {
		if _, err := s.RecordCheckIn(ctx, evt); err != nil {
			t.Fatalf("RecordCheckIn %s: %v", evt.CustomerName, err)
		}
	}

	disagree, err := s.DisagreementEvents(ctx, 50)
	if err != nil {
		t.Fatalf("DisagreementEvents: %v", err)
	}

	// We expect exactly the three disagreement rows (#3, #4, #5).
	if got, want := len(disagree), 3; got != want {
		t.Fatalf("len(disagreements) = %d, want %d", got, want)
	}

	// Newest-first ordering: recheck_allowed was inserted last.
	names := []string{disagree[0].CustomerName, disagree[1].CustomerName, disagree[2].CustomerName}
	want := []string{"Admit Two", "Admit One", "Miss One"}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("disagree[%d] = %q, want %q (full order: %v)", i, names[i], want[i], names)
		}
	}

	// The agree rows must not show up.
	for _, r := range disagree {
		if r.CustomerName == "Agree Allow" || r.CustomerName == "Agree Deny" {
			t.Errorf("agreement row %q leaked into disagreements", r.CustomerName)
		}
		if r.UnifiResult == "" {
			t.Errorf("row %q with empty UnifiResult leaked into disagreements", r.CustomerName)
		}
	}

	// ── Counter view ────────────────────────────────────────
	stats, err := s.ShadowDecisionStatsToday(ctx)
	if err != nil {
		t.Fatalf("ShadowDecisionStatsToday: %v", err)
	}
	if stats.Total != 6 {
		t.Errorf("Total = %d, want 6", stats.Total)
	}
	if stats.Agree != 2 {
		t.Errorf("Agree = %d, want 2", stats.Agree)
	}
	if stats.Disagree != 3 {
		t.Errorf("Disagree = %d, want 3", stats.Disagree)
	}
	if stats.Unknown != 1 {
		t.Errorf("Unknown = %d, want 1", stats.Unknown)
	}
	if stats.WouldMiss != 1 {
		t.Errorf("WouldMiss = %d, want 1", stats.WouldMiss)
	}
	if stats.WouldAdmit != 2 {
		t.Errorf("WouldAdmit = %d, want 2 (allowed + recheck_allowed vs BLOCKED)", stats.WouldAdmit)
	}
}

// TestDisagreementEvents_Empty makes sure an untouched store returns
// an empty slice, not an error — the panel should render cleanly on
// a fresh install.
func TestDisagreementEvents_Empty(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	rows, err := s.DisagreementEvents(ctx, 10)
	if err != nil {
		t.Fatalf("DisagreementEvents on empty store: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected 0 rows on empty store, got %d", len(rows))
	}

	stats, err := s.ShadowDecisionStatsToday(ctx)
	if err != nil {
		t.Fatalf("ShadowDecisionStatsToday: %v", err)
	}
	if stats.Total != 0 || stats.Disagree != 0 {
		t.Errorf("expected zero stats, got %+v", stats)
	}
}
