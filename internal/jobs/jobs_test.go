package jobs

import (
	"context"
	"errors"
	"io"
	"log/slog"
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

// TestLoopWithInterval_RunsInitialPassAndTicks pins the contract:
// fn fires once on entry (so a fresh boot doesn't have to wait a
// full interval for the first run), and again on every ticker fire.
// We pick a tiny interval and watch the call count cross 2, then
// cancel — the third tick may or may not land inside the window
// depending on scheduling, so the assertion is "≥ 2 calls", not
// equality.
func TestLoopWithInterval_RunsInitialPassAndTicks(t *testing.T) {
	db := openTestStore(t)
	logger := discardLogger()

	var calls atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	err := LoopWithInterval(ctx, 50*time.Millisecond, db, logger, TypeDirectorySync,
		func(ctx context.Context) (any, error) {
			calls.Add(1)
			return map[string]any{"observed": calls.Load()}, nil
		})
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("LoopWithInterval err = %v, want ctx done", err)
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

// TestLoopWithInterval_LogsAndContinuesOnError asserts the loop
// doesn't bail out when fn returns an error. A transient upstream
// failure on one tick should be logged and ticked past, not left
// to wedge the scheduler until the bridge restarts.
func TestLoopWithInterval_LogsAndContinuesOnError(t *testing.T) {
	db := openTestStore(t)
	logger := discardLogger()

	var calls atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_ = LoopWithInterval(ctx, 50*time.Millisecond, db, logger, TypeUniFiIngest,
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
