package cache

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mosaic-climbing/checkin-bridge/internal/redpoint"
	"github.com/mosaic-climbing/checkin-bridge/internal/store"
	"github.com/mosaic-climbing/checkin-bridge/internal/testutil"
)

// Regression tests for the outage-marks-everyone-DELETED bug: a
// Redpoint outage used to be indistinguishable from "every customer
// was deleted" because RefreshCustomers swallowed per-ID errors, so
// RefreshAllStatuses flipped the whole cache to DELETED while the sync
// job reported green.

func seedRefreshMember(t *testing.T, db *store.Store, customerID string) {
	t.Helper()
	if err := db.UpsertMember(context.Background(), &store.Member{
		NfcUID:      "TOK-" + strings.ToUpper(customerID),
		CustomerID:  customerID,
		FirstName:   "Test",
		LastName:    customerID,
		BadgeStatus: "ACTIVE",
		Active:      true,
		CachedAt:    time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshAllStatuses_OutageLeavesStatusesUntouchedAndFailsJob(t *testing.T) {
	logger := discardLogger()
	db, err := store.Open(t.TempDir(), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedRefreshMember(t, db, "rp-1")
	seedRefreshMember(t, db, "rp-2")

	// Dead Redpoint: server closed before the refresh runs.
	fakeRP := testutil.NewFakeRedpoint()
	deadURL := fakeRP.GraphQLURL()
	fakeRP.Close()

	rp := redpoint.NewClient(deadURL, "k", "TST", logger)
	s := NewSyncer(db, rp, SyncConfig{SyncInterval: time.Hour}, logger)

	err = s.RefreshAllStatuses(context.Background())
	if err == nil {
		t.Fatal("RefreshAllStatuses returned nil during a total outage; the job must fail so the outage is visible")
	}
	if !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("error = %v, want mention of unreachable customers", err)
	}

	// The core invariant: nobody got marked DELETED/inactive.
	for _, id := range []string{"rp-1", "rp-2"} {
		m, gerr := db.GetMemberByNFC(context.Background(), "TOK-"+strings.ToUpper(id))
		if gerr != nil || m == nil {
			t.Fatalf("member %s lookup: %v %v", id, m, gerr)
		}
		if !m.Active || m.BadgeStatus != "ACTIVE" {
			t.Errorf("member %s = active=%v badge=%s after outage; must be untouched (was ACTIVE)",
				id, m.Active, m.BadgeStatus)
		}
	}
}

func TestRefreshAllStatuses_ConfirmedDeletionStillMarksDeleted(t *testing.T) {
	logger := discardLogger()
	db, err := store.Open(t.TempDir(), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedRefreshMember(t, db, "rp-gone")

	// Live fake Redpoint that does NOT know rp-gone: the customer(id:)
	// query succeeds and returns null — a confirmed deletion.
	fakeRP := testutil.NewFakeRedpoint()
	defer fakeRP.Close()

	rp := redpoint.NewClient(fakeRP.GraphQLURL(), "k", "TST", logger)
	s := NewSyncer(db, rp, SyncConfig{SyncInterval: time.Hour}, logger)

	if err := s.RefreshAllStatuses(context.Background()); err != nil {
		t.Fatalf("RefreshAllStatuses: %v (confirmed deletions are not job failures)", err)
	}

	m, gerr := db.GetMemberByNFC(context.Background(), "TOK-RP-GONE")
	if gerr != nil || m == nil {
		t.Fatalf("member lookup: %v %v", m, gerr)
	}
	if m.Active || m.BadgeStatus != "DELETED" {
		t.Errorf("confirmed-deleted member = active=%v badge=%s, want inactive DELETED", m.Active, m.BadgeStatus)
	}
}
