package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Tests for the shell-route fix (deep links), the vendored-static
// route, and the middleware's /ui-prefix handling. Pre-fix, every
// hx-push-url URL (/ui/members, /ui/sync, …) was served by a catch-all
// that hardcoded the dashboard, logged-out deep links rendered raw
// JSON 401s, and htmx came from a CDN.

// wrapUI applies the production security middleware to srv, so these
// tests exercise the real request path (middleware decides who sees
// login vs JSON vs redirects — the bare mux would skip all of that).
func wrapUI(t *testing.T, srv *Server) http.Handler {
	t.Helper()
	return SecurityMiddleware(SecurityConfig{
		AdminAPIKey: "test-admin-key",
		Sessions:    srv.sessions,
		Logger:      discardLogger(),
	}, srv)
}

// uiSession returns a logged-in session cookie pair for srv.
func uiSession(t *testing.T, srv *Server) []*http.Cookie {
	t.Helper()
	token, csrf, err := srv.sessions.CreateSession()
	if err != nil {
		t.Fatal(err)
	}
	return []*http.Cookie{
		{Name: srv.sessions.cookieName, Value: token},
		{Name: srv.sessions.csrfCookieName, Value: csrf},
	}
}

func getUI(t *testing.T, h http.Handler, path string, cookies []*http.Cookie, htmx bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	if htmx {
		req.Header.Set("HX-Request", "true")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestUIDeepLinks_RenderTheNamedPage(t *testing.T) {
	srv, _, _ := setupTestServer(t)
	h := wrapUI(t, srv)
	cookies := uiSession(t, srv)

	// Each pushed URL must render ITS page server-side, not dashboard.
	pages := map[string]string{
		"/ui/":            "Real-time gym access overview", // dashboard subtitle
		"/ui/members":     "Manage NFC-enrolled members",
		"/ui/needs-match": "Needs Match",
		"/ui/checkins":    "Check-in History",
		"/ui/sync":        "Sync &amp; Jobs",
		"/ui/metrics":     "Metrics",
	}
	for path, marker := range pages {
		w := getUI(t, h, path, cookies, false)
		if w.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, w.Code)
			continue
		}
		body := w.Body.String()
		if !strings.Contains(body, marker) {
			t.Errorf("GET %s did not render its own page (marker %q missing)", path, marker)
		}
		// Full-page render: must include the layout shell.
		if !strings.Contains(body, "<nav class=\"sidebar\">") {
			t.Errorf("GET %s missing layout shell", path)
		}
	}
}

func TestUIDeepLinks_HTMXGetsFragmentOnly(t *testing.T) {
	srv, _, _ := setupTestServer(t)
	h := wrapUI(t, srv)
	cookies := uiSession(t, srv)

	w := getUI(t, h, "/ui/members", cookies, true)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "<nav class=\"sidebar\">") {
		t.Error("HX-Request response included the full layout; want fragment only")
	}
	if !strings.Contains(body, "Manage NFC-enrolled members") {
		t.Error("HX-Request response missing the members page content")
	}
}

func TestUIDeepLinks_UnknownPage404s(t *testing.T) {
	srv, _, _ := setupTestServer(t)
	h := wrapUI(t, srv)
	cookies := uiSession(t, srv)

	w := getUI(t, h, "/ui/definitely-not-a-page", cookies, false)
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown page = %d, want 404 (the old silent dashboard fallback masked broken links)", w.Code)
	}
}

func TestUIDeepLinks_LoggedOutGetsLoginPageNotJSON(t *testing.T) {
	srv, _, _ := setupTestServer(t)
	h := wrapUI(t, srv)

	w := getUI(t, h, "/ui/members", nil, false)
	if w.Code != http.StatusOK {
		t.Fatalf("logged-out full page = %d, want 200 (login page)", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Staff password") {
		t.Errorf("logged-out deep link did not render the login page; body starts %q", body[:min(80, len(body))])
	}
	if strings.Contains(body, `{"error"`) {
		t.Error("logged-out deep link rendered raw JSON")
	}
}

func TestUIDeepLinks_LoggedOutHTMXGetsRedirect(t *testing.T) {
	srv, _, _ := setupTestServer(t)
	h := wrapUI(t, srv)

	w := getUI(t, h, "/ui/members", nil, true)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("logged-out HTMX = %d, want 401", w.Code)
	}
	if w.Header().Get("HX-Redirect") == "" {
		t.Error("logged-out HTMX response missing HX-Redirect")
	}
}

func TestVendoredHTMX_ServedPublicAndCached(t *testing.T) {
	srv, _, _ := setupTestServer(t)
	h := wrapUI(t, srv)

	// No session cookies: the login page loads this pre-auth.
	w := getUI(t, h, "/ui/static/htmx-2.0.4.min.js", nil, false)
	if w.Code != http.StatusOK {
		t.Fatalf("static asset = %d, want 200 without a session", w.Code)
	}
	if got := w.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("Cache-Control = %q, want immutable long-cache (filename-versioned asset)", got)
	}
	if w.Body.Len() < 10_000 {
		t.Errorf("htmx asset suspiciously small: %d bytes", w.Body.Len())
	}
}

func TestGenericBranch_HTMXSessionExpiryGetsRedirect(t *testing.T) {
	srv, _, _ := setupTestServer(t)

	// A sync-page button POST (non-/ui path) with no session and
	// HX-Request set: must HX-Redirect to login, not return bare JSON —
	// otherwise the button spinner stops and nothing visibly happens.
	h := wrapUI(t, srv)
	req := httptest.NewRequest("POST", "/cache/sync", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", w.Code)
	}
	if w.Header().Get("HX-Redirect") != "/ui/" {
		t.Errorf("HX-Redirect = %q, want /ui/", w.Header().Get("HX-Redirect"))
	}
}
