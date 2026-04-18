package statusync

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/mosaic-climbing/checkin-bridge/internal/metrics"
	"github.com/mosaic-climbing/checkin-bridge/internal/redpoint"
	"github.com/mosaic-climbing/checkin-bridge/internal/store"
	"github.com/mosaic-climbing/checkin-bridge/internal/testutil"
	"github.com/mosaic-climbing/checkin-bridge/internal/unifi"
)

// TestScheduleNext_WallClock covers the wall-clock scheduling path
// (Config.SyncTimeLocal set). The cases here are the ones that broke first
// drafts of this code: choosing today vs tomorrow on either side of the
// target time, the exact-equality boundary, and the minLead bump.
func TestScheduleNext_WallClock(t *testing.T) {
	loc := time.FixedZone("test", 0)
	mkSyncer := func(hhmm string) *Syncer {
		return &Syncer{
			config: Config{SyncTimeLocal: hhmm, SyncInterval: 24 * time.Hour},
			logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
	}

	cases := []struct {
		name    string
		hhmm    string
		now     time.Time
		minLead time.Duration
		want    time.Time
	}{
		{
			name: "now before today's target → schedule today",
			hhmm: "03:00",
			now:  time.Date(2026, 4, 16, 1, 30, 0, 0, loc),
			want: time.Date(2026, 4, 16, 3, 0, 0, 0, loc),
		},
		{
			name: "now after today's target → schedule tomorrow",
			hhmm: "03:00",
			now:  time.Date(2026, 4, 16, 14, 30, 0, 0, loc),
			want: time.Date(2026, 4, 17, 3, 0, 0, 0, loc),
		},
		{
			name: "now exactly equals today's target → schedule tomorrow (avoids re-firing same moment)",
			hhmm: "03:00",
			now:  time.Date(2026, 4, 16, 3, 0, 0, 0, loc),
			want: time.Date(2026, 4, 17, 3, 0, 0, 0, loc),
		},
		{
			name:    "today's target inside minLead window → bump to tomorrow",
			hhmm:    "03:00",
			now:     time.Date(2026, 4, 16, 2, 59, 30, 0, loc),
			minLead: 2 * time.Minute,
			want:    time.Date(2026, 4, 17, 3, 0, 0, 0, loc),
		},
		{
			name:    "today's target outside minLead window → schedule today",
			hhmm:    "03:00",
			now:     time.Date(2026, 4, 16, 2, 50, 0, 0, loc),
			minLead: 2 * time.Minute,
			want:    time.Date(2026, 4, 16, 3, 0, 0, 0, loc),
		},
		{
			// Crossing midnight: now is 23:59, target is 03:00 → schedule
			// tomorrow's 03:00, not "today's" 03:00 in the past.
			name: "crossing midnight",
			hhmm: "03:00",
			now:  time.Date(2026, 4, 16, 23, 59, 0, 0, loc),
			want: time.Date(2026, 4, 17, 3, 0, 0, 0, loc),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := mkSyncer(tc.hhmm)
			got := s.scheduleNext(tc.now, tc.minLead)
			if !got.Equal(tc.want) {
				t.Errorf("scheduleNext(%s, minLead=%s) = %s, want %s",
					tc.now.Format(time.RFC3339), tc.minLead,
					got.Format(time.RFC3339), tc.want.Format(time.RFC3339))
			}
		})
	}
}

// TestScheduleNext_Interval covers the legacy interval-ticker path
// (SyncTimeLocal empty). The minLead floor must clamp first runs that
// would otherwise fire before the cache had time to populate.
func TestScheduleNext_Interval(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	now := time.Date(2026, 4, 16, 12, 0, 0, 0, time.UTC)

	t.Run("interval larger than minLead → use interval", func(t *testing.T) {
		s := &Syncer{
			config: Config{SyncInterval: 1 * time.Hour},
			logger: logger,
		}
		got := s.scheduleNext(now, 2*time.Minute)
		want := now.Add(1 * time.Hour)
		if !got.Equal(want) {
			t.Errorf("got %s, want %s", got, want)
		}
	})

	t.Run("interval smaller than minLead → clamp to minLead floor", func(t *testing.T) {
		s := &Syncer{
			config: Config{SyncInterval: 30 * time.Second},
			logger: logger,
		}
		got := s.scheduleNext(now, 2*time.Minute)
		want := now.Add(2 * time.Minute)
		if !got.Equal(want) {
			t.Errorf("got %s, want %s", got, want)
		}
	})

	t.Run("zero minLead → use bare interval (steady-state path)", func(t *testing.T) {
		s := &Syncer{
			config: Config{SyncInterval: 24 * time.Hour},
			logger: logger,
		}
		got := s.scheduleNext(now, 0)
		want := now.Add(24 * time.Hour)
		if !got.Equal(want) {
			t.Errorf("got %s, want %s", got, want)
		}
	})
}

// TestScheduleNext_MalformedFallsBackToInterval ensures a SyncTimeLocal that
// somehow escapes config validation (e.g. a future code path that mutates it
// at runtime) doesn't crash the loop — it logs and falls back to the
// interval ticker.
func TestScheduleNext_MalformedFallsBackToInterval(t *testing.T) {
	s := &Syncer{
		config: Config{
			SyncTimeLocal: "not-a-time",
			SyncInterval:  6 * time.Hour,
		},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	now := time.Date(2026, 4, 16, 10, 0, 0, 0, time.UTC)
	got := s.scheduleNext(now, 0)
	want := now.Add(6 * time.Hour)
	if !got.Equal(want) {
		t.Errorf("got %s, want %s (interval fallback)", got, want)
	}
}

// TestSleepUntil_RespectsContext makes the cancellation contract explicit:
// a cancelled context must return false promptly, not deadlock.
func TestSleepUntil_RespectsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan bool, 1)
	go func() {
		// Far-future target so timer.C won't fire.
		done <- sleepUntil(ctx, time.Now().Add(time.Hour))
	}()

	cancel()

	select {
	case ok := <-done:
		if ok {
			t.Error("sleepUntil returned true after cancellation; want false")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sleepUntil did not return after cancellation within 2s")
	}
}

// TestSleepUntil_PastTargetReturnsImmediately ensures we don't deadlock on
// a target time that's already in the past — useful when scheduleNext's
// minLead clamp is disabled and the interval falls inside the loop's own
// scheduling overhead.
func TestSleepUntil_PastTargetReturnsImmediately(t *testing.T) {
	done := make(chan bool, 1)
	go func() {
		done <- sleepUntil(context.Background(), time.Now().Add(-1*time.Hour))
	}()
	select {
	case ok := <-done:
		if !ok {
			t.Error("expected immediate true return on past target")
		}
	case <-time.After(time.Second):
		t.Fatal("sleepUntil deadlocked on past target")
	}
}

// TestShadowMode_NoUniFiWrites verifies that when shadow mode is on,
// RunSync logs every activation/deactivation but never calls
// UpdateUserStatus on the UniFi API.
//
// Scenario: one UniFi user is ACTIVE with an NFC token, and the local store
// shows that member as frozen. A live sync would deactivate them; shadow
// mode must leave UniFi alone.
func TestShadowMode_NoUniFiWrites(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	fakeUnifi := testutil.NewFakeUniFi()
	defer fakeUnifi.Close()

	// Seed fake UniFi with one active user holding a known NFC token.
	fakeUnifi.Users = []map[string]any{
		{
			"id":         "unifi-user-1",
			"first_name": "Frozen",
			"last_name":  "Member",
			"status":     "ACTIVE",
			"credentials": []any{
				map[string]any{
					"type":  "nfc",
					"token": "TOKEN-FROZEN",
				},
			},
		},
	}

	fakeRedpoint := testutil.NewFakeRedpoint()
	defer fakeRedpoint.Close()

	unifiClient := unifi.NewClient(
		"wss://unused",
		fakeUnifi.BaseURL(),
		"test-token",
		500,
		"",
		logger,
	)
	rpClient := redpoint.NewClient(
		fakeRedpoint.GraphQLURL(),
		"test-api-key",
		"TEST",
		logger,
	)

	dir := t.TempDir()
	db, err := store.Open(dir, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Store says the member is frozen → live sync would DEACTIVATE.
	if err := db.UpsertMember(context.Background(), &store.Member{
		NfcUID:      "TOKEN-FROZEN",
		CustomerID:  "rp-cust-1",
		FirstName:   "Frozen",
		LastName:    "Member",
		BadgeStatus: "FROZEN",
		Active:      true, // active in Redpoint, but badge frozen → not allowed
		CachedAt:    time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}

	syncer := New(unifiClient, rpClient, db, Config{
		SyncInterval:   time.Hour,
		RateLimitDelay: time.Millisecond,
	}, logger)
	syncer.SetShadowMode(true)

	result, err := syncer.RunSync(context.Background())
	if err != nil {
		t.Fatalf("RunSync: %v", err)
	}

	// Accounting: the decision counter increments so operators see what
	// the live syncer WOULD do, but no actual UniFi write occurred.
	if result.Matched != 1 {
		t.Errorf("Matched = %d, want 1", result.Matched)
	}
	if result.Deactivated != 1 {
		t.Errorf("Deactivated (logical) = %d, want 1", result.Deactivated)
	}
	if result.Errors != 0 {
		t.Errorf("Errors = %d, want 0", result.Errors)
	}

	// The contract: zero PUT /users/:id calls reached UniFi.
	if got := fakeUnifi.StatusUpdateCount(); got != 0 {
		t.Errorf("shadow mode sent %d UniFi status update(s); want 0", got)
	}
}

// TestRunSync_StampsLivenessGauge verifies the C3 liveness signal: when
// RunSync completes successfully, the last_sync_completed_at gauge is
// set to the current unix timestamp and sync_runs_total increments. The
// alert contract is that operators page on `time() - gauge > 2 * interval`,
// so the gauge must be non-zero after a run and must reflect "now".
func TestRunSync_StampsLivenessGauge(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	fakeUnifi := testutil.NewFakeUniFi()
	defer fakeUnifi.Close()
	// Empty user list → RunSync walks matching/legacy/expiry paths and
	// exits cleanly. That's the simplest successful path, which is what
	// we need to exercise the gauge emit at the bottom of RunSync.
	fakeUnifi.Users = []map[string]any{}

	fakeRedpoint := testutil.NewFakeRedpoint()
	defer fakeRedpoint.Close()

	unifiClient := unifi.NewClient(
		"wss://unused",
		fakeUnifi.BaseURL(),
		"test-token",
		500,
		"",
		logger,
	)
	rpClient := redpoint.NewClient(
		fakeRedpoint.GraphQLURL(),
		"test-api-key",
		"TEST",
		logger,
	)

	dir := t.TempDir()
	db, err := store.Open(dir, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	syncer := New(unifiClient, rpClient, db, Config{
		SyncInterval:   time.Hour,
		RateLimitDelay: time.Millisecond,
	}, logger)
	reg := metrics.New()
	syncer.SetMetrics(reg)

	before := time.Now().Unix()
	if _, err := syncer.RunSync(context.Background()); err != nil {
		t.Fatalf("RunSync: %v", err)
	}
	after := time.Now().Unix()

	gauge := reg.Gauge("last_sync_completed_at").Value()
	if gauge == 0 {
		t.Fatalf("last_sync_completed_at = 0; expected non-zero unix timestamp")
	}
	gaugeI := int64(gauge)
	if gaugeI < before || gaugeI > after {
		t.Errorf("last_sync_completed_at = %d; want within [%d, %d]", gaugeI, before, after)
	}
	if got := reg.Counter("sync_runs_total").Value(); got != 1 {
		t.Errorf("sync_runs_total = %d; want 1", got)
	}
	// Sanity: the supervisor counter should NOT have incremented on a
	// clean run — it's only for panic-driven restarts.
	if got := reg.Counter("sync_loop_restarted_total").Value(); got != 0 {
		t.Errorf("sync_loop_restarted_total = %d; want 0 (no panic)", got)
	}
}

// TestRunWithRecover_CatchesPanic verifies that a panic inside the fn
// argument is caught, converted to crashed=true, and does not propagate
// to the caller. This is the atomic recover contract; the supervisor
// relies on it for the "panic → bump counter → re-launch" flow.
func TestRunWithRecover_CatchesPanic(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &Syncer{logger: logger}

	crashed := s.runWithRecover(context.Background(), func(context.Context) {
		panic("boom from inside runLoop")
	})
	if !crashed {
		t.Fatal("runWithRecover: crashed=false after panic; want true")
	}
}

// TestRunWithRecover_CleanReturn verifies the non-panic path: when fn
// returns normally, crashed must be false. This is the "don't
// false-positive" side of the recover contract.
func TestRunWithRecover_CleanReturn(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &Syncer{logger: logger}

	crashed := s.runWithRecover(context.Background(), func(context.Context) {
		// intentionally empty
	})
	if crashed {
		t.Error("runWithRecover: crashed=true on clean return; want false")
	}
}

// TestSupervisedLoop_RestartsOnPanic drives the full supervisor-wrapper
// end-to-end: a fn that panics on the first invocation and returns
// normally on the second, with a ctx that's cancelled after the second
// call. Expectations: restart counter == 1 (one restart happened), and
// supervisedLoop returns without hanging.
func TestSupervisedLoop_RestartsOnPanic(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := metrics.New()
	s := &Syncer{
		logger:  logger,
		metrics: reg,
		config:  Config{SyncInterval: 24 * time.Hour},
	}

	calls := 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fn := func(context.Context) {
		calls++
		if calls == 1 {
			panic("simulated sync loop panic")
		}
		// Second call returns normally; we cancel the ctx from inside
		// so supervisedLoopWithFn exits the outer for loop on next
		// iteration. Without cancel, supervisedLoopWithFn would keep
		// re-launching forever (the "re-launch on clean return" branch).
		cancel()
	}

	done := make(chan struct{})
	go func() {
		s.supervisedLoopWithFn(ctx, fn)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisedLoopWithFn did not return within 2s; likely stuck in restart loop")
	}

	if calls != 2 {
		t.Errorf("fn called %d times; want 2 (panic then clean)", calls)
	}
	// The counter should have bumped at least once (after the panic) and
	// at most twice (supervisor also increments after clean-return path
	// by design, since that branch is "shouldn't happen in real code").
	got := reg.Counter("sync_loop_restarted_total").Value()
	if got < 1 || got > 2 {
		t.Errorf("sync_loop_restarted_total = %d; want 1 or 2", got)
	}
}
