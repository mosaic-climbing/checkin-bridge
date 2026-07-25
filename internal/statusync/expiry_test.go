package statusync

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/mosaic-climbing/checkin-bridge/internal/redpoint"
	"github.com/mosaic-climbing/checkin-bridge/internal/store"
	"github.com/mosaic-climbing/checkin-bridge/internal/testutil"
	"github.com/mosaic-climbing/checkin-bridge/internal/unifi"
)

// buildExpiryTestSyncer wires a Syncer against a real FakeUniFi + FakeRedpoint.
// Returns the syncer, the fake UA server (so tests can inspect StatusUpdates),
// and the db (so tests can seed pending rows and inspect audit/pending state).
func buildExpiryTestSyncer(t *testing.T, shadow bool) (*Syncer, *testutil.FakeUniFi, *store.Store) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	fakeUA := testutil.NewFakeUniFi()
	t.Cleanup(fakeUA.Close)
	fakeRP := testutil.NewFakeRedpoint()
	t.Cleanup(fakeRP.Close)

	ua := unifi.NewClient("wss://unused", fakeUA.BaseURL(), "test-token", 500, "", logger)
	rp := redpoint.NewClient(fakeRP.GraphQLURL(), "test-api-key", "TEST", logger)

	db, err := store.Open(t.TempDir(), logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	s := New(ua, rp, db, Config{
		SyncInterval:       time.Hour,
		RateLimitDelay:     time.Millisecond,
		UnmatchedGraceDays: 7,
	}, shadow, nil /* metrics */, logger)
	return s, fakeUA, db
}

// seedExpired drops a pending row whose grace_until is already in the
// past, so runExpiryPhase picks it up immediately.
func seedExpired(t *testing.T, db *store.Store, uaUserID, reason string) {
	t.Helper()
	past := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	if err := db.UpsertPending(context.Background(), &store.Pending{
		UAUserID:   uaUserID,
		Reason:     reason,
		GraceUntil: past,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestRunExpiryPhase_AlertOnly_NeverDeactivates — the ratified policy:
// an overdue unmatched user is surfaced (Expired count → digest push)
// but NEVER auto-deactivated. No UA-Hub write, no audit row, and the
// pending row stays for staff to resolve in the Needs Match panel. A
// wrong auto-deactivation is member-facing and not self-healing (the
// denied-tap recheck cannot resolve an unmapped user), which is why
// this is a hard contract in LIVE mode, not just shadow.
func TestRunExpiryPhase_AlertOnly_NeverDeactivates(t *testing.T) {
	s, fakeUA, db := buildExpiryTestSyncer(t, false /* live mode */)
	seedExpired(t, db, "ua-expired", store.PendingReasonNoMatch)

	r := &SyncResult{}
	s.runExpiryPhase(context.Background(), r)

	if r.Expired != 1 {
		t.Errorf("Expired = %d, want 1 (overdue count for the digest)", r.Expired)
	}
	if r.Deactivated != 0 {
		t.Errorf("Deactivated = %d, want 0 — expiry must never deactivate", r.Deactivated)
	}
	if r.Errors != 0 {
		t.Errorf("Errors = %d, want 0", r.Errors)
	}
	if got := fakeUA.StatusUpdateCount(); got != 0 {
		t.Errorf("UA status update count = %d, want 0 (alert-only)", got)
	}
	if p, _ := db.GetPending(context.Background(), "ua-expired"); p == nil {
		t.Error("pending row was removed; must stay for staff to resolve")
	}
	audits, _ := db.ListMatchAudit(context.Background(), "ua-expired", 0)
	if len(audits) != 0 {
		t.Errorf("audits = %d, want 0 (no mutation happened)", len(audits))
	}
}

// TestRunExpiryPhase_Shadow_SameAsLive — alert-only expiry behaves
// identically in shadow and live mode (there is no mutation for shadow
// to suppress); this pins that flipping modes can't change expiry
// behaviour.
func TestRunExpiryPhase_Shadow_SameAsLive(t *testing.T) {
	s, fakeUA, db := buildExpiryTestSyncer(t, true)
	seedExpired(t, db, "ua-shadow", store.PendingReasonAmbiguousEmail)

	r := &SyncResult{}
	s.runExpiryPhase(context.Background(), r)

	if r.Expired != 1 || r.Deactivated != 0 {
		t.Errorf("counters = Expired %d / Deactivated %d; want 1/0", r.Expired, r.Deactivated)
	}
	if got := fakeUA.StatusUpdateCount(); got != 0 {
		t.Errorf("shadow mode sent %d UniFi update(s); want 0", got)
	}
	if p, _ := db.GetPending(context.Background(), "ua-shadow"); p == nil {
		t.Error("pending row must be preserved")
	}
}

// TestRunExpiryPhase_NotYetExpired_Ignored — rows whose grace_until is
// still in the future must be left untouched.
func TestRunExpiryPhase_NotYetExpired_Ignored(t *testing.T) {
	s, fakeUA, db := buildExpiryTestSyncer(t, false)

	future := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	if err := db.UpsertPending(context.Background(), &store.Pending{
		UAUserID: "ua-waiting", Reason: store.PendingReasonNoMatch, GraceUntil: future,
	}); err != nil {
		t.Fatal(err)
	}

	r := &SyncResult{}
	s.runExpiryPhase(context.Background(), r)

	if r.Expired != 0 || r.Deactivated != 0 {
		t.Errorf("counters = Expired %d / Deactivated %d; want 0/0", r.Expired, r.Deactivated)
	}
	if got := fakeUA.StatusUpdateCount(); got != 0 {
		t.Errorf("UA updates = %d, want 0", got)
	}
	if p, _ := db.GetPending(context.Background(), "ua-waiting"); p == nil {
		t.Error("pending row for not-yet-expired user was deleted")
	}
}

// TestRunExpiryPhase_MultipleRows — two overdue rows both surface in the
// count; neither is touched.
func TestRunExpiryPhase_MultipleRows(t *testing.T) {
	s, fakeUA, db := buildExpiryTestSyncer(t, false)
	seedExpired(t, db, "ua-A", store.PendingReasonNoEmail)
	seedExpired(t, db, "ua-B", store.PendingReasonAmbiguousName)

	r := &SyncResult{}
	s.runExpiryPhase(context.Background(), r)

	if r.Expired != 2 || r.Deactivated != 0 {
		t.Errorf("counters = Expired %d / Deactivated %d; want 2/0", r.Expired, r.Deactivated)
	}
	if got := fakeUA.StatusUpdateCount(); got != 0 {
		t.Errorf("UA updates = %d, want 0 (alert-only)", got)
	}
}

// TestRunExpiryPhase_ContextCancelled — if the context is cancelled
// mid-loop, the phase returns cleanly without processing further rows.
func TestRunExpiryPhase_ContextCancelled(t *testing.T) {
	s, _, db := buildExpiryTestSyncer(t, false)
	seedExpired(t, db, "ua-first", store.PendingReasonNoMatch)
	seedExpired(t, db, "ua-second", store.PendingReasonNoMatch)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel
	r := &SyncResult{}
	s.runExpiryPhase(ctx, r)

	// Depending on loop ordering we may or may not have entered the body;
	// the key contract is "no panic, reasonable state".
	if r.Errors != 0 {
		t.Errorf("Errors = %d; want 0 (cancellation is not an error)", r.Errors)
	}
}

// TestRunMatchingPhase_SkipsAlreadyMapped — the matching phase must not
// re-match UA users who already have a mapping row. This is the
// fast-path that keeps the sync from hammering Redpoint for every
// already-bound user every day.
func TestRunMatchingPhase_SkipsAlreadyMapped(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	fakeRP := testutil.NewFakeRedpoint()
	defer fakeRP.Close()
	fakeUA := testutil.NewFakeUniFi()
	defer fakeUA.Close()

	ua := unifi.NewClient("wss://unused", fakeUA.BaseURL(), "test-token", 500, "", logger)
	rp := redpoint.NewClient(fakeRP.GraphQLURL(), "test-api-key", "TEST", logger)
	db, err := store.Open(t.TempDir(), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Pre-seed a mapping for ua-bound — the matcher must leave them alone.
	if err := db.UpsertMapping(context.Background(), &store.Mapping{
		UAUserID: "ua-bound", RedpointCustomer: "rp-1", MatchedBy: MatchSourceEmail,
	}); err != nil {
		t.Fatal(err)
	}

	s := New(ua, rp, db, Config{
		RateLimitDelay: time.Millisecond, UnmatchedGraceDays: 7,
	}, false /* shadowMode */, nil /* metrics */, logger)

	users := []unifi.UniFiUser{
		{ID: "ua-bound", FirstName: "Already", LastName: "Mapped", Email: "x@example.com"},
		{ID: "ua-fresh", FirstName: "Fresh", LastName: "User", Email: "fresh@example.com"},
	}
	r := &SyncResult{UniFiUsers: 2}
	s.runMatchingPhase(context.Background(), users, r)

	if r.Matching != 1 {
		t.Errorf("Matching = %d, want 1 (only ua-fresh should have been matched)", r.Matching)
	}
	// ua-bound's mapping must still exist untouched.
	m, _ := db.GetMapping(context.Background(), "ua-bound")
	if m == nil || m.RedpointCustomer != "rp-1" {
		t.Errorf("ua-bound mapping = %+v; want untouched", m)
	}
	// ua-fresh has no Redpoint customer with that email, so it ends up
	// pending(no_match) rather than matched.
	if r.NewlyMatched != 0 {
		t.Errorf("NewlyMatched = %d, want 0", r.NewlyMatched)
	}
	if r.NewlyPending != 1 {
		t.Errorf("NewlyPending = %d, want 1", r.NewlyPending)
	}
	p, _ := db.GetPending(context.Background(), "ua-fresh")
	if p == nil {
		t.Error("ua-fresh should have a pending row")
	}
}
