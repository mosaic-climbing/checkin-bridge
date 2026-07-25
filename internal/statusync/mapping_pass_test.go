package statusync

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/mosaic-climbing/checkin-bridge/internal/redpoint"
	"github.com/mosaic-climbing/checkin-bridge/internal/store"
	"github.com/mosaic-climbing/checkin-bridge/internal/testutil"
	"github.com/mosaic-climbing/checkin-bridge/internal/unifi"
)

// Tests for runMappingStatusPass — the mapping-driven status engine that
// replaces legacy Step 2 when LegacyNFCStatusLoop=false. The engine's
// contract: ua_user_mappings → members → IsAllowed() decides desired
// UA-Hub status; StatusWrites mode gates the writes ("off" holds all,
// "activate-only" holds deactivations, "full" applies all); the
// mass-deactivation guard vetoes the whole deactivation set when a run
// wants more than MaxDeactivationsPerRun.

func buildMappingPassSyncer(t *testing.T, cfg Config) (*Syncer, *testutil.FakeUniFi, *store.Store) {
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

	if cfg.RateLimitDelay == 0 {
		cfg.RateLimitDelay = time.Millisecond
	}
	s := New(ua, rp, db, cfg, false /* shadowMode; StatusWrites drives gating */, nil, logger)
	return s, fakeUA, db
}

// seedMappedMember creates the mapping + members pair the pass joins:
// uaID → customerID in ua_user_mappings, and a members row carrying the
// (cache-syncer-refreshed) badge state.
func seedMappedMember(t *testing.T, db *store.Store, uaID, customerID, badge string, active bool) {
	t.Helper()
	ctx := context.Background()
	if err := db.UpsertMapping(ctx, &store.Mapping{
		UAUserID: uaID, RedpointCustomer: customerID, MatchedBy: MatchSourceEmail,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertMember(ctx, &store.Member{
		NfcUID:      "tok-" + uaID,
		CustomerID:  customerID,
		FirstName:   "User",
		LastName:    uaID,
		BadgeStatus: badge,
		Active:      active,
		CachedAt:    time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMappingPass_FullMode_DeactivatesFrozenMember(t *testing.T) {
	s, fakeUA, db := buildMappingPassSyncer(t, Config{StatusWrites: "full"})
	seedMappedMember(t, db, "ua-1", "rp-1", "FROZEN", true)

	r := &SyncResult{}
	s.runMappingStatusPass(context.Background(),
		[]unifi.UniFiUser{{ID: "ua-1", Status: "ACTIVE"}}, r)

	if r.Matched != 1 || r.Deactivated != 1 || r.HeldDeactivations != 0 {
		t.Errorf("counters = %+v; want Matched 1, Deactivated 1, held 0", r)
	}
	if got := fakeUA.StatusUpdateCount(); got != 1 {
		t.Errorf("UA updates = %d, want 1", got)
	}
}

func TestMappingPass_FullMode_ActivatesRenewedMember(t *testing.T) {
	s, fakeUA, db := buildMappingPassSyncer(t, Config{StatusWrites: "full"})
	seedMappedMember(t, db, "ua-2", "rp-2", "ACTIVE", true)

	r := &SyncResult{}
	s.runMappingStatusPass(context.Background(),
		[]unifi.UniFiUser{{ID: "ua-2", Status: "DEACTIVATED"}}, r)

	if r.Activated != 1 || r.Deactivated != 0 {
		t.Errorf("counters = %+v; want Activated 1", r)
	}
	if got := fakeUA.StatusUpdateCount(); got != 1 {
		t.Errorf("UA updates = %d, want 1", got)
	}
}

func TestMappingPass_ActivateOnly_HoldsDeactivationsAppliesActivations(t *testing.T) {
	s, fakeUA, db := buildMappingPassSyncer(t, Config{StatusWrites: "activate-only"})
	seedMappedMember(t, db, "ua-frozen", "rp-f", "FROZEN", true)  // wants deactivate
	seedMappedMember(t, db, "ua-renewed", "rp-r", "ACTIVE", true) // wants activate

	r := &SyncResult{}
	s.runMappingStatusPass(context.Background(), []unifi.UniFiUser{
		{ID: "ua-frozen", Status: "ACTIVE"},
		{ID: "ua-renewed", Status: "DEACTIVATED"},
	}, r)

	if r.Activated != 1 {
		t.Errorf("Activated = %d, want 1 (activations apply in activate-only)", r.Activated)
	}
	if r.Deactivated != 1 || r.HeldDeactivations != 1 {
		t.Errorf("Deactivated/Held = %d/%d, want 1/1 (decision counted, write held)",
			r.Deactivated, r.HeldDeactivations)
	}
	if got := fakeUA.StatusUpdateCount(); got != 1 {
		t.Errorf("UA updates = %d, want exactly 1 (the activation only)", got)
	}
}

func TestMappingPass_OffMode_HoldsEverything(t *testing.T) {
	s, fakeUA, db := buildMappingPassSyncer(t, Config{StatusWrites: "off"})
	seedMappedMember(t, db, "ua-frozen", "rp-f", "FROZEN", true)
	seedMappedMember(t, db, "ua-renewed", "rp-r", "ACTIVE", true)

	r := &SyncResult{}
	s.runMappingStatusPass(context.Background(), []unifi.UniFiUser{
		{ID: "ua-frozen", Status: "ACTIVE"},
		{ID: "ua-renewed", Status: "DEACTIVATED"},
	}, r)

	if r.Activated != 1 || r.Deactivated != 1 || r.HeldDeactivations != 1 {
		t.Errorf("counters = %+v; want decisions counted (1 activate, 1 deactivate held)", r)
	}
	if got := fakeUA.StatusUpdateCount(); got != 0 {
		t.Errorf("UA updates = %d, want 0 in off mode", got)
	}
}

func TestMappingPass_GuardTripsOnMassDeactivation(t *testing.T) {
	s, fakeUA, db := buildMappingPassSyncer(t, Config{
		StatusWrites:           "full",
		MaxDeactivationsPerRun: 3,
	})
	// Four frozen members (over the cap of 3) + one renewal.
	var users []unifi.UniFiUser
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("ua-f%d", i)
		seedMappedMember(t, db, id, "rp-"+id, "FROZEN", true)
		users = append(users, unifi.UniFiUser{ID: id, Status: "ACTIVE"})
	}
	seedMappedMember(t, db, "ua-ok", "rp-ok", "ACTIVE", true)
	users = append(users, unifi.UniFiUser{ID: "ua-ok", Status: "DEACTIVATED"})

	r := &SyncResult{}
	s.runMappingStatusPass(context.Background(), users, r)

	if !r.GuardTripped {
		t.Fatal("GuardTripped = false, want true (4 deactivations > cap 3)")
	}
	if r.HeldDeactivations != 4 {
		t.Errorf("HeldDeactivations = %d, want 4 (all held atomically)", r.HeldDeactivations)
	}
	if r.Activated != 1 {
		t.Errorf("Activated = %d, want 1 (activations are never guarded)", r.Activated)
	}
	if got := fakeUA.StatusUpdateCount(); got != 1 {
		t.Errorf("UA updates = %d, want exactly 1 (the activation)", got)
	}
}

func TestMappingPass_GuardNotTrippedAtCap(t *testing.T) {
	s, fakeUA, db := buildMappingPassSyncer(t, Config{
		StatusWrites:           "full",
		MaxDeactivationsPerRun: 3,
	})
	var users []unifi.UniFiUser
	for i := 0; i < 3; i++ { // exactly at cap
		id := fmt.Sprintf("ua-f%d", i)
		seedMappedMember(t, db, id, "rp-"+id, "EXPIRED", true)
		users = append(users, unifi.UniFiUser{ID: id, Status: "ACTIVE"})
	}

	r := &SyncResult{}
	s.runMappingStatusPass(context.Background(), users, r)

	if r.GuardTripped {
		t.Error("GuardTripped at exactly the cap; guard must be strictly-greater-than")
	}
	if got := fakeUA.StatusUpdateCount(); got != 3 {
		t.Errorf("UA updates = %d, want 3", got)
	}
}

func TestMappingPass_UnmappedAndUnmaterialisedUsers(t *testing.T) {
	s, fakeUA, db := buildMappingPassSyncer(t, Config{StatusWrites: "full"})
	// ua-nomap: no mapping at all (PIN-only staff or pending).
	// ua-nomember: mapping exists but ingest hasn't written the members row.
	if err := db.UpsertMapping(context.Background(), &store.Mapping{
		UAUserID: "ua-nomember", RedpointCustomer: "rp-x", MatchedBy: MatchSourceEmail,
	}); err != nil {
		t.Fatal(err)
	}

	r := &SyncResult{}
	s.runMappingStatusPass(context.Background(), []unifi.UniFiUser{
		{ID: "ua-nomap", Status: "ACTIVE"},
		{ID: "ua-nomember", Status: "ACTIVE"},
	}, r)

	if r.Unmatched != 1 {
		t.Errorf("Unmatched = %d, want 1 (ua-nomap)", r.Unmatched)
	}
	if r.MappedNoCache != 1 {
		t.Errorf("MappedNoCache = %d, want 1 (ua-nomember)", r.MappedNoCache)
	}
	if got := fakeUA.StatusUpdateCount(); got != 0 {
		t.Errorf("UA updates = %d, want 0 — neither user may be written", got)
	}
}

func TestMappingPass_InSyncUserUnchanged(t *testing.T) {
	s, fakeUA, db := buildMappingPassSyncer(t, Config{StatusWrites: "full"})
	seedMappedMember(t, db, "ua-good", "rp-good", "ACTIVE", true)

	r := &SyncResult{}
	s.runMappingStatusPass(context.Background(),
		[]unifi.UniFiUser{{ID: "ua-good", Status: "ACTIVE"}}, r)

	if r.Unchanged != 1 || r.Activated != 0 || r.Deactivated != 0 {
		t.Errorf("counters = %+v; want Unchanged 1 only", r)
	}
	if got := fakeUA.StatusUpdateCount(); got != 0 {
		t.Errorf("UA updates = %d, want 0", got)
	}
}

// TestNew_StatusWritesResolution pins the mode-resolution contract: an
// empty StatusWrites inherits from the positional shadowMode argument so
// pre-split call sites keep their exact behavior.
func TestNew_StatusWritesResolution(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cases := []struct {
		cfgMode string
		shadow  bool
		want    string
	}{
		{"", true, "off"},
		{"", false, "full"},
		{"off", false, "off"},
		{"activate-only", true, "activate-only"},
		{"full", true, "full"},
	}
	for _, c := range cases {
		s := New(nil, nil, nil, Config{StatusWrites: c.cfgMode}, c.shadow, nil, logger)
		if s.statusWrites != c.want {
			t.Errorf("New(StatusWrites=%q, shadow=%v).statusWrites = %q, want %q",
				c.cfgMode, c.shadow, s.statusWrites, c.want)
		}
	}
}
