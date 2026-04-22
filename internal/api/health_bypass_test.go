package api

// v0.5.8 coverage for the /health → allowlist bypass (#93).
//
// Rationale: update.sh's loopback health probe (127.0.0.1 → /health)
// was getting 403'd by SecurityMiddleware whenever ALLOWED_NETWORKS
// was tight and didn't include 127.0.0.1/32, which on the v0.5.2
// deploy caused auto-rollback of a perfectly good binary. /health
// is a bare "is the process accepting requests?" probe with no
// auth-state or customer data leakage, so it's moved outside the
// allowlist gate entirely.
//
// These tests pin:
//   1. /health returns 200 even when the caller is NOT in the
//      allowlist (the primary bug-fix behavior).
//   2. /health still emits the security response headers (no
//      regression on nosniff/frame-options/HSTS surface).
//   3. Every OTHER path remains gated by the allowlist (the bypass
//      is path-scoped, not a general allowlist escape).

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newHealthBypassHandler(allowed []*net.IPNet, https bool) http.Handler {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return SecurityMiddleware(SecurityConfig{
		AdminAPIKey:     "test-key",
		AllowedNetworks: allowed,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		HTTPS:           https,
	}, inner)
}

func TestHealthBypassesAllowlist_FromOutsideCIDR(t *testing.T) {
	handler := newHealthBypassHandler(
		[]*net.IPNet{mustParseCIDR("10.0.1.0/24")},
		false,
	)

	// 172.16.0.5 is outside the allowlist. Pre-fix this returned 403;
	// post-fix /health is exempt and returns 200.
	req := httptest.NewRequest("GET", "/health", nil)
	req.RemoteAddr = "172.16.0.5:12345"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("health from outside allowlist: got %d, want 200", w.Code)
	}
}

func TestHealthBypassesAllowlist_FromLoopback(t *testing.T) {
	// The deploy probe case: update.sh curls /health from 127.0.0.1
	// and ALLOWED_NETWORKS doesn't include the loopback block.
	handler := newHealthBypassHandler(
		[]*net.IPNet{mustParseCIDR("10.0.1.0/24")},
		false,
	)

	req := httptest.NewRequest("GET", "/health", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("health from loopback with tight allowlist: got %d, want 200", w.Code)
	}
}

func TestHealthBypass_EmitsSecurityHeaders(t *testing.T) {
	// The fast-path must still emit the nosniff / frame-deny / HSTS
	// headers — a regression here would silently relax the security
	// surface on the one endpoint we're making public.
	handler := newHealthBypassHandler(
		[]*net.IPNet{mustParseCIDR("10.0.1.0/24")},
		true, // HTTPS=true so HSTS should be present
	)

	req := httptest.NewRequest("GET", "/health", nil)
	req.RemoteAddr = "172.16.0.5:12345"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := w.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want DENY", got)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := w.Header().Get("Strict-Transport-Security"); got == "" {
		t.Errorf("Strict-Transport-Security missing on HTTPS=true /health")
	}
}

func TestHealthBypass_IsPathScoped(t *testing.T) {
	// The bypass must NOT turn into a general allowlist escape — any
	// path other than the exact /health string must still be gated.
	handler := newHealthBypassHandler(
		[]*net.IPNet{mustParseCIDR("10.0.1.0/24")},
		false,
	)

	cases := []string{
		"/health/detailed", // looks like /health but isn't the exact path
		"/healthz",         // alternate health probe — NOT in scope
		"/metrics",
		"/ui/",
		"/checkins",
	}
	for _, path := range cases {
		req := httptest.NewRequest("GET", path, nil)
		req.RemoteAddr = "172.16.0.5:12345"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s from outside allowlist: got %d, want 403 (bypass must be exact-match only)", path, w.Code)
		}
	}
}
