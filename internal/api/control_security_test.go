package api

// Tests for ControlSecurityMiddleware. This middleware backs the control
// plane listener (127.0.0.1:ControlPort) and is intentionally simpler
// than SecurityMiddleware: admin Bearer key only, no session auth, no
// /ui carve-outs, no CSRF. Every assertion below pins a property that
// would be a meaningful regression if it silently flipped.

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func controlTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// ok is a trivial terminal handler that signals "the middleware let the
// request through". Every middleware-allows test asserts we reach this.
var controlOKHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
})

// TestControlMiddleware_RejectsMissingAuth pins that the control plane
// never serves an unauthenticated request. Unlike the public plane, it
// has no /health bypass, no /ui pass-through, and no session-cookie
// path — the only credential is the admin API key.
func TestControlMiddleware_RejectsMissingAuth(t *testing.T) {
	mw := ControlSecurityMiddleware(ControlSecurityConfig{
		AdminAPIKey: "secret",
		Logger:      controlTestLogger(),
	}, controlOKHandler)

	req := httptest.NewRequest("POST", "/unlock/door-1", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	mw.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (missing Authorization)", w.Code)
	}
	if w.Header().Get("WWW-Authenticate") == "" {
		t.Error("missing WWW-Authenticate challenge on 401 response")
	}
}

// TestControlMiddleware_RejectsWrongKey — wrong key is the same as no
// key from the security middleware's point of view.
func TestControlMiddleware_RejectsWrongKey(t *testing.T) {
	mw := ControlSecurityMiddleware(ControlSecurityConfig{
		AdminAPIKey: "secret",
		Logger:      controlTestLogger(),
	}, controlOKHandler)

	req := httptest.NewRequest("POST", "/unlock/door-1", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer wrong-key")
	w := httptest.NewRecorder()
	mw.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (wrong bearer token)", w.Code)
	}
}

// TestControlMiddleware_AcceptsCorrectKey — positive case.
func TestControlMiddleware_AcceptsCorrectKey(t *testing.T) {
	mw := ControlSecurityMiddleware(ControlSecurityConfig{
		AdminAPIKey: "secret",
		Logger:      controlTestLogger(),
	}, controlOKHandler)

	req := httptest.NewRequest("POST", "/unlock/door-1", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	mw.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (valid admin key)", w.Code)
	}
}

// TestControlMiddleware_EmptyKeyConfigRefusesAll pins the "empty config
// key must not fall open" invariant. config.validate() enforces
// AdminAPIKey != "" at boot, but the middleware is a belt-and-braces
// second check — an empty configured key must reject every request.
// A bug here would turn the control plane into an open listener.
func TestControlMiddleware_EmptyKeyConfigRefusesAll(t *testing.T) {
	mw := ControlSecurityMiddleware(ControlSecurityConfig{
		AdminAPIKey: "", // misconfigured
		Logger:      controlTestLogger(),
	}, controlOKHandler)

	req := httptest.NewRequest("POST", "/unlock/door-1", strings.NewReader(`{}`))
	// Even sending the empty string as the Bearer token — which would
	// fool a naive ConstantTimeCompare of two zero-length strings —
	// must still be refused.
	req.Header.Set("Authorization", "Bearer ")
	w := httptest.NewRecorder()
	mw.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (empty AdminAPIKey must refuse every request, not fall open)", w.Code)
	}
}

// TestControlMiddleware_NoCSRFNoSessionCarveout asserts the middleware
// does not honour session cookies or the /ui pass-through rules. If the
// request has no bearer key, 401 — regardless of any other headers.
func TestControlMiddleware_NoCSRFNoSessionCarveout(t *testing.T) {
	mw := ControlSecurityMiddleware(ControlSecurityConfig{
		AdminAPIKey: "secret",
		Logger:      controlTestLogger(),
	}, controlOKHandler)

	// /ui path on the control plane — public middleware would serve it,
	// control middleware must refuse (no key).
	req := httptest.NewRequest("GET", "/ui/login", nil)
	w := httptest.NewRecorder()
	mw.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("/ui/login on control plane: status = %d, want 401 (no /ui carve-out on control plane)", w.Code)
	}

	// /health on the control plane — public middleware would serve it,
	// control middleware must refuse (no key). /health has no business
	// being on the control plane; if the route ever lands there, the
	// credential gate must still apply.
	req2 := httptest.NewRequest("GET", "/health", nil)
	w2 := httptest.NewRecorder()
	mw.ServeHTTP(w2, req2)
	if w2.Code != http.StatusUnauthorized {
		t.Errorf("/health on control plane: status = %d, want 401 (no /health bypass on control plane)", w2.Code)
	}
}
