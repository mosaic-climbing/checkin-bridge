package api

// P3: tests for POST /debug/reset-breakers. Covers:
//   - unconfigured resetter → 503 (fail-closed rather than silently 200)
//   - configured resetter → 200 + wasOpen echoed through
//   - the resetter callback is actually invoked
//
// Auth-path coverage (admin-key vs. session vs. unauth) is handled by
// SecurityMiddleware's own test suite; these tests go directly through
// srv.ServeHTTP without that middleware so we're asserting the handler's
// own contract. The only thing the middleware adds here is "require
// admin key or session", which is covered under security_test.go's
// generic "unknown non-/ui POST path" matrix.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestDebugResetBreakers_NotConfigured verifies the fail-closed shape
// when cmd/bridge never wired a resetter callback. 503 rather than 200
// because a silent "ok=true" response with no work done is actively
// misleading for an operator firing this from a runbook.
func TestDebugResetBreakers_NotConfigured(t *testing.T) {
	srv, _, _ := setupTestServer(t)
	// srv.breakerResetter is unset by default — this is the test.

	req := httptest.NewRequest("POST", "/debug/reset-breakers", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

// TestDebugResetBreakers_WasOpen verifies the happy path: the callback
// fires, and its return value (wasOpen=true) is echoed back in the
// JSON body so a runbook curl sees "we did something real".
func TestDebugResetBreakers_WasOpen(t *testing.T) {
	srv, _, _ := setupTestServer(t)

	var callCount int
	srv.SetBreakerResetter(func() bool {
		callCount++
		return true // simulate "breaker was open"
	})

	req := httptest.NewRequest("POST", "/debug/reset-breakers", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if callCount != 1 {
		t.Errorf("resetter invoked %d times, want 1", callCount)
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["ok"] != true {
		t.Errorf("ok = %v, want true", resp["ok"])
	}
	if resp["wasOpen"] != true {
		t.Errorf("wasOpen = %v, want true", resp["wasOpen"])
	}
	if resp["breaker"] != "recheck" {
		t.Errorf("breaker = %v, want \"recheck\"", resp["breaker"])
	}
}

// TestDebugResetBreakers_NoOp covers the "operator pressed the button
// when it wasn't needed" case: the callback still runs (and logs the
// manual-reset transition), but wasOpen=false makes clear nothing
// actually changed. Keeping this reachable — and clearly distinguished
// in the response — means we don't have to make the endpoint smart
// about "should I have done anything", which would couple the HTTP
// layer to breaker internals.
func TestDebugResetBreakers_NoOp(t *testing.T) {
	srv, _, _ := setupTestServer(t)

	srv.SetBreakerResetter(func() bool {
		return false // simulate "breaker was already closed"
	})

	req := httptest.NewRequest("POST", "/debug/reset-breakers", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["wasOpen"] != false {
		t.Errorf("wasOpen = %v, want false", resp["wasOpen"])
	}
}
