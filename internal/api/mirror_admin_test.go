package api

// Tests for the POST /admin/mirror/resync and GET /admin/mirror/stats
// handlers. These live on the control plane, so they're exercised
// through srv.ControlHandler() rather than the public mux — pinning the
// routing on the correct listener is part of what we're asserting.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/mosaic-climbing/checkin-bridge/internal/store"
)

// TestMirrorResync_NoWalkerConfigured asserts that /admin/mirror/resync
// returns 503 when the Walker setter hasn't been invoked. This is the
// same defensive shape as /debug/reset-breakers and prevents a
// misconfigured bridge from silently dispatching a no-op "sync".
func TestMirrorResync_NoWalkerConfigured(t *testing.T) {
	srv, _, _ := setupTestServer(t)
	// Intentionally do NOT call SetMirrorWalker.

	req := httptest.NewRequest("POST", "/admin/mirror/resync", nil)
	w := httptest.NewRecorder()
	srv.ControlHandler().ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

// TestMirrorResync_Dispatches_202 verifies the happy path: a wired
// walker, no running sync, returns 202 Accepted and the walker
// callback is invoked in the background.
func TestMirrorResync_Dispatches_202(t *testing.T) {
	srv, _, _ := setupTestServer(t)

	var walkCalls int32
	walkDone := make(chan struct{})
	srv.SetMirrorWalker(func(ctx context.Context) error {
		// Use atomic-alike without importing sync/atomic — a plain
		// int32 ++ is unsafe under -race, so use a mutex-guarded
		// counter.
		testCounterInc(&walkCalls)
		close(walkDone)
		return nil
	})

	req := httptest.NewRequest("POST", "/admin/mirror/resync", nil)
	w := httptest.NewRecorder()
	srv.ControlHandler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v (%s)", err, w.Body.String())
	}
	if resp["status"] != "dispatched" {
		t.Errorf("status = %v, want dispatched", resp["status"])
	}

	// Verify the walker ACTUALLY ran — bg.Go is fire-and-forget from
	// the handler's POV, but the goroutine should fire within a
	// reasonable window even on a loaded CI runner.
	select {
	case <-walkDone:
	case <-time.After(2 * time.Second):
		t.Fatal("walker callback never fired after 2s")
	}
}

// TestMirrorResync_409WhenRunningFresh verifies that a "running"
// sync_state with a recent started_at causes a 409 Conflict and does
// NOT invoke the walker a second time. This is the main defence
// against an impatient operator kicking off overlapping runs.
func TestMirrorResync_409WhenRunningFresh(t *testing.T) {
	srv, db, _ := setupTestServer(t)

	// Seed a fresh running state (started_at = now).
	ctx := context.Background()
	if err := db.UpdateSyncState(ctx, &store.SyncState{
		Status:    "running",
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("seed sync_state: %v", err)
	}

	var walkCalls int32
	srv.SetMirrorWalker(func(ctx context.Context) error {
		testCounterInc(&walkCalls)
		return nil
	})

	req := httptest.NewRequest("POST", "/admin/mirror/resync", nil)
	w := httptest.NewRecorder()
	srv.ControlHandler().ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", w.Code)
	}

	// Sleep briefly to catch any stray dispatch. We're asserting
	// walkCalls == 0 stays 0; a 50ms window is a pragmatic upper
	// bound on "did bg.Go fire a goroutine before we looked?"
	time.Sleep(50 * time.Millisecond)
	if n := testCounterGet(&walkCalls); n != 0 {
		t.Errorf("walker invoked %d times despite 409", n)
	}
}

// TestMirrorResync_AllowsWhenRunningIsStale verifies that a "running"
// state with a started_at older than mirrorStaleWindow is treated as
// dead and a new walk is allowed to start. This keeps a crashed
// bridge from permanently locking out future syncs.
func TestMirrorResync_AllowsWhenRunningIsStale(t *testing.T) {
	srv, db, _ := setupTestServer(t)

	// Seed a stale running state (started_at = 2h ago, way past
	// the 30m stale window).
	ctx := context.Background()
	staleAt := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	if err := db.UpdateSyncState(ctx, &store.SyncState{
		Status:    "running",
		StartedAt: staleAt,
	}); err != nil {
		t.Fatalf("seed sync_state: %v", err)
	}

	fired := make(chan struct{})
	srv.SetMirrorWalker(func(ctx context.Context) error {
		close(fired)
		return nil
	})

	req := httptest.NewRequest("POST", "/admin/mirror/resync", nil)
	w := httptest.NewRecorder()
	srv.ControlHandler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202 (stale lock released); body=%s", w.Code, w.Body.String())
	}
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("walker didn't fire on stale-lock path")
	}
}

// TestMirrorResync_WalkerErrorSurfacesInSyncState — we don't currently
// surface the walk's error on the HTTP response (it's async), but the
// walker itself updates sync_state, so operators can poll /stats
// afterward. Verify that a walker returning an error doesn't cause
// any panic in the bg.Go wrapper path.
func TestMirrorResync_WalkerErrorDoesNotPanic(t *testing.T) {
	srv, _, _ := setupTestServer(t)

	done := make(chan struct{})
	srv.SetMirrorWalker(func(ctx context.Context) error {
		defer close(done)
		return errors.New("simulated failure")
	})

	req := httptest.NewRequest("POST", "/admin/mirror/resync", nil)
	w := httptest.NewRecorder()
	srv.ControlHandler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("walker goroutine didn't complete")
	}
}

// TestMirrorStats_HappyPath verifies GET /admin/mirror/stats returns
// JSON with syncState + counts + totalRows fields populated.
func TestMirrorStats_HappyPath(t *testing.T) {
	srv, db, _ := setupTestServer(t)
	ctx := context.Background()

	// Seed two customers at different badge statuses.
	if err := db.UpsertCustomerWithBadgeBatch(ctx, []store.Customer{
		{RedpointID: "c1", FirstName: "A", LastName: "Active",
			BadgeStatus: "ACTIVE", LastSyncedAt: time.Now().Format(time.RFC3339)},
		{RedpointID: "c2", FirstName: "F", LastName: "Frozen",
			BadgeStatus: "FROZEN", LastSyncedAt: time.Now().Format(time.RFC3339)},
	}); err != nil {
		t.Fatalf("seed customers: %v", err)
	}

	req := httptest.NewRequest("GET", "/admin/mirror/stats", nil)
	w := httptest.NewRecorder()
	srv.ControlHandler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v (%s)", err, w.Body.String())
	}

	counts, ok := resp["counts"].(map[string]any)
	if !ok {
		t.Fatalf("counts field missing or wrong type: %T", resp["counts"])
	}
	if counts["ACTIVE"].(float64) != 1 {
		t.Errorf("counts.ACTIVE = %v, want 1", counts["ACTIVE"])
	}
	if counts["FROZEN"].(float64) != 1 {
		t.Errorf("counts.FROZEN = %v, want 1", counts["FROZEN"])
	}
	if resp["totalRows"].(float64) != 2 {
		t.Errorf("totalRows = %v, want 2", resp["totalRows"])
	}
	// syncState must be present (even if just the seeded id=1 row).
	if _, ok := resp["syncState"]; !ok {
		t.Errorf("syncState field missing")
	}
	if resp["staleWindowS"].(float64) != 1800 {
		t.Errorf("staleWindowS = %v, want 1800", resp["staleWindowS"])
	}
}

// TestMirrorAdmin_RoutesOnControlMux pins both endpoints to the
// control plane (not the public mux). If someone later accidentally
// registers them on s.mux, this test catches the regression.
func TestMirrorAdmin_RoutesOnControlMux(t *testing.T) {
	srv, _, _ := setupTestServer(t)
	srv.SetMirrorWalker(func(ctx context.Context) error { return nil })

	for _, tc := range []struct {
		method, path string
	}{
		{"POST", "/admin/mirror/resync"},
		{"GET", "/admin/mirror/stats"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			// Public mux must NOT dispatch these.
			req := httptest.NewRequest(tc.method, tc.path, nil)
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, req)
			if w.Code != http.StatusNotFound {
				t.Errorf("%s %s on public mux returned %d, want 404 (route leaked)", tc.method, tc.path, w.Code)
			}

			// Control mux must dispatch.
			req2 := httptest.NewRequest(tc.method, tc.path, nil)
			w2 := httptest.NewRecorder()
			srv.ControlHandler().ServeHTTP(w2, req2)
			if w2.Code == http.StatusNotFound {
				t.Errorf("%s %s on control mux returned 404, want registered", tc.method, tc.path)
			}
		})
	}
}

// ─── tiny race-safe counter ─────────────────────────────────────
//
// We avoid sync/atomic to keep the test import block minimal;
// a trivial mutex does the job.
var testMu sync.Mutex

func testCounterInc(p *int32) {
	testMu.Lock()
	defer testMu.Unlock()
	*p++
}
func testCounterGet(p *int32) int32 {
	testMu.Lock()
	defer testMu.Unlock()
	return *p
}
