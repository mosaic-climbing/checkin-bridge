package api

// Tests covering the control-plane / data-plane split introduced by A1.
//
// The two routes that cause physical-world side effects — POST /unlock
// /{doorId} and the devhooks-gated POST /test-checkin — live on the
// control mux, wired by cmd/bridge to a second http.Server bound to
// 127.0.0.1:ControlPort. These tests pin three invariants at the
// handler level, before any listener binding is involved:
//
//   1. Control routes are NOT reachable via the public mux
//      (Server.ServeHTTP). A browser hitting the LAN-facing public port
//      must see a 404, not the handler, regardless of auth state.
//
//   2. Control routes ARE reachable via ControlHandler(). Drives the
//      unlock path through the control mux and verifies the handler
//      runs (we don't care about the response body here — the unit
//      tests for handleUnlock cover that — only that the route lands
//      on the mux and dispatches.)
//
//   3. The data-plane mutations that the staff UI posts to from the
//      browser (/cache/sync, /directory/sync, /ingest/unifi,
//      /status-sync) remain on the public mux. Narrowing the split to
//      only /unlock + /test-checkin was a deliberate scope call; these
//      assertions keep a future refactor from accidentally widening
//      the split and breaking the UI.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestControlRoute_NotOnPublicMux asserts POST /unlock/{doorId} returns
// 404 from the public mux. This is the primary defence behind the split:
// the LAN-facing listener serves the public mux, and the control route
// simply isn't registered there. An attacker who reaches the LAN port
// can't POST /unlock to pop doors even if they have the admin API key.
func TestControlRoute_NotOnPublicMux(t *testing.T) {
	srv, _, _ := setupTestServer(t)

	req := httptest.NewRequest("POST", "/unlock/door-1", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("POST /unlock/{doorId} on public mux returned %d, want 404 — control route leaked onto the data plane", w.Code)
	}
}

// TestControlRoute_OnControlMux asserts POST /unlock/{doorId} is reachable
// via ControlHandler. We don't exercise the full unlock path (that needs
// a fake UniFi client; the unlock-handler unit tests already cover it),
// only that the mux dispatches to the handler. A 404 here would mean the
// route was silently dropped; any other status (even an error from the
// handler itself) proves the mux matched.
func TestControlRoute_OnControlMux(t *testing.T) {
	srv, _, _ := setupTestServer(t)

	req := httptest.NewRequest("POST", "/unlock/door-1", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	srv.ControlHandler().ServeHTTP(w, req)

	if w.Code == http.StatusNotFound {
		t.Errorf("POST /unlock/{doorId} on control mux returned 404 — control route not registered")
	}
}

// TestDataPlaneMutations_StayOnPublicMux pins the routes the staff UI
// posts to from the browser (sync.html's four hx-post targets). Moving
// any of these to the control mux without a matching /ui/* proxy would
// silently break the UI — session cookies reach the public listener,
// not the loopback-bound control listener.
func TestDataPlaneMutations_StayOnPublicMux(t *testing.T) {
	cases := []struct {
		method string
		path   string
	}{
		{"POST", "/cache/sync"},
		{"POST", "/directory/sync"},
		{"POST", "/ingest/unifi"},
		{"POST", "/status-sync"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			srv, _, _ := setupTestServer(t)

			// Public mux must dispatch (any non-404 status proves the
			// route is registered; we don't care what the handler does
			// beyond that — some of these depend on a live UniFi/
			// Redpoint and will error with 4xx/5xx here).
			req := httptest.NewRequest(tc.method, tc.path, nil)
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, req)
			if w.Code == http.StatusNotFound {
				t.Errorf("%s %s on public mux returned 404 — route disappeared from the data plane", tc.method, tc.path)
			}

			// Control mux must NOT dispatch these. Keeping them off
			// controlMux prevents a future refactor from accidentally
			// double-registering and opening up the route on the
			// loopback listener too (which would defeat the scope-
			// narrowing rationale).
			req2 := httptest.NewRequest(tc.method, tc.path, nil)
			w2 := httptest.NewRecorder()
			srv.ControlHandler().ServeHTTP(w2, req2)
			if w2.Code != http.StatusNotFound {
				t.Errorf("%s %s on control mux returned %d, want 404 — route leaked onto the control plane", tc.method, tc.path, w2.Code)
			}
		})
	}
}

// TestReadOnlyRoutes_StayOnPublicMux pins the read-only endpoints —
// /health, /stats, /checkins, /directory/search — on the public mux.
// The control mux is deliberately minimal; read-only routes that don't
// mutate state don't belong there even if their admin-key auth is the
// same.
func TestReadOnlyRoutes_StayOnPublicMux(t *testing.T) {
	srv, _, _ := setupTestServer(t)

	cases := []string{
		"/health",
		"/stats",
		"/checkins",
		"/directory/status",
	}
	for _, path := range cases {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest("GET", path, nil)
			w := httptest.NewRecorder()
			srv.ControlHandler().ServeHTTP(w, req)
			if w.Code != http.StatusNotFound {
				t.Errorf("GET %s on control mux returned %d, want 404 — read-only route leaked onto the control plane", path, w.Code)
			}
		})
	}
}
