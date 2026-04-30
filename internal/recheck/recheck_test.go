package recheck

// Unit tests for Service.RecheckDeniedTap. The whole point of A3 was that
// this path became testable without spinning up a SQLite store, a GraphQL
// transport, or a UA-Hub REST endpoint — so the tests here use tiny hand-
// rolled fakes that satisfy the narrow Store/RedpointClient/UnifiClient
// interfaces defined alongside Service.
//
// The tests cover:
//
//  1. The four-quadrant decision matrix (UA=allow|deny × Redpoint=allow|deny).
//     The handler only calls recheck after UA already denied, so the only
//     two rows tested are the UA=deny cases, each crossed with whether
//     Redpoint currently says the member is active.
//
//  2. The freshness gate (MaxStaleness). A zero MaxStaleness must reproduce
//     the pre-A3 behaviour (always recheck). A non-zero value must short-
//     circuit the Redpoint call when the cache is fresh enough, and must
//     NOT short-circuit when the cache is older than the budget.
//
//  3. The breaker. Consecutive Redpoint failures must trip it, the tripped
//     breaker must short-circuit subsequent calls (no live query), and the
//     cooldown-then-success path must close it again. The breaker tests
//     here exercise Service behaviour end-to-end (not the breaker unit in
//     isolation — that's breaker_test.go).
//
//  4. Edge cases: unknown NFC card (cache miss), shadow mode (no UA
//     mutation), UA ListUsers failure (reactivated=true + specific Reason).

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mosaic-climbing/checkin-bridge/internal/redpoint"
	"github.com/mosaic-climbing/checkin-bridge/internal/store"
	"github.com/mosaic-climbing/checkin-bridge/internal/unifi"
)

// ─── Fakes ──────────────────────────────────────────────────────

// fakeStore is an in-memory Store. GetMemberByNFC returns the canned
// member (or nil if notFound is set); UpsertMember captures the last
// upsert so tests can assert what the recheck wrote back to the cache.
type fakeStore struct {
	mu         sync.Mutex
	member     *store.Member
	notFound   bool
	getErr     error
	upsertErr  error
	lastUpsert *store.Member
}

func (f *fakeStore) GetMemberByNFC(ctx context.Context, nfcUID string) (*store.Member, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.notFound {
		return nil, nil
	}
	// Return a shallow copy so the caller can mutate freely without
	// perturbing our canned member.
	m := *f.member
	return &m, nil
}

func (f *fakeStore) UpsertMember(ctx context.Context, m *store.Member) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upsertErr != nil {
		return f.upsertErr
	}
	cp := *m
	f.lastUpsert = &cp
	return nil
}

// fakeRedpoint is a stub RedpointClient. calls counts the number of
// RefreshCustomers invocations (used to prove the breaker short-circuit).
// The err+customers fields let a test pre-program the next response.
type fakeRedpoint struct {
	mu        sync.Mutex
	customers []*redpoint.Customer
	err       error
	calls     int
}

func (f *fakeRedpoint) RefreshCustomers(ctx context.Context, customerIDs []string) ([]*redpoint.Customer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.customers, nil
}

// callCount reads the call counter under the same mutex the mutator
// uses so the race detector stays happy when breaker cooldown tests
// race the timer against the assertion.
func (f *fakeRedpoint) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeUnifi stubs the UnifiClient. lastUpdate records the (id, status)
// pair of the most recent UpdateUserStatus call — the primary assertion
// target for "member reactivated" tests.
type fakeUnifi struct {
	mu           sync.Mutex
	users        []unifi.UniFiUser
	listErr      error
	updateErr    error
	lastUpdateID string
	lastStatus   string
	updateCalls  int
}

func (f *fakeUnifi) ListUsers(ctx context.Context) ([]unifi.UniFiUser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.users, nil
}

func (f *fakeUnifi) UpdateUserStatus(ctx context.Context, userID, status string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateCalls++
	if f.updateErr != nil {
		return f.updateErr
	}
	f.lastUpdateID = userID
	f.lastStatus = status
	return nil
}

// quietLogger drops all log output. Each test gets its own so log-
// level assertions would be possible if needed; we just don't want the
// -v output to drown in recheck info lines.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// newService is the shared fixture builder — keeps the per-test setup
// terse and makes it impossible to forget to wire the logger.
func newService(cfg Config, s Store, rp RedpointClient, ua UnifiClient) *Service {
	return New(s, rp, ua, cfg, quietLogger())
}

// denied returns a canned cached Member that fails IsAllowed() — the
// only shape the recheck path ever sees (upstream handler already
// denied, so we know cache said deny).
func denied(cachedAt string) *store.Member {
	return &store.Member{
		NfcUID:      "NFC123",
		CustomerID:  "cust-1",
		FirstName:   "Alex",
		LastName:    "Honnold",
		BadgeStatus: "FROZEN",
		Active:      false,
		CachedAt:    cachedAt,
	}
}

// activeCustomer returns a Redpoint Customer shaped like a currently-
// allowed member. Used in the "Redpoint=allow" quadrant.
func activeCustomer() *redpoint.Customer {
	return &redpoint.Customer{
		ID:        "cust-1",
		Active:    true,
		FirstName: "Alex",
		LastName:  "Honnold",
		Badge: &redpoint.BadgeStatus{
			Status: "ACTIVE",
			CustomerBadge: &redpoint.CustomerBadge{
				Name: "Monthly",
			},
		},
	}
}

// frozenCustomer returns a Redpoint Customer that still fails the
// recheck — used in the "Redpoint=deny" quadrant.
func frozenCustomer() *redpoint.Customer {
	return &redpoint.Customer{
		ID:        "cust-1",
		Active:    false,
		FirstName: "Alex",
		LastName:  "Honnold",
		Badge: &redpoint.BadgeStatus{
			Status: "FROZEN",
		},
	}
}

// ─── Four-quadrant matrix ───────────────────────────────────────

// TestRecheck_UADeny_RedpointAllow is the reactivation happy path.
// UA-Hub denied → cache says denied → Redpoint live query says active →
// we update the store and flip UA-Hub to ACTIVE. Reactivated must be
// true; the store must carry the fresh BadgeStatus; UA-Hub must have
// been called with the right user ID.
func TestRecheck_UADeny_RedpointAllow(t *testing.T) {
	s := &fakeStore{member: denied("")}
	rp := &fakeRedpoint{customers: []*redpoint.Customer{activeCustomer()}}
	ua := &fakeUnifi{users: []unifi.UniFiUser{
		{ID: "unifi-1", NfcTokens: []string{"NFC123"}},
	}}
	svc := newService(Config{}, s, rp, ua)

	res, err := svc.RecheckDeniedTap(context.Background(), "NFC123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil || !res.Reactivated {
		t.Fatalf("expected Reactivated=true, got %+v", res)
	}
	if res.CustomerID != "cust-1" {
		t.Errorf("CustomerID: want cust-1, got %q", res.CustomerID)
	}
	if s.lastUpsert == nil {
		t.Fatal("expected store.UpsertMember to be called")
	}
	if s.lastUpsert.BadgeStatus != "ACTIVE" {
		t.Errorf("upserted BadgeStatus: want ACTIVE, got %q", s.lastUpsert.BadgeStatus)
	}
	if !s.lastUpsert.Active {
		t.Error("upserted Active: want true")
	}
	if ua.lastUpdateID != "unifi-1" {
		t.Errorf("UA update id: want unifi-1, got %q", ua.lastUpdateID)
	}
	if ua.lastStatus != "ACTIVE" {
		t.Errorf("UA update status: want ACTIVE, got %q", ua.lastStatus)
	}
}

// TestRecheck_UADeny_RedpointDeny is the "denial is correct" quadrant.
// UA-Hub denied → Redpoint also says still not active → Reactivated
// must be false, no UA update, no store upsert (nothing changed).
func TestRecheck_UADeny_RedpointDeny(t *testing.T) {
	s := &fakeStore{member: denied("")}
	rp := &fakeRedpoint{customers: []*redpoint.Customer{frozenCustomer()}}
	ua := &fakeUnifi{}
	svc := newService(Config{}, s, rp, ua)

	res, err := svc.RecheckDeniedTap(context.Background(), "NFC123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Reactivated {
		t.Fatalf("expected Reactivated=false, got %+v", res)
	}
	if !strings.Contains(res.Reason, "still not allowed") {
		t.Errorf("Reason: want 'still not allowed', got %q", res.Reason)
	}
	if s.lastUpsert != nil {
		t.Error("store.UpsertMember should not be called on still-denied")
	}
	if ua.updateCalls != 0 {
		t.Errorf("UA.UpdateUserStatus should not be called on still-denied (got %d calls)", ua.updateCalls)
	}
}

// TestRecheck_CacheMiss — the NFC tap is for a card we've never seen
// locally. The recheck returns a Result with a reason explaining the
// miss; Reactivated is false and no Redpoint/UA calls are made.
func TestRecheck_CacheMiss(t *testing.T) {
	s := &fakeStore{notFound: true}
	rp := &fakeRedpoint{}
	ua := &fakeUnifi{}
	svc := newService(Config{}, s, rp, ua)

	res, err := svc.RecheckDeniedTap(context.Background(), "NFC-unknown")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Reactivated {
		t.Error("cache miss must not reactivate")
	}
	if !strings.Contains(res.Reason, "unknown card") {
		t.Errorf("Reason: want 'unknown card', got %q", res.Reason)
	}
	if rp.callCount() != 0 {
		t.Errorf("Redpoint should not be queried on cache miss (got %d calls)", rp.callCount())
	}
}

// TestRecheck_CustomerNotInRedpoint — the card's customer ID doesn't
// exist upstream (deletion, data drift, etc). The recheck must NOT
// reactivate and must not count as a breaker failure (application-level
// "not found" is not an upstream health signal).
func TestRecheck_CustomerNotInRedpoint(t *testing.T) {
	s := &fakeStore{member: denied("")}
	rp := &fakeRedpoint{customers: nil} // empty response, no error
	ua := &fakeUnifi{}
	svc := newService(Config{BreakerThreshold: 2}, s, rp, ua)

	res, err := svc.RecheckDeniedTap(context.Background(), "NFC123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Reactivated {
		t.Error("missing-in-Redpoint must not reactivate")
	}
	if !strings.Contains(res.Reason, "not found in Redpoint") {
		t.Errorf("Reason: got %q", res.Reason)
	}
	// A second call with the same empty response must still pass breaker
	// (proves we didn't wrongly count "customer not found" as a failure).
	_, _ = svc.RecheckDeniedTap(context.Background(), "NFC123")
	if svc.breaker.isOpen() {
		t.Error("breaker must not trip on missing-customer (not an upstream health signal)")
	}
}

// ─── Freshness (MaxStaleness) gate ──────────────────────────────

// TestRecheck_MaxStaleness_Zero_AlwaysRechecks is the pre-A3 default
// contract: MaxStaleness=0 means every denial pays a live query.
func TestRecheck_MaxStaleness_Zero_AlwaysRechecks(t *testing.T) {
	cachedAt := time.Now().UTC().Format(time.RFC3339)
	s := &fakeStore{member: denied(cachedAt)}
	rp := &fakeRedpoint{customers: []*redpoint.Customer{frozenCustomer()}}
	ua := &fakeUnifi{}
	svc := newService(Config{MaxStaleness: 0}, s, rp, ua)

	_, err := svc.RecheckDeniedTap(context.Background(), "NFC123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rp.callCount() != 1 {
		t.Errorf("Redpoint calls: want 1 (zero MaxStaleness = always recheck), got %d", rp.callCount())
	}
}

// TestRecheck_MaxStaleness_Fresh_SkipsRedpoint — cache is younger than
// the budget, so the denial is trusted and we do NOT hit Redpoint.
func TestRecheck_MaxStaleness_Fresh_SkipsRedpoint(t *testing.T) {
	now := time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC)
	cachedAt := now.Add(-30 * time.Minute).Format(time.RFC3339) // 30m old
	s := &fakeStore{member: denied(cachedAt)}
	rp := &fakeRedpoint{customers: []*redpoint.Customer{activeCustomer()}}
	ua := &fakeUnifi{}
	svc := newService(Config{
		MaxStaleness: 2 * time.Hour,
		Now:          func() time.Time { return now },
	}, s, rp, ua)

	res, err := svc.RecheckDeniedTap(context.Background(), "NFC123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Reactivated {
		t.Error("fresh-cache denial must not reactivate (recheck skipped)")
	}
	if !strings.Contains(res.Reason, "fresh enough") {
		t.Errorf("Reason: want 'fresh enough', got %q", res.Reason)
	}
	if rp.callCount() != 0 {
		t.Errorf("Redpoint should be skipped on fresh cache (got %d calls)", rp.callCount())
	}
}

// TestRecheck_MaxStaleness_Stale_Rechecks — cache is older than the
// budget, so we fall through to the live query just like MaxStaleness=0.
func TestRecheck_MaxStaleness_Stale_Rechecks(t *testing.T) {
	now := time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC)
	cachedAt := now.Add(-3 * time.Hour).Format(time.RFC3339) // 3h old
	s := &fakeStore{member: denied(cachedAt)}
	rp := &fakeRedpoint{customers: []*redpoint.Customer{activeCustomer()}}
	ua := &fakeUnifi{users: []unifi.UniFiUser{
		{ID: "unifi-1", NfcTokens: []string{"NFC123"}},
	}}
	svc := newService(Config{
		MaxStaleness: 2 * time.Hour,
		Now:          func() time.Time { return now },
	}, s, rp, ua)

	res, err := svc.RecheckDeniedTap(context.Background(), "NFC123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Reactivated {
		t.Error("stale cache + Redpoint=allow must reactivate")
	}
	if rp.callCount() != 1 {
		t.Errorf("Redpoint calls: want 1, got %d", rp.callCount())
	}
}

// TestRecheck_MaxStaleness_UnparseableCachedAt_FailsOpen — if the
// cached timestamp is junk, we fall through to the live query rather
// than silently trusting the denial. Better to pay an extra Redpoint
// call than to wrongly lock out a renewed member because of a corrupt
// timestamp (the alternative failure mode is much worse).
func TestRecheck_MaxStaleness_UnparseableCachedAt_FailsOpen(t *testing.T) {
	s := &fakeStore{member: denied("this-is-not-a-timestamp")}
	rp := &fakeRedpoint{customers: []*redpoint.Customer{frozenCustomer()}}
	ua := &fakeUnifi{}
	svc := newService(Config{MaxStaleness: 1 * time.Hour}, s, rp, ua)

	_, err := svc.RecheckDeniedTap(context.Background(), "NFC123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rp.callCount() != 1 {
		t.Errorf("unparseable CachedAt should fail-open to live recheck, got %d calls", rp.callCount())
	}
}

// ─── Breaker behaviour (end-to-end through Service) ─────────────

// TestRecheck_Breaker_TripsOnConsecutiveFailures — Redpoint keeps
// erroring; after `threshold` calls the breaker trips and further
// RecheckDeniedTap calls short-circuit without invoking Redpoint.
func TestRecheck_Breaker_TripsOnConsecutiveFailures(t *testing.T) {
	s := &fakeStore{member: denied("")}
	rp := &fakeRedpoint{err: errors.New("upstream 503")}
	ua := &fakeUnifi{}
	svc := newService(Config{
		BreakerThreshold: 3,
		BreakerCooldown:  100 * time.Millisecond,
	}, s, rp, ua)

	// First three calls all fail — each increments the breaker.
	for i := 0; i < 3; i++ {
		_, err := svc.RecheckDeniedTap(context.Background(), "NFC123")
		if err == nil {
			t.Fatalf("call %d: expected error from upstream failure", i)
		}
	}
	if !svc.breaker.isOpen() {
		t.Fatal("breaker should be open after threshold failures")
	}

	// Next call must short-circuit: no Redpoint invocation, no error,
	// Reason contains the breaker explanation.
	before := rp.callCount()
	res, err := svc.RecheckDeniedTap(context.Background(), "NFC123")
	if err != nil {
		t.Fatalf("breaker-open path should not surface err, got %v", err)
	}
	if rp.callCount() != before {
		t.Errorf("breaker-open must NOT invoke Redpoint (calls: %d → %d)", before, rp.callCount())
	}
	if res.Reactivated {
		t.Error("breaker-open result must not claim reactivated")
	}
	if !strings.Contains(res.Reason, "circuit breaker open") {
		t.Errorf("Reason: want 'circuit breaker open', got %q", res.Reason)
	}
}

// TestRecheck_Breaker_ResetsOnSuccess — a success in the middle of a
// failure streak clears the counter, so it takes `threshold` fresh
// failures to trip.
func TestRecheck_Breaker_ResetsOnSuccess(t *testing.T) {
	s := &fakeStore{member: denied("")}
	rp := &fakeRedpoint{err: errors.New("upstream")}
	ua := &fakeUnifi{}
	svc := newService(Config{BreakerThreshold: 3, BreakerCooldown: 100 * time.Millisecond}, s, rp, ua)

	// Two failures.
	for i := 0; i < 2; i++ {
		_, _ = svc.RecheckDeniedTap(context.Background(), "NFC123")
	}
	// Switch upstream to success, one call, reset.
	rp.mu.Lock()
	rp.err = nil
	rp.customers = []*redpoint.Customer{frozenCustomer()}
	rp.mu.Unlock()
	_, err := svc.RecheckDeniedTap(context.Background(), "NFC123")
	if err != nil {
		t.Fatalf("expected clean call after err cleared, got %v", err)
	}
	// Back to failure — the breaker must tolerate 2 more before tripping.
	rp.mu.Lock()
	rp.err = errors.New("upstream")
	rp.mu.Unlock()
	for i := 0; i < 2; i++ {
		_, _ = svc.RecheckDeniedTap(context.Background(), "NFC123")
	}
	if svc.breaker.isOpen() {
		t.Error("breaker must not be open — success reset the counter, only 2 fresh failures")
	}
}

// TestRecheck_Breaker_RecoversAfterCooldown — the breaker re-admits
// traffic after cooldown; a successful probe closes it. Uses injected
// clock on the breaker plus a time-manipulated cooldown so there's no
// real sleep.
func TestRecheck_Breaker_RecoversAfterCooldown(t *testing.T) {
	// Start a little past the epoch so subtraction doesn't underflow.
	now := time.Unix(10_000, 0)
	clock := &testClock{t: now}

	s := &fakeStore{member: denied("")}
	rp := &fakeRedpoint{err: errors.New("upstream")}
	ua := &fakeUnifi{users: []unifi.UniFiUser{
		{ID: "unifi-1", NfcTokens: []string{"NFC123"}},
	}}
	svc := newService(Config{
		BreakerThreshold: 2,
		BreakerCooldown:  50 * time.Millisecond,
	}, s, rp, ua)
	// Inject the clock on the breaker directly. Service doesn't expose
	// the breaker externally, but within-package access is fine for
	// testing and this keeps the public API unchanged.
	svc.breaker.now = clock.now

	// Trip it.
	for i := 0; i < 2; i++ {
		_, _ = svc.RecheckDeniedTap(context.Background(), "NFC123")
	}
	if !svc.breaker.isOpen() {
		t.Fatal("breaker should be open")
	}

	// Advance past cooldown + flip upstream healthy (active customer).
	clock.advance(60 * time.Millisecond)
	rp.mu.Lock()
	rp.err = nil
	rp.customers = []*redpoint.Customer{activeCustomer()}
	rp.mu.Unlock()

	res, err := svc.RecheckDeniedTap(context.Background(), "NFC123")
	if err != nil {
		t.Fatalf("post-cooldown call should succeed, got %v", err)
	}
	if !res.Reactivated {
		t.Error("post-cooldown success should reactivate")
	}
	if svc.breaker.isOpen() {
		t.Error("breaker should be closed after a successful probe")
	}
}

// ─── Shadow mode ────────────────────────────────────────────────

// TestRecheck_ShadowMode — the store is still updated and
// Reactivated=true is still returned, but the UA-Hub mutation is
// skipped. The Reason makes the shadow fact observable in logs.
func TestRecheck_ShadowMode(t *testing.T) {
	s := &fakeStore{member: denied("")}
	rp := &fakeRedpoint{customers: []*redpoint.Customer{activeCustomer()}}
	ua := &fakeUnifi{users: []unifi.UniFiUser{
		{ID: "unifi-1", NfcTokens: []string{"NFC123"}},
	}}
	svc := newService(Config{ShadowMode: true}, s, rp, ua)

	res, err := svc.RecheckDeniedTap(context.Background(), "NFC123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Reactivated {
		t.Error("shadow mode should still report reactivated=true")
	}
	if !strings.Contains(res.Reason, "shadow") {
		t.Errorf("Reason: want 'shadow', got %q", res.Reason)
	}
	if ua.updateCalls != 0 {
		t.Errorf("shadow mode must skip UA mutation (got %d UA calls)", ua.updateCalls)
	}
	if s.lastUpsert == nil {
		t.Error("shadow mode must still update store (so next tap passes locally)")
	}
}

// (Removed in PR3: TestRecheck_SetShadowMode_Runtime exercised a
// SetShadowMode setter that no longer exists. ShadowMode is now
// construction-only via Config.ShadowMode; the construction-time
// happy path is covered by other tests in this file.)

// ─── UA failure tails ───────────────────────────────────────────

// TestRecheck_UAListUsersFails_ReactivatedWithReason — Redpoint said
// "active" and we upserted the store, but listing UA users errored.
// The handler still returns Reactivated=true because the next tap
// will pass on the now-fresh cache; the Reason communicates the
// partial state.
func TestRecheck_UAListUsersFails_ReactivatedWithReason(t *testing.T) {
	s := &fakeStore{member: denied("")}
	rp := &fakeRedpoint{customers: []*redpoint.Customer{activeCustomer()}}
	ua := &fakeUnifi{listErr: errors.New("ua-hub 502")}
	svc := newService(Config{}, s, rp, ua)

	res, err := svc.RecheckDeniedTap(context.Background(), "NFC123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Reactivated {
		t.Error("Reactivated must be true even when UA list fails (cache is fresh)")
	}
	if !strings.Contains(res.Reason, "UniFi reactivation pending next sync") {
		t.Errorf("Reason: want 'UniFi reactivation pending next sync', got %q", res.Reason)
	}
	if s.lastUpsert == nil {
		t.Error("store must be updated before the UA step")
	}
}

// TestRecheck_UAUpdateFails_ReactivatedWithReason — UA list OK but the
// status write errored. Same outcome: Reactivated=true + informative
// Reason; the cache is already updated so the next tap succeeds.
func TestRecheck_UAUpdateFails_ReactivatedWithReason(t *testing.T) {
	s := &fakeStore{member: denied("")}
	rp := &fakeRedpoint{customers: []*redpoint.Customer{activeCustomer()}}
	ua := &fakeUnifi{
		users:     []unifi.UniFiUser{{ID: "unifi-1", NfcTokens: []string{"NFC123"}}},
		updateErr: errors.New("ua-hub 500"),
	}
	svc := newService(Config{}, s, rp, ua)

	res, err := svc.RecheckDeniedTap(context.Background(), "NFC123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Reactivated {
		t.Error("Reactivated must be true even when UA update fails")
	}
	if !strings.Contains(res.Reason, "UniFi update failed") {
		t.Errorf("Reason: want 'UniFi update failed', got %q", res.Reason)
	}
}

// testClock is a minimal monotonic clock for breaker recovery tests.
// Kept separate from breaker_test.go's fakeClock so edits to one don't
// risk the other. Advance() is serialised with now(), so the -race
// detector stays quiet when a test races the breaker against elapsed-
// time reads.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}
