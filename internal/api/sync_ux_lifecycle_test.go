package api

// v0.5.7.1 sync lifecycle coverage. Two bugs converged on the staff
// /ui/sync pill leaving its sync card stuck on "⟳ Running" forever:
//
//   1. finishSyncJob inherited r.Context() from the HTTP request,
//      and HTMX/the browser routinely abandons that request after a
//      few minutes — long before a UA-Hub mirror refresh actually
//      completes (~4–5min at LEF). When the request ctx was
//      cancelled, the SQLite UPDATE in CompleteJob/FailJob silently
//      no-op'd on a Done() context and the running row was never
//      transitioned to its terminal state.
//
//   2. There was no operator escape hatch. Even after #1 is patched,
//      a daemon restart mid-refresh can leave a stale running row
//      around; staff had to ssh into the bridge and run sqlite to
//      clear it.
//
// finishSyncJob now detaches the parent ctx with WithoutCancel and
// bounds the write with a 5s deadline; handleSyncUnstick lets staff
// fail a stuck row with one click. The tests below pin both fixes.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestFinishSyncJob_DetachesCancelledContext is the regression test
// for the cancelled-r.Context bug. Pass a ctx that's already
// Done(), call finishSyncJob, then read the row back through a
// fresh ctx and assert it advanced past 'running'. Pre-fix this
// test failed because CompleteJob inherited the cancellation and
// the SQLite UPDATE returned context.Canceled before touching
// the row.
func TestFinishSyncJob_DetachesCancelledContext(t *testing.T) {
	srv, db, _ := setupTestServer(t)

	// Seed a running job. Use the real startSyncJob path so the
	// id format and column defaults match what production writes.
	id := srv.startSyncJob(context.Background(), jobTypeUAHubSync)

	// Construct an already-cancelled ctx, mirroring what r.Context()
	// looks like after HTMX abandons the long POST.
	deadCtx, cancel := context.WithCancel(context.Background())
	cancel()

	srv.finishSyncJob(deadCtx, id, map[string]any{"observed": 7}, nil)

	// Read through a fresh ctx. Pre-fix: row.Status remained
	// "running" because CompleteJob's UPDATE saw a Done() ctx.
	row, err := db.GetJob(context.Background(), id)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if row == nil {
		t.Fatalf("job %s vanished", id)
	}
	if row.Status != "completed" {
		t.Errorf("status = %q, want completed (cancelled-ctx detach failed?)", row.Status)
	}
}

// TestFinishSyncJob_CancelledContextFailureBranch covers the failure
// mirror of the same bug — a refresh that errored out should still
// land in 'failed' even when the request ctx is dead.
func TestFinishSyncJob_CancelledContextFailureBranch(t *testing.T) {
	srv, db, _ := setupTestServer(t)
	id := srv.startSyncJob(context.Background(), jobTypeUAHubSync)

	deadCtx, cancel := context.WithCancel(context.Background())
	cancel()

	srv.finishSyncJob(deadCtx, id, nil, errFailFixture("upstream timeout"))

	row, err := db.GetJob(context.Background(), id)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if row == nil {
		t.Fatalf("job %s vanished", id)
	}
	if row.Status != "failed" {
		t.Errorf("status = %q, want failed", row.Status)
	}
	if !strings.Contains(row.Error, "upstream timeout") {
		t.Errorf("error = %q, want it to contain 'upstream timeout'", row.Error)
	}
}

// TestHandleSyncUnstick_RunningRowFailsAndRendersPill is the
// happy-path test for the operator escape hatch: with a row in
// 'running', POST /ui/sync/unstick/{type} flips it to 'failed'
// with the documented reason and returns a fragment whose body
// shows the failed badge.
func TestHandleSyncUnstick_RunningRowFailsAndRendersPill(t *testing.T) {
	srv, db, _ := setupTestServer(t)
	id := srv.startSyncJob(context.Background(), jobTypeUAHubSync)

	req := httptest.NewRequest(http.MethodPost,
		"/ui/sync/unstick/"+jobTypeUAHubSync, nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	body := w.Body.String()
	if !strings.Contains(body, "Failed") {
		t.Errorf("body = %q, want failed badge", body)
	}

	row, err := db.GetJob(context.Background(), id)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if row.Status != "failed" {
		t.Errorf("status = %q, want failed (unstick handler did not flip row)", row.Status)
	}
	if !strings.Contains(row.Error, "manually cleared") {
		t.Errorf("error = %q, want it to mention 'manually cleared'", row.Error)
	}
}

// TestHandleSyncUnstick_NoRunningRowIsIdempotent: clicking Clear
// stuck on a card whose latest row is already completed (race vs.
// the daemon transitioning the row first) must not mutate state and
// must render whatever the latest pill state is.
func TestHandleSyncUnstick_NoRunningRowIsIdempotent(t *testing.T) {
	srv, db, _ := setupTestServer(t)

	// Latest job is completed, not running.
	id := srv.startSyncJob(context.Background(), jobTypeUAHubSync)
	srv.finishSyncJob(context.Background(), id, map[string]any{"observed": 1}, nil)

	req := httptest.NewRequest(http.MethodPost,
		"/ui/sync/unstick/"+jobTypeUAHubSync, nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	row, err := db.GetJob(context.Background(), id)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if row.Status != "completed" {
		t.Errorf("status = %q, want completed (unstick clobbered a finished row)", row.Status)
	}
}

// TestHandleSyncUnstick_UnknownTypeIsHarmless covers the path
// guard. /ui/sync/unstick/bogus must not surface a 5xx and must not
// touch the store.
func TestHandleSyncUnstick_UnknownTypeIsHarmless(t *testing.T) {
	srv, _, _ := setupTestServer(t)

	req := httptest.NewRequest(http.MethodPost,
		"/ui/sync/unstick/bogus_type", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (defensive renderer)", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Never run") {
		t.Errorf("body = %q, want 'Never run' pill for unknown type", body)
	}
}

// TestMakeJobProgressFn_WritesJobsProgress is the seam the cmd/bridge
// closure uses to thread progress into the in-flight jobs.progress
// row. Drives a phase string through and asserts the row has it.
func TestMakeJobProgressFn_WritesJobsProgress(t *testing.T) {
	srv, db, _ := setupTestServer(t)
	id := srv.startSyncJob(context.Background(), jobTypeUAHubSync)

	fn := srv.makeJobProgressFn(id)
	if fn == nil {
		t.Fatal("makeJobProgressFn returned nil — should always return a callable")
	}
	fn("hydrating 12/17")

	row, err := db.GetJob(context.Background(), id)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	// Progress is JSON-marshalled by the store, so the column
	// holds `"hydrating 12/17"` (quoted). Assert via Contains so
	// the test is robust to a future un-marshal-on-read change.
	if !strings.Contains(row.Progress, "hydrating 12/17") {
		t.Errorf("progress = %q, want it to contain 'hydrating 12/17'", row.Progress)
	}
}

// errFailFixture is a tiny error type so the failure-branch test can
// assert against a stable Error() string without dragging in errors
// from the real sync packages.
type errFailFixture string

func (e errFailFixture) Error() string { return string(e) }
