package jobs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mosaic-climbing/checkin-bridge/internal/store"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(dir, discardLogger())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestTrack_Success is the regression test for the original bug:
// scheduled syncer goroutines were running but writing nothing to the
// jobs table, so the /ui/sync page's "Last run" pill silently dropped
// every scheduled run. Track must produce a row that LastJobByType
// can find with status=completed.
func TestTrack_Success(t *testing.T) {
	db := openTestStore(t)
	logger := discardLogger()

	wantResult := map[string]any{"observed": 42}
	err := Track(context.Background(), db, logger, TypeCacheSync,
		func(ctx context.Context) (any, error) {
			return wantResult, nil
		})
	if err != nil {
		t.Fatalf("Track returned error: %v", err)
	}

	row, err := db.LastJobByType(context.Background(), TypeCacheSync)
	if err != nil {
		t.Fatalf("LastJobByType: %v", err)
	}
	if row == nil {
		t.Fatal("no row written; the scheduled-runs bug is back")
	}
	if row.Status != "completed" {
		t.Errorf("row.Status = %q, want completed", row.Status)
	}
	if row.Type != TypeCacheSync {
		t.Errorf("row.Type = %q, want %q", row.Type, TypeCacheSync)
	}
}

// TestTrack_Failure pins the failure branch — fn's error must land in
// jobs.error and the row must transition to 'failed'. Errors returned
// by fn pass through to the caller verbatim so callers can still log
// or react.
func TestTrack_Failure(t *testing.T) {
	db := openTestStore(t)
	logger := discardLogger()

	sentinel := errors.New("upstream timeout")
	err := Track(context.Background(), db, logger, TypeStatusSync,
		func(ctx context.Context) (any, error) {
			return nil, sentinel
		})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Track err = %v, want %v", err, sentinel)
	}

	row, err := db.LastJobByType(context.Background(), TypeStatusSync)
	if err != nil {
		t.Fatalf("LastJobByType: %v", err)
	}
	if row == nil {
		t.Fatal("no row written for failed run")
	}
	if row.Status != "failed" {
		t.Errorf("row.Status = %q, want failed", row.Status)
	}
	if row.Error != sentinel.Error() {
		t.Errorf("row.Error = %q, want %q", row.Error, sentinel.Error())
	}
}

// TestTrack_DetachesCancelledContext mirrors the api package's
// finishSyncJob detach test (sync_ux_lifecycle_test.go). A cancelled
// parent ctx must not strand the row in 'running' — scheduled
// syncers are routinely cancelled mid-flight on shutdown, and the
// last successful tick before that should still close out cleanly.
func TestTrack_DetachesCancelledContext(t *testing.T) {
	db := openTestStore(t)
	logger := discardLogger()

	// Pre-cancel the ctx that gets handed to fn. fn ignores it and
	// returns success; Track must still write the 'completed' state.
	deadCtx, cancel := context.WithCancel(context.Background())
	cancel()

	err := Track(deadCtx, db, logger, TypeUAHubSync,
		func(ctx context.Context) (any, error) {
			return map[string]any{"observed": 1}, nil
		})
	if err != nil {
		t.Fatalf("Track returned error: %v", err)
	}

	row, err := db.LastJobByType(context.Background(), TypeUAHubSync)
	if err != nil {
		t.Fatalf("LastJobByType: %v", err)
	}
	if row == nil {
		t.Fatal("no row written")
	}
	if row.Status != "completed" {
		t.Errorf("row.Status = %q, want completed (Finish detach failed?)", row.Status)
	}
}

// TestLoop_RunsInitialPassAndTicks pins the basic contract: fn fires
// once on entry (so a fresh boot doesn't have to wait a full Interval
// for the first run), and again on every tick. Tiny Interval, small
// ctx window, no jitter or backoff so the scheduling is deterministic.
func TestLoop_RunsInitialPassAndTicks(t *testing.T) {
	db := openTestStore(t)
	logger := discardLogger()

	var calls atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	err := Loop(ctx, LoopConfig{Interval: 50 * time.Millisecond}, db, logger, TypeDirectorySync,
		func(ctx context.Context) (any, error) {
			calls.Add(1)
			return map[string]any{"observed": calls.Load()}, nil
		})
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("Loop err = %v, want ctx done", err)
	}
	if got := calls.Load(); got < 2 {
		t.Errorf("calls = %d, want ≥ 2 (initial + at least one tick)", got)
	}

	// Every call should have written a job row; the most recent must
	// be 'completed' since fn returned nil error.
	row, err := db.LastJobByType(context.Background(), TypeDirectorySync)
	if err != nil {
		t.Fatalf("LastJobByType: %v", err)
	}
	if row == nil || row.Status != "completed" {
		t.Errorf("most recent row = %+v, want status=completed", row)
	}
}

// TestLoop_LogsAndContinuesOnError asserts the loop doesn't bail out
// when fn returns an error. With backoff disabled (BackoffStart=0)
// failures retry on the regular Interval cadence, matching the
// pre-hardening behaviour and keeping this test fast.
func TestLoop_LogsAndContinuesOnError(t *testing.T) {
	db := openTestStore(t)
	logger := discardLogger()

	var calls atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_ = Loop(ctx, LoopConfig{Interval: 50 * time.Millisecond}, db, logger, TypeUniFiIngest,
		func(ctx context.Context) (any, error) {
			calls.Add(1)
			return nil, errors.New("upstream blip")
		})

	if got := calls.Load(); got < 2 {
		t.Errorf("calls = %d, want ≥ 2 even when fn errors", got)
	}
	row, err := db.LastJobByType(context.Background(), TypeUniFiIngest)
	if err != nil {
		t.Fatalf("LastJobByType: %v", err)
	}
	if row == nil || row.Status != "failed" {
		t.Errorf("most recent row = %+v, want status=failed", row)
	}
}

// TestLoop_InitialDelayDefersFirstRun exercises the stagger knob —
// no fire should happen before InitialDelay elapses. The test uses a
// small but generous-enough delay that scheduling overhead doesn't
// false-trigger an early call assertion.
func TestLoop_InitialDelayDefersFirstRun(t *testing.T) {
	db := openTestStore(t)
	logger := discardLogger()

	var calls atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	cfg := LoopConfig{
		Interval:     5 * time.Second, // big enough that the second tick won't land
		InitialDelay: 80 * time.Millisecond,
	}

	start := time.Now()
	_ = Loop(ctx, cfg, db, logger, TypeStatusSync,
		func(ctx context.Context) (any, error) {
			// Record when the first call lands, relative to start.
			calls.Add(1)
			return nil, nil
		})

	if got := calls.Load(); got != 1 {
		t.Fatalf("calls = %d, want exactly 1 (initial fire after InitialDelay, second tick beyond ctx)", got)
	}
	// The fire should have happened at-or-after InitialDelay. Allow a
	// small under-budget for scheduling slack — atomic.Add lands a
	// bit before time.Since reads.
	if elapsed := time.Since(start); elapsed < 60*time.Millisecond {
		t.Errorf("fired after %s, want ≥ ~80ms (InitialDelay)", elapsed)
	}
}

// TestLoop_BackoffGrowsOnConsecutiveFailures verifies that a
// failing fn produces increasing waits (start → 2× → cap), and that
// success resets the wait back to Interval. The test instruments the
// observed wait between fn calls and asserts the second wait is
// strictly larger than Interval (i.e. backoff kicked in).
func TestLoop_BackoffGrowsOnConsecutiveFailures(t *testing.T) {
	db := openTestStore(t)
	logger := discardLogger()

	// Pre-allocate a generous slice for call timestamps. A small
	// buffered channel here would block fn once the test had
	// collected enough samples, hanging the loop until ctx expires.
	const maxRecords = 64
	var (
		mu    sync.Mutex
		times []time.Time
	)
	record := func() {
		mu.Lock()
		defer mu.Unlock()
		if len(times) < maxRecords {
			times = append(times, time.Now())
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	cfg := LoopConfig{
		Interval:     20 * time.Millisecond,
		BackoffStart: 80 * time.Millisecond,
		BackoffMax:   200 * time.Millisecond,
	}

	_ = Loop(ctx, cfg, db, logger, TypeCacheSync,
		func(ctx context.Context) (any, error) {
			record()
			return nil, errors.New("simulated failure")
		})

	mu.Lock()
	defer mu.Unlock()
	if len(times) < 2 {
		t.Fatalf("only %d calls observed; need ≥ 2 to measure backoff", len(times))
	}
	// Wait between call 1 and call 2 should be ≥ BackoffStart, give
	// or take a small scheduling slack (no jitter is configured).
	gap := times[1].Sub(times[0])
	if gap < 60*time.Millisecond {
		t.Errorf("first failure → second call gap = %s; want ≥ ~80ms (BackoffStart)", gap)
	}
}

// TestLoop_BackoffResetsOnSuccess verifies that after a recovering
// success the next wait is the regular Interval, not BackoffStart.
// fn fails twice (advancing backoff) then succeeds. The wait after
// the first success should be back to ~Interval.
func TestLoop_BackoffResetsOnSuccess(t *testing.T) {
	db := openTestStore(t)
	logger := discardLogger()

	const maxGaps = 64
	var (
		mu       sync.Mutex
		observed []time.Duration
	)
	last := time.Now()
	noteGap := func() {
		mu.Lock()
		defer mu.Unlock()
		now := time.Now()
		if len(observed) < maxGaps {
			observed = append(observed, now.Sub(last))
		}
		last = now
	}

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	cfg := LoopConfig{
		Interval:     20 * time.Millisecond,
		BackoffStart: 60 * time.Millisecond,
		BackoffMax:   200 * time.Millisecond,
	}

	var calls atomic.Int32
	_ = Loop(ctx, cfg, db, logger, TypeStatusSync,
		func(ctx context.Context) (any, error) {
			n := calls.Add(1)
			noteGap()
			// First two fail, rest succeed.
			if n <= 2 {
				return nil, errors.New("transient")
			}
			return nil, nil
		})

	mu.Lock()
	defer mu.Unlock()
	if len(observed) < 4 {
		t.Fatalf("only %d calls observed; need ≥ 4 to see backoff-then-recovery", len(observed))
	}
	// observed[3] is the wait after the first success — should be
	// back to roughly Interval, not the post-backoff (~120ms) value.
	if observed[3] > 50*time.Millisecond {
		t.Errorf("post-recovery wait = %s; want ≈ Interval (20ms)", observed[3])
	}
}

// TestTrack_NilStore guards the no-op path. A scheduler constructed
// without a store (e.g. a unit test that doesn't care about job
// tracking) must not crash; the wrapped fn still runs and its
// error/result still propagates.
func TestTrack_NilStore(t *testing.T) {
	logger := discardLogger()
	called := false
	err := Track(context.Background(), nil, logger, TypeCacheSync,
		func(ctx context.Context) (any, error) {
			called = true
			return nil, nil
		})
	if err != nil {
		t.Fatalf("Track returned error: %v", err)
	}
	if !called {
		t.Fatal("fn was not invoked when store was nil")
	}
}

// TestNextBackoff covers the math in isolation: zero start disables,
// otherwise start → 2× → cap-at-max.
func TestNextBackoff(t *testing.T) {
	cases := []struct {
		name              string
		current, start, max time.Duration
		want              time.Duration
	}{
		{"disabled when start is 0", 30 * time.Second, 0, 5 * time.Minute, 0},
		{"first failure jumps to start", 0, 5 * time.Second, 5 * time.Minute, 5 * time.Second},
		{"second failure doubles", 5 * time.Second, 5 * time.Second, 5 * time.Minute, 10 * time.Second},
		{"capped at max", 4 * time.Minute, 5 * time.Second, 5 * time.Minute, 5 * time.Minute},
		{"unbounded when max is 0", 4 * time.Minute, 5 * time.Second, 0, 8 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nextBackoff(tc.current, tc.start, tc.max)
			if got != tc.want {
				t.Errorf("nextBackoff(%s, %s, %s) = %s, want %s",
					tc.current, tc.start, tc.max, got, tc.want)
			}
		})
	}
}
