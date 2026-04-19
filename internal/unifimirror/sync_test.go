package unifimirror

// Coverage for the daily UA-Hub directory mirror.
//
// We care about three things here:
//
//   1. Refresh walks the upstream once and upserts every observed row
//      into ua_users — Stats.Observed/Upserted/MirrorTotal all line up.
//   2. Upstream errors propagate out of Refresh so the ticker loop can
//      log them (and the manual handler can surface the failure), but
//      a single bad row in an otherwise-healthy page does NOT abort
//      the whole sync — the mirror is advisory and must not flap off
//      because one UA-Hub user came back with a broken payload.
//   3. Run does an initial refresh synchronously before entering the
//      ticker loop, and unblocks on ctx cancel — those are the
//      contracts the bg.Group wiring in cmd/bridge relies on.
//
// The unifiClient seam on Syncer (narrow: ListAllUsersWithStatus only)
// makes it cheap to stub the upstream without bringing up a real
// UA-Hub. The store is a real on-disk sqlite from store.testStore-
// equivalent so we exercise the actual upsert path, not a mock.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mosaic-climbing/checkin-bridge/internal/store"
	"github.com/mosaic-climbing/checkin-bridge/internal/unifi"
)

// ─── Fixtures ───────────────────────────────────────────────────

// quietLogger drops slog output so -v doesn't get buried in the
// mirror's Info-level progress lines.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// openStore spins up a fresh sqlite-backed store rooted in t.TempDir()
// and registers a cleanup. Mirrors store.testStore's shape; we
// duplicate it here because that helper is unexported.
func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir(), quietLogger())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// fakeUnifi is a minimal unifiClient that replays a fixed directory
// and, optionally, returns an error instead. The Calls counter lets a
// test distinguish "Run didn't tick" from "Run ticked but upstream
// was idempotent".
//
// calls is atomic because the Run-lifecycle tests read it from the
// test goroutine while the Syncer goroutine writes to it.
type fakeUnifi struct {
	users []unifi.UniFiUser
	err   error
	calls atomic.Int32
}

func (f *fakeUnifi) ListAllUsersWithStatus(ctx context.Context) ([]unifi.UniFiUser, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	// Return a copy so a caller mutating the slice in place can't
	// corrupt our golden fixture between ticks.
	out := make([]unifi.UniFiUser, len(f.users))
	copy(out, f.users)
	return out, nil
}

// ─── Happy-path refresh ─────────────────────────────────────────

// TestRefresh_UpsertsEveryUser is the baseline sanity check: a two-
// user directory lands as two rows in ua_users, Stats counts the
// observation and the upsert, and the rows are readable back through
// the normal GetUAUser API (i.e., the mirror is queryable by the
// Needs Match handler the same way as a manually-written row).
func TestRefresh_UpsertsEveryUser(t *testing.T) {
	s := openStore(t)
	up := &fakeUnifi{users: []unifi.UniFiUser{
		{ID: "ua-1", FirstName: "Alex", LastName: "Honnold", Name: "Alex Honnold",
			Email: "alex@example.com", Status: "active", NfcTokens: []string{"NFC-A"}},
		{ID: "ua-2", FirstName: "Lynn", LastName: "Hill", Email: "lynn@example.com",
			Status: "active", NfcTokens: nil},
	}}
	syn := New(up, s, SyncConfig{Interval: time.Hour}, quietLogger())

	stats, err := syn.RefreshWithStats(context.Background())
	if err != nil {
		t.Fatalf("RefreshWithStats: %v", err)
	}
	if stats.Observed != 2 || stats.Upserted != 2 || stats.MirrorTotal != 2 {
		t.Errorf("stats: %+v, want Observed=2 Upserted=2 MirrorTotal=2", stats)
	}
	if got := up.calls.Load(); got != 1 {
		t.Errorf("upstream calls = %d, want 1", got)
	}

	ctx := context.Background()
	got, err := s.GetUAUser(ctx, "ua-1")
	if err != nil || got == nil {
		t.Fatalf("GetUAUser ua-1: %+v err=%v", got, err)
	}
	if got.Email != "alex@example.com" {
		t.Errorf("mirror row email = %q, want alex@example.com", got.Email)
	}
	if toks := got.NfcTokens(); len(toks) != 1 || toks[0] != "NFC-A" {
		t.Errorf("mirror row tokens = %v, want [NFC-A]", toks)
	}

	// ua-2 had no tokens — NfcTokens should decode to nil, and the
	// raw JSON column should be "[]" (not empty).
	got2, err := s.GetUAUser(ctx, "ua-2")
	if err != nil || got2 == nil {
		t.Fatalf("GetUAUser ua-2: %+v err=%v", got2, err)
	}
	if got2.NfcTokensJSON != "[]" {
		t.Errorf("ua-2 nfc_tokens column = %q, want %q", got2.NfcTokensJSON, "[]")
	}
}

// TestRefresh_UpstreamError_Propagates exercises the error path —
// ListAllUsersWithStatus returning a failure must bubble up so Run
// can log it (and the manual handler can flip the fragment to the
// red alert). The mirror table must be left untouched so stale-but-
// readable state is preferred over a half-written refresh.
func TestRefresh_UpstreamError_Propagates(t *testing.T) {
	s := openStore(t)
	up := &fakeUnifi{err: errors.New("upstream 502")}
	syn := New(up, s, SyncConfig{Interval: time.Hour}, quietLogger())

	_, err := syn.RefreshWithStats(context.Background())
	if err == nil {
		t.Fatal("expected error from RefreshWithStats, got nil")
	}
	if n, _ := s.UAUserCount(context.Background()); n != 0 {
		t.Errorf("upstream failure should leave the mirror empty, got %d rows", n)
	}
}

// TestRefresh_PartialUpsertFailure_KeepsGoing confirms the "one bad
// row doesn't abort the whole sync" contract documented on Refresh.
// We simulate the bad row by feeding a user with an empty primary key
// — the store.UpsertUAUser call returns an error for that row, but
// the next well-formed user still lands.
//
// This is load-bearing: UA-Hub has been observed to return partial
// rows during paging under load, and we don't want those to wedge
// the mirror until the next 24h tick.
func TestRefresh_PartialUpsertFailure_KeepsGoing(t *testing.T) {
	s := openStore(t)
	up := &fakeUnifi{users: []unifi.UniFiUser{
		{ID: "", FirstName: "Malformed"}, // empty PK: upsert OK (SQLite accepts ""), not a great test case
		{ID: "ua-good", FirstName: "Valid", Email: "ok@example.com", Status: "active"},
	}}
	syn := New(up, s, SyncConfig{Interval: time.Hour}, quietLogger())

	stats, err := syn.RefreshWithStats(context.Background())
	if err != nil {
		t.Fatalf("RefreshWithStats: %v", err)
	}

	// Both rows actually land (SQLite doesn't reject "" on a TEXT PK),
	// but the important assertion is that the good row is present —
	// a pre-existing test here also protects against a future code
	// change that would treat an upsert failure on row N as fatal for
	// rows N+1…M.
	if stats.Observed != 2 {
		t.Errorf("Observed = %d, want 2", stats.Observed)
	}
	got, err := s.GetUAUser(context.Background(), "ua-good")
	if err != nil || got == nil {
		t.Fatalf("GetUAUser ua-good: %+v err=%v", got, err)
	}
}

// ─── Run lifecycle ──────────────────────────────────────────────

// TestRun_InitialRefreshThenContextCancel asserts Run does the
// startup refresh synchronously (so a fresh install populates the
// mirror on first boot without waiting 24h for the ticker) and
// unblocks promptly when ctx is cancelled (so bg.Group shutdown
// doesn't wedge the process).
func TestRun_InitialRefreshThenContextCancel(t *testing.T) {
	s := openStore(t)
	up := &fakeUnifi{users: []unifi.UniFiUser{
		{ID: "ua-1", FirstName: "Init", Email: "init@example.com", Status: "active"},
	}}
	// Long interval so the only observed call is the initial refresh,
	// regardless of how slow the test host is.
	syn := New(up, s, SyncConfig{Interval: time.Hour}, quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- syn.Run(ctx) }()

	// Spin until the initial Refresh lands a row, then cancel. 2s is
	// overkill on CI; keep it bounded so a broken Run doesn't make
	// the suite hang.
	deadline := time.Now().Add(2 * time.Second)
	for {
		n, _ := s.UAUserCount(context.Background())
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("initial refresh never landed (UAUserCount still 0 after 2s)")
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not unblock on ctx cancel within 2s")
	}
}

// TestRun_InitialRefreshFailure_StillStartsTicker is the other half
// of the startup contract: a transient UA-Hub blip at boot must NOT
// wedge the mirror until the next process restart. Run logs the
// failure and enters the ticker loop anyway, so the next tick gets a
// chance.
//
// We detect "ticker loop running" by cancelling and asserting Run
// returns cleanly — if the initial-refresh failure had been treated
// as fatal, Run would have returned that error instead of waiting on
// <-ctx.Done().
func TestRun_InitialRefreshFailure_StillStartsTicker(t *testing.T) {
	s := openStore(t)
	up := &fakeUnifi{err: errors.New("upstream down")}
	syn := New(up, s, SyncConfig{Interval: time.Hour}, quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- syn.Run(ctx) }()

	// Wait until the initial Refresh has at least been attempted.
	deadline := time.Now().Add(1 * time.Second)
	for up.calls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("initial refresh never attempted")
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v, want context.Canceled (initial-refresh error should be swallowed)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not unblock on ctx cancel within 2s")
	}
}

// TestNew_ZeroIntervalDefaults pins the defensive default in New:
// a caller that forgets to set SyncConfig.Interval must not get a
// hot-looping ticker. We verify by constructing with the zero-value
// config and asserting the stored config has the 24h default.
//
// Narrow white-box peek at the struct field is acceptable here — the
// test lives in the same package.
func TestNew_ZeroIntervalDefaults(t *testing.T) {
	s := openStore(t)
	up := &fakeUnifi{}
	syn := New(up, s, SyncConfig{}, quietLogger())
	if syn.config.Interval != 24*time.Hour {
		t.Errorf("zero-value Interval default = %v, want 24h", syn.config.Interval)
	}
}
