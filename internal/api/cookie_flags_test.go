package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSetCookie_InsecureByDefault verifies cookies are insecure when secureCookies is false.
func TestSetCookie_InsecureByDefault(t *testing.T) {
	sm := NewSessionManager("testpass")
	w := httptest.NewRecorder()

	sm.SetCookie(w, "test-token")

	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected 1 cookie, got %d", len(cookies))
	}
	cookie := cookies[0]
	if cookie.Secure {
		t.Errorf("expected Secure=false, got true")
	}
	if cookie.SameSite != http.SameSiteStrictMode {
		t.Errorf("expected SameSite=Strict, got %v", cookie.SameSite)
	}
	if !cookie.HttpOnly {
		t.Errorf("expected HttpOnly=true, got false")
	}
}

// TestSetCookie_SecureWhenEnabled verifies cookies are secure when secureCookies is true.
func TestSetCookie_SecureWhenEnabled(t *testing.T) {
	sm := NewSessionManager("testpass")
	sm.SetSecureCookies(true)
	w := httptest.NewRecorder()

	sm.SetCookie(w, "test-token")

	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected 1 cookie, got %d", len(cookies))
	}
	cookie := cookies[0]
	if !cookie.Secure {
		t.Errorf("expected Secure=true, got false")
	}
	if cookie.SameSite != http.SameSiteStrictMode {
		t.Errorf("expected SameSite=Strict, got %v", cookie.SameSite)
	}
	if !cookie.HttpOnly {
		t.Errorf("expected HttpOnly=true, got false")
	}
}

// TestClearCookie_InsecureByDefault verifies clear cookies are insecure by default.
func TestClearCookie_InsecureByDefault(t *testing.T) {
	sm := NewSessionManager("testpass")
	w := httptest.NewRecorder()

	sm.ClearCookie(w)

	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected 1 cookie, got %d", len(cookies))
	}
	cookie := cookies[0]
	if cookie.Secure {
		t.Errorf("expected Secure=false, got true")
	}
	if cookie.MaxAge != -1 {
		t.Errorf("expected MaxAge=-1, got %d", cookie.MaxAge)
	}
}

// TestClearCookie_SecureWhenEnabled verifies clear cookies are secure when enabled.
func TestClearCookie_SecureWhenEnabled(t *testing.T) {
	sm := NewSessionManager("testpass")
	sm.SetSecureCookies(true)
	w := httptest.NewRecorder()

	sm.ClearCookie(w)

	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected 1 cookie, got %d", len(cookies))
	}
	cookie := cookies[0]
	if !cookie.Secure {
		t.Errorf("expected Secure=true, got false")
	}
	if cookie.MaxAge != -1 {
		t.Errorf("expected MaxAge=-1, got %d", cookie.MaxAge)
	}
}

// TestSetCSRFCookie_InsecureByDefault verifies CSRF cookies are insecure by default.
func TestSetCSRFCookie_InsecureByDefault(t *testing.T) {
	sm := NewSessionManager("testpass")
	w := httptest.NewRecorder()

	sm.SetCSRFCookie(w, "csrf-token")

	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected 1 cookie, got %d", len(cookies))
	}
	cookie := cookies[0]
	if cookie.Secure {
		t.Errorf("expected Secure=false, got true")
	}
	if cookie.SameSite != http.SameSiteStrictMode {
		t.Errorf("expected SameSite=Strict, got %v", cookie.SameSite)
	}
	if !cookie.HttpOnly {
		t.Errorf("expected HttpOnly=true, got false")
	}
}

// TestSetCSRFCookie_SecureWhenEnabled verifies CSRF cookies are secure when enabled.
func TestSetCSRFCookie_SecureWhenEnabled(t *testing.T) {
	sm := NewSessionManager("testpass")
	sm.SetSecureCookies(true)
	w := httptest.NewRecorder()

	sm.SetCSRFCookie(w, "csrf-token")

	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected 1 cookie, got %d", len(cookies))
	}
	cookie := cookies[0]
	if !cookie.Secure {
		t.Errorf("expected Secure=true, got false")
	}
	if cookie.SameSite != http.SameSiteStrictMode {
		t.Errorf("expected SameSite=Strict, got %v", cookie.SameSite)
	}
	if !cookie.HttpOnly {
		t.Errorf("expected HttpOnly=true, got false")
	}
}

// TestClearCSRFCookie_InsecureByDefault verifies CSRF clear cookies are insecure by default.
func TestClearCSRFCookie_InsecureByDefault(t *testing.T) {
	sm := NewSessionManager("testpass")
	w := httptest.NewRecorder()

	sm.ClearCSRFCookie(w)

	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected 1 cookie, got %d", len(cookies))
	}
	cookie := cookies[0]
	if cookie.Secure {
		t.Errorf("expected Secure=false, got true")
	}
	if cookie.MaxAge != -1 {
		t.Errorf("expected MaxAge=-1, got %d", cookie.MaxAge)
	}
}

// TestClearCSRFCookie_SecureWhenEnabled verifies CSRF clear cookies are secure when enabled.
func TestClearCSRFCookie_SecureWhenEnabled(t *testing.T) {
	sm := NewSessionManager("testpass")
	sm.SetSecureCookies(true)
	w := httptest.NewRecorder()

	sm.ClearCSRFCookie(w)

	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected 1 cookie, got %d", len(cookies))
	}
	cookie := cookies[0]
	if !cookie.Secure {
		t.Errorf("expected Secure=true, got false")
	}
	if cookie.MaxAge != -1 {
		t.Errorf("expected MaxAge=-1, got %d", cookie.MaxAge)
	}
}

// TestSetCookie_PathMatchesConstant verifies the session cookie Path matches sessionCookiePath constant.
func TestSetCookie_PathMatchesConstant(t *testing.T) {
	sm := NewSessionManager("testpass")
	w := httptest.NewRecorder()

	sm.SetCookie(w, "test-token")

	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected 1 cookie, got %d", len(cookies))
	}
	cookie := cookies[0]
	if cookie.Path != sessionCookiePath {
		t.Errorf("expected Path=%q, got %q", sessionCookiePath, cookie.Path)
	}
}

// TestClearCookie_PathMatchesConstant verifies the clear session cookie Path matches sessionCookiePath constant.
func TestClearCookie_PathMatchesConstant(t *testing.T) {
	sm := NewSessionManager("testpass")
	w := httptest.NewRecorder()

	sm.ClearCookie(w)

	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected 1 cookie, got %d", len(cookies))
	}
	cookie := cookies[0]
	if cookie.Path != sessionCookiePath {
		t.Errorf("expected Path=%q, got %q", sessionCookiePath, cookie.Path)
	}
}

// TestSetCSRFCookie_PathMatchesConstant verifies the CSRF cookie Path matches csrfCookiePath constant.
func TestSetCSRFCookie_PathMatchesConstant(t *testing.T) {
	sm := NewSessionManager("testpass")
	w := httptest.NewRecorder()

	sm.SetCSRFCookie(w, "csrf-token")

	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected 1 cookie, got %d", len(cookies))
	}
	cookie := cookies[0]
	if cookie.Path != csrfCookiePath {
		t.Errorf("expected Path=%q, got %q", csrfCookiePath, cookie.Path)
	}
}

// TestClearCSRFCookie_PathMatchesConstant verifies the clear CSRF cookie Path matches csrfCookiePath constant.
func TestClearCSRFCookie_PathMatchesConstant(t *testing.T) {
	sm := NewSessionManager("testpass")
	w := httptest.NewRecorder()

	sm.ClearCSRFCookie(w)

	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected 1 cookie, got %d", len(cookies))
	}
	cookie := cookies[0]
	if cookie.Path != csrfCookiePath {
		t.Errorf("expected Path=%q, got %q", csrfCookiePath, cookie.Path)
	}
}
