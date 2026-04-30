package jobs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

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
