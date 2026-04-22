package api

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBcryptAuth(t *testing.T) {
	sm := NewSessionManager("correct-horse-battery")

	// Correct password
	if !sm.Authenticate("correct-horse-battery", "10.0.1.5:1234") {
		t.Error("should authenticate with correct password")
	}

	// Wrong password
	if sm.Authenticate("wrong-password", "10.0.1.5:1234") {
		t.Error("should not authenticate with wrong password")
	}
}

func TestSessionHMACValidation(t *testing.T) {
	sm := NewSessionManager("test-pass")

	token, csrf, err := sm.CreateSession()
	if err != nil {
		t.Fatal(err)
	}
	if csrf == "" {
		t.Error("CreateSession should return a non-empty CSRF token")
	}
	if sm.CSRFTokenFor(token) != csrf {
		t.Error("CSRFTokenFor should return the token bound at session creation")
	}

	// Valid token
	if !sm.ValidateSession(token) {
		t.Error("valid token should validate")
	}

	// Forged token (wrong signature)
	if sm.ValidateSession("abcdef1234567890abcdef1234567890abcdef1234567890.forgedddddddddddd") {
		t.Error("forged token should not validate")
	}

	// Malformed token (no dot)
	if sm.ValidateSession("notokenhere") {
		t.Error("malformed token should not validate")
	}

	// Destroy and re-check
	sm.DestroySession(token)
	if sm.ValidateSession(token) {
		t.Error("destroyed token should not validate")
	}
	// Destroyed session has no CSRF token either
	if sm.CSRFTokenFor(token) != "" {
		t.Error("destroyed session should not carry a CSRF token")
	}
}

func TestLoginRateLimiting(t *testing.T) {
	sm := NewSessionManager("test-pass")
	ip := "10.0.1.100:5555"

	// Fail 5 times
	for i := 0; i < maxLoginFailures; i++ {
		sm.Authenticate("wrong", ip)
	}

	// Should be locked out now — even correct password is rejected
	if sm.Authenticate("test-pass", ip) {
		t.Error("should be locked out after max failures")
	}

	if !sm.IsLockedOut(ip) {
		t.Error("IsLockedOut should return true")
	}

	// Different IP should not be locked
	if !sm.Authenticate("test-pass", "10.0.1.200:5555") {
		t.Error("different IP should not be locked out")
	}
}

func TestParseAllowedNetworks(t *testing.T) {
	tests := []struct {
		input   string
		wantLen int
		wantErr bool
	}{
		{"", 0, false},
		{"10.0.1.0/24", 1, false},
		{"10.0.1.0/24, 192.168.1.0/24", 2, false},
		{"10.0.1.5", 1, false},                   // single IP → /32
		{"10.0.1.0/24, 10.0.1.5", 2, false},      // mixed
		{"not-an-ip", 0, true},                    // invalid
		{"10.0.1.0/33", 0, true},                  // invalid CIDR
	}

	for _, tt := range tests {
		nets, err := ParseAllowedNetworks(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseAllowedNetworks(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			continue
		}
		if len(nets) != tt.wantLen {
			t.Errorf("ParseAllowedNetworks(%q) = %d networks, want %d", tt.input, len(nets), tt.wantLen)
		}
	}
}

func TestIsAllowedIP(t *testing.T) {
	nets := []*net.IPNet{
		mustParseCIDR("10.0.1.0/24"),
		mustParseCIDR("192.168.50.0/24"),
	}

	tests := []struct {
		ip   string
		want bool
	}{
		{"10.0.1.5", true},
		{"10.0.1.254", true},
		{"10.0.2.5", false},      // wrong subnet
		{"192.168.50.1", true},
		{"192.168.51.1", false},  // wrong subnet
		{"172.16.0.1", false},    // not in any allowed range
	}

	for _, tt := range tests {
		if got := isAllowedIP(tt.ip, nets); got != tt.want {
			t.Errorf("isAllowedIP(%q) = %v, want %v", tt.ip, got, tt.want)
		}
	}
}

func TestNormalizeIP(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"10.0.1.5:54321", "10.0.1.5"},
		{"10.0.1.5", "10.0.1.5"},
		{"[::1]:8080", "::1"},
	}

	for _, tt := range tests {
		if got := normalizeIP(tt.input); got != tt.want {
			t.Errorf("normalizeIP(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestCSRFProtection(t *testing.T) {
	sm := NewSessionManager("test-pass")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Create a test handler behind security middleware
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := SecurityMiddleware(SecurityConfig{
		AdminAPIKey: "test-key",
		Sessions:    sm,
		Logger:      logger,
	}, inner)

	// Create a session
	token, csrf, _ := sm.CreateSession()

	// Helper: session + X-Requested-With + cookie + header (the happy path).
	addSessionAuth := func(req *http.Request) {
		req.AddCookie(&http.Cookie{Name: sm.cookieName, Value: token})
		req.AddCookie(&http.Cookie{Name: sm.csrfCookieName, Value: csrf})
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		req.Header.Set("X-CSRF-Token", csrf)
	}

	// POST with session cookie but NO X-Requested-With header → should be rejected
	req := httptest.NewRequest("POST", "/members", strings.NewReader(`{}`))
	req.AddCookie(&http.Cookie{Name: sm.cookieName, Value: token})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("POST without X-Requested-With: got %d, want 403", w.Code)
	}

	// POST with session + X-Requested-With but NO CSRF cookie/header → should be rejected
	req = httptest.NewRequest("POST", "/members", strings.NewReader(`{}`))
	req.AddCookie(&http.Cookie{Name: sm.cookieName, Value: token})
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("POST with X-Requested-With but no CSRF token: got %d, want 403", w.Code)
	}

	// POST with session + X-Requested-With + mismatched CSRF → should be rejected
	req = httptest.NewRequest("POST", "/members", strings.NewReader(`{}`))
	req.AddCookie(&http.Cookie{Name: sm.cookieName, Value: token})
	req.AddCookie(&http.Cookie{Name: sm.csrfCookieName, Value: csrf})
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("X-CSRF-Token", "wrong-token-value")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("POST with mismatched CSRF: got %d, want 403", w.Code)
	}

	// POST with session cookie AND X-Requested-With AND matching CSRF → should pass
	req = httptest.NewRequest("POST", "/members", strings.NewReader(`{}`))
	addSessionAuth(req)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("POST with full session auth + CSRF: got %d, want 200", w.Code)
	}

	// POST with API key (no session) → should pass without X-Requested-With or CSRF
	req = httptest.NewRequest("POST", "/members", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer test-key")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("POST with API key: got %d, want 200", w.Code)
	}

	// GET with session cookie → should pass without X-Requested-With (reads are safe)
	req = httptest.NewRequest("GET", "/cache", nil)
	req.AddCookie(&http.Cookie{Name: sm.cookieName, Value: token})
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("GET with session: got %d, want 200", w.Code)
	}
}

// TestCSRFProtection_UIFragMutating locks in CSRF enforcement on the
// /ui/frag/* branch. Before the S8 fix the /ui/* branch returned early
// after session auth, so POST /ui/frag/door-policy (and the match/skip/
// defer siblings) accepted session-only requests with no CSRF token —
// an attacker page could fire mutations at a logged-in staff browser.
// These cases keep the bug from regressing.
func TestCSRFProtection_UIFragMutating(t *testing.T) {
	sm := NewSessionManager("test-pass")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := SecurityMiddleware(SecurityConfig{
		AdminAPIKey: "test-key",
		Sessions:    sm,
		Logger:      logger,
	}, inner)

	token, csrf, _ := sm.CreateSession()

	// Mutating POST to /ui/frag/* with session but NO CSRF → 403
	req := httptest.NewRequest("POST", "/ui/frag/door-policy", strings.NewReader(`{}`))
	req.AddCookie(&http.Cookie{Name: sm.cookieName, Value: token})
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("POST /ui/frag/door-policy without CSRF: got %d, want 403", w.Code)
	}

	// Mutating POST to /ui/frag/* with session + missing X-Requested-With → 403
	req = httptest.NewRequest("POST", "/ui/frag/unmatched/abc/match", strings.NewReader(`{}`))
	req.AddCookie(&http.Cookie{Name: sm.cookieName, Value: token})
	req.AddCookie(&http.Cookie{Name: sm.csrfCookieName, Value: csrf})
	req.Header.Set("X-CSRF-Token", csrf)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("POST /ui/frag/unmatched/*/match without X-Requested-With: got %d, want 403", w.Code)
	}

	// Mutating DELETE to /ui/frag/* with mismatched CSRF → 403
	req = httptest.NewRequest("DELETE", "/ui/frag/door-policy/door-123", nil)
	req.AddCookie(&http.Cookie{Name: sm.cookieName, Value: token})
	req.AddCookie(&http.Cookie{Name: sm.csrfCookieName, Value: csrf})
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("X-CSRF-Token", "wrong-token")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("DELETE /ui/frag/door-policy with mismatched CSRF: got %d, want 403", w.Code)
	}

	// Happy path: mutating POST with full session + CSRF → 200
	req = httptest.NewRequest("POST", "/ui/frag/door-policy", strings.NewReader(`{}`))
	req.AddCookie(&http.Cookie{Name: sm.cookieName, Value: token})
	req.AddCookie(&http.Cookie{Name: sm.csrfCookieName, Value: csrf})
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("X-CSRF-Token", csrf)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("POST /ui/frag/door-policy with full auth: got %d, want 200", w.Code)
	}

	// GET to /ui/frag/* with session → 200 (no CSRF required on reads)
	req = httptest.NewRequest("GET", "/ui/frag/unmatched-list", nil)
	req.AddCookie(&http.Cookie{Name: sm.cookieName, Value: token})
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("GET /ui/frag/unmatched-list with session: got %d, want 200", w.Code)
	}
}

// TestExtractClientIP_TrustModel covers the S1 fix: forwarding headers
// are honoured only when r.RemoteAddr's IP is itself inside one of the
// configured trusted-proxy CIDRs. Every row in this table is a case that
// used to trust attacker-supplied data; the expected value is the real
// peer identity after the fix.
func TestExtractClientIP_TrustModel(t *testing.T) {
	trusted := []*net.IPNet{mustParseCIDR("10.99.0.0/16")}

	tests := []struct {
		name           string
		remoteAddr     string
		xff            string
		xRealIP        string
		trustedProxies []*net.IPNet
		want           string
	}{
		{
			name:           "no trusted proxies → ignore XFF (attacker spoof attempt)",
			remoteAddr:     "10.0.1.5:12345",
			xff:            "10.0.1.1",
			trustedProxies: nil,
			want:           "10.0.1.5",
		},
		{
			name:           "no trusted proxies → ignore X-Real-IP (attacker spoof attempt)",
			remoteAddr:     "10.0.1.5:12345",
			xRealIP:        "10.0.1.1",
			trustedProxies: nil,
			want:           "10.0.1.5",
		},
		{
			name:           "peer is a trusted proxy → XFF honoured",
			remoteAddr:     "10.99.0.7:5000",
			xff:            "192.168.1.100",
			trustedProxies: trusted,
			want:           "192.168.1.100",
		},
		{
			name:           "peer is not a trusted proxy → XFF ignored even though list is populated",
			remoteAddr:     "172.16.0.5:5000",
			xff:            "192.168.1.100",
			trustedProxies: trusted,
			want:           "172.16.0.5",
		},
		{
			name:           "RFC 7239 walk: right-to-left, last untrusted wins",
			remoteAddr:     "10.99.0.7:5000",
			xff:            "attacker-injected-value, 192.168.1.100, 10.99.0.3",
			trustedProxies: trusted,
			want:           "192.168.1.100",
		},
		{
			name:           "XFF entirely trusted → fall through to X-Real-IP",
			remoteAddr:     "10.99.0.7:5000",
			xff:            "10.99.0.3, 10.99.0.4",
			xRealIP:        "192.168.1.100",
			trustedProxies: trusted,
			want:           "192.168.1.100",
		},
		{
			name:           "XFF empty + trusted peer + X-Real-IP → X-Real-IP honoured",
			remoteAddr:     "10.99.0.7:5000",
			xRealIP:        "192.168.1.100",
			trustedProxies: trusted,
			want:           "192.168.1.100",
		},
		{
			name:           "XFF empty, no X-Real-IP, untrusted peer → RemoteAddr host",
			remoteAddr:     "172.16.0.5:5000",
			trustedProxies: trusted,
			want:           "172.16.0.5",
		},
		{
			name:           "IPv6 RemoteAddr without port → fall back to raw string",
			remoteAddr:     "2001:db8::1",
			trustedProxies: nil,
			want:           "2001:db8::1",
		},
		{
			name:           "spaces around XFF entries get trimmed",
			remoteAddr:     "10.99.0.7:5000",
			xff:            "   192.168.1.100   ",
			trustedProxies: trusted,
			want:           "192.168.1.100",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			req.RemoteAddr = tt.remoteAddr
			if tt.xff != "" {
				req.Header.Set("X-Forwarded-For", tt.xff)
			}
			if tt.xRealIP != "" {
				req.Header.Set("X-Real-IP", tt.xRealIP)
			}
			got := extractClientIP(req, tt.trustedProxies)
			if got != tt.want {
				t.Errorf("extractClientIP = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestSecurityMiddleware_AllowlistHonoursTrust is the integration
// assertion: the IP allowlist must see the post-trust-check peer, not
// whatever the XFF header claims. Without the S1 fix, this test would
// pass by returning 200 (the spoofed XFF matches the allowlist), which
// is the exact bug.
func TestSecurityMiddleware_AllowlistHonoursTrust(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := SecurityMiddleware(SecurityConfig{
		AdminAPIKey:     "test-key",
		AllowedNetworks: []*net.IPNet{mustParseCIDR("10.0.1.0/24")},
		TrustedProxies:  nil, // nothing in front of us
		Logger:          logger,
	}, inner)

	// Attacker connects from 172.16.0.5 (outside the allowlist) and
	// sets XFF to an allowlisted value. The middleware must block.
	//
	// (Path: /metrics rather than /health — /health now bypasses the
	// allowlist unconditionally for the deploy-probe use case (#93), so
	// testing the allowlist itself needs a gated path.)
	req := httptest.NewRequest("GET", "/metrics", nil)
	req.RemoteAddr = "172.16.0.5:12345"
	req.Header.Set("X-Forwarded-For", "10.0.1.50")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("spoofed XFF allowlist bypass: got %d, want 403", w.Code)
	}

	// Legitimate caller from inside the allowlist, no XFF. 401 is fine
	// here — we're not providing an API key, so the allowlist path lets
	// us through and the auth layer returns Unauthorized. The point of
	// this assertion is "not 403" (IP gate didn't block), not "200".
	req = httptest.NewRequest("GET", "/metrics", nil)
	req.RemoteAddr = "10.0.1.50:12345"
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code == http.StatusForbidden {
		t.Errorf("legitimate allowlisted caller: got 403, want not-403 (the IP gate must not block)")
	}
}

func mustParseCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

// TestHSTS_AbsentByDefault verifies HSTS header is absent when HTTPS=false.
func TestHSTS_AbsentByDefault(t *testing.T) {
	sm := NewSessionManager("test-pass")
	_, _, err := sm.CreateSession()
	if err != nil {
		t.Fatal(err)
	}

	handler := SecurityMiddleware(SecurityConfig{
		AdminAPIKey: "test-key",
		Sessions:    sm,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		HTTPS:       false,
	}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	hsts := w.Header().Get("Strict-Transport-Security")
	if hsts != "" {
		t.Errorf("HSTS should be absent when HTTPS=false, got %q", hsts)
	}
}

// TestHSTS_PresentWhenHTTPSEnabled verifies HSTS header is present when HTTPS=true.
func TestHSTS_PresentWhenHTTPSEnabled(t *testing.T) {
	sm := NewSessionManager("test-pass")
	_, _, err := sm.CreateSession()
	if err != nil {
		t.Fatal(err)
	}

	handler := SecurityMiddleware(SecurityConfig{
		AdminAPIKey: "test-key",
		Sessions:    sm,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		HTTPS:       true,
	}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	hsts := w.Header().Get("Strict-Transport-Security")
	expectedHSTS := "max-age=31536000; includeSubDomains"
	if hsts != expectedHSTS {
		t.Errorf("HSTS header mismatch: got %q, want %q", hsts, expectedHSTS)
	}
}
