package api

import (
	"container/list"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// SessionManager handles staff login sessions with signed HTTP-only cookies.
type SessionManager struct {
	mu            sync.RWMutex
	sessions      map[string]sessionEntry // token → entry
	passwordHash  []byte                  // bcrypt hash of staff password
	signingKey    []byte                  // HMAC key for signing session tokens
	sessionMaxAge time.Duration
	cookieName    string
	csrfCookieName string
	secureCookies bool                    // if true, set Secure flag on all cookies

	// Per-IP login rate limiting. The map + list pair implement a bounded LRU:
	//   loginAttempts: ip → *list.Element (so we can O(1) locate the element).
	//   loginLRU:      doubly-linked list of *loginTracker, newest at Front.
	// Authenticate touches the entry (MoveToFront). When the map grows past
	// maxLoginAttemptEntries, evictOneLocked walks from Back looking for the
	// oldest *non-locked* entry and removes it; currently-locked entries are
	// preserved unconditionally so an attacker can't force their own lockout
	// to be evicted by flooding unique source IPs.
	loginMu       sync.Mutex
	loginAttempts map[string]*list.Element
	loginLRU      *list.List

	// asyncWG tracks the background janitor goroutine. Shutdown() blocks on
	// this so the janitor exits cleanly before the process returns.
	asyncWG sync.WaitGroup
}

// sessionEntry stores per-session state. The CSRF token is bound to the
// session so rotating the session (re-login) also rotates CSRF, and so we
// can do a double-submit compare without a separate CSRF store.
type sessionEntry struct {
	createdAt time.Time
	csrfToken string
}

type loginTracker struct {
	ip          string // map key, carried on the list node so we can delete on eviction
	failures    int
	lastTry     time.Time
	lockedUntil time.Time
}

const (
	maxLoginFailures  = 5                // lock out after 5 failures
	loginLockDuration = 5 * time.Minute  // lock duration after max failures
	loginWindowReset  = 15 * time.Minute // reset failure count after this idle period

	// sessionTokenVersion is the prefix carried on every issued session token.
	// Format: "<version>|<raw-hex>.<hmac-hex>". The prefix exists so a future
	// signature-algorithm change can be rolled out without silently validating
	// stale tokens — bumping the version string invalidates the whole cookie
	// set cleanly. v2 uses full 64-hex-char SHA-256 HMAC; v1 (which used a
	// 16-hex truncated HMAC) is intentionally not accepted — S3 in the review.
	sessionTokenVersion = "v2"
	sessionTokenPrefix  = sessionTokenVersion + "|"

	// sessionKeySize is the HMAC key length in bytes. 32 = 256 bits, matching
	// SHA-256's block input; anything shorter gives HMAC a weaker lower bound.
	sessionKeySize = 32

	// S8: Cookie path scoping — DEFER narrowing to /ui
	//
	// The UI serves authenticated HTMX-triggered requests to both /ui/* and root-level
	// paths. Five call sites from authenticated UI pages target root-level endpoints:
	//   - hx-post="/cache/sync" (sync.html:10)
	//   - hx-post="/status-sync" (sync.html:21)
	//   - hx-post="/directory/sync" (sync.html:32)
	//   - hx-post="/ingest/unifi?dry_run=true" (sync.html:43)
	//   - hx-delete="/members/%s" (fragments.go — row-level Remove button)
	//
	// Narrowing Path from "/" to "/ui" would prevent these six calls from sending the
	// session cookie, causing silent auth failures. The fix is not cookie-path scoping,
	// but a future refactor that moves all authenticated endpoints under /ui/* (e.g.,
	// /ui/cache/sync instead of /cache/sync). Once that refactor completes, this constant
	// can be set to "/ui" and the cookie scope becomes a strict security improvement.
	//
	// Until then, Path="/" is correct. The constant exists for that future refactor and
	// for the path-assertion tests (TestSetCookie_PathMatchesConstant, et al.) which will
	// guard against accidental narrowing that breaks auth.
	sessionCookiePath = "/"
	csrfCookiePath    = "/"
)

// The following are var rather than const so integration tests can tune the
// sweep cadence and eviction cap without running a production-sized workload.
// Not exposed in any public API; production code never mutates them.
var (
	maxLoginAttemptEntries = 10_000          // hard cap on the tracker map
	loginJanitorInterval   = 1 * time.Minute // how often the janitor sweeps stale entries

	// loginStaleAge is the age beyond which an unlocked tracker is safe to sweep.
	// Set to loginWindowReset*2 so a sweep can't race with a brand-new attempt
	// that just reset its own counter; the extra window gives the attempt time
	// to complete its bcrypt + MoveToFront cycle before we'd consider it stale.
	loginStaleAge = 2 * loginWindowReset
)

// NewSessionManager creates a session manager with an ephemeral (in-memory)
// signing key. Every restart generates a new key, so all active staff
// sessions are invalidated. Intended for tests and for the rare deployment
// that explicitly does not want persistent sessions; production code should
// call NewSessionManagerWithKeyFile so staff don't get logged out on every
// restart (S4 in docs/architecture-review.md).
func NewSessionManager(staffPassword string) *SessionManager {
	key := make([]byte, sessionKeySize)
	if _, err := rand.Read(key); err != nil {
		// crypto/rand.Read never fails on any supported platform; treat a
		// failure as catastrophic rather than silently carrying a zero key.
		panic("session: crypto/rand failed: " + err.Error())
	}
	return newSessionManagerWithKey(staffPassword, key)
}

// NewSessionManagerWithKeyFile is the production constructor. On first boot
// it generates a 32-byte HMAC key, writes it atomically (tmp + rename) to
// keyPath with mode 0600, and uses it to sign session tokens. On subsequent
// boots it reads the existing key and reuses it so active staff sessions
// survive restarts. The parent directory is created with mode 0700 if it
// doesn't already exist.
//
// Returns an error if the key file exists but is an unexpected length — we
// never want to silently truncate or zero-pad an HMAC key.
func NewSessionManagerWithKeyFile(staffPassword, keyPath string) (*SessionManager, error) {
	key, err := loadOrCreateSigningKey(keyPath)
	if err != nil {
		return nil, err
	}
	return newSessionManagerWithKey(staffPassword, key), nil
}

func newSessionManagerWithKey(staffPassword string, key []byte) *SessionManager {
	// bcrypt the staff password — cost 12 is ~250ms on modern hardware
	hash, err := bcrypt.GenerateFromPassword([]byte(staffPassword), 12)
	if err != nil {
		// Should never fail for a reasonable password
		panic("bcrypt hash failed: " + err.Error())
	}

	return &SessionManager{
		sessions:       make(map[string]sessionEntry),
		passwordHash:   hash,
		signingKey:     key,
		sessionMaxAge:  12 * time.Hour,
		cookieName:     "mosaic_session",
		csrfCookieName: "mosaic_csrf",
		loginAttempts:  make(map[string]*list.Element),
		loginLRU:       list.New(),
	}
}

// loadOrCreateSigningKey returns the HMAC signing key at path, creating and
// persisting a fresh one if the file doesn't exist. Writes are atomic (tmp
// file in the same directory + rename) so a crash mid-write can't leave a
// half-written key on disk, and permissions are pinned to 0600 on both
// create and reuse paths. If the file exists but is the wrong size, we
// return an error rather than silently continuing — that state usually
// means manual tampering or disk corruption and should block startup.
func loadOrCreateSigningKey(path string) ([]byte, error) {
	if data, err := os.ReadFile(path); err == nil {
		if len(data) != sessionKeySize {
			return nil, fmt.Errorf("session key at %s has invalid length %d (expected %d)", path, len(data), sessionKeySize)
		}
		// Tighten perms in case a previous incarnation or an operator
		// created the file with a wider mode. Non-fatal if it fails —
		// chmod on a readable file failing is unusual enough to flag,
		// but not worth refusing to start over.
		if err := os.Chmod(path, 0o600); err != nil && !errors.Is(err, os.ErrPermission) {
			return nil, fmt.Errorf("chmod session key %s: %w", path, err)
		}
		return data, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read session key %s: %w", path, err)
	}

	// File didn't exist — generate and persist.
	key := make([]byte, sessionKeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate session key: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir session key dir %s: %w", dir, err)
	}

	// Atomic write via tmp + rename. The tmp file lives in the same dir so
	// rename is a same-filesystem operation (atomic by POSIX).
	tmp, err := os.CreateTemp(dir, ".session.key.tmp-*")
	if err != nil {
		return nil, fmt.Errorf("create temp session key in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	// Defense-in-depth: make sure the tmp file goes away on any failure
	// before rename. A leftover tmp file is harmless (ignored on read)
	// but clutter is clutter.
	cleanup := func() { _ = os.Remove(tmpPath) }

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return nil, fmt.Errorf("chmod temp session key: %w", err)
	}
	if _, err := tmp.Write(key); err != nil {
		_ = tmp.Close()
		cleanup()
		return nil, fmt.Errorf("write session key: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return nil, fmt.Errorf("fsync session key: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return nil, fmt.Errorf("close temp session key: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return nil, fmt.Errorf("rename session key %s -> %s: %w", tmpPath, path, err)
	}
	return key, nil
}

// StartJanitor launches the background sweep goroutine. Call once, early in
// main.go. The goroutine runs until ctx is cancelled; Shutdown(ctx) will then
// block until the goroutine has fully returned.
func (sm *SessionManager) StartJanitor(ctx context.Context) {
	sm.asyncWG.Add(1)
	go sm.janitor(ctx)
}

// SetSecureCookies controls whether session/CSRF cookies are set with the
// Secure flag. Call this once at construction time when the bridge is
// deployed behind TLS (Bridge.HTTPS=true). Safe to leave false for LAN/HTTP
// deployments.
func (sm *SessionManager) SetSecureCookies(b bool) {
	sm.secureCookies = b
}

// Shutdown waits for the janitor goroutine to exit. It does not cancel any
// context on its own — the caller is expected to cancel the context first
// (typically via the main.go root-ctx cancel()), then call Shutdown to block
// until the janitor has returned. Returns ctx.Err() if the wait times out.
func (sm *SessionManager) Shutdown(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		sm.asyncWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (sm *SessionManager) janitor(ctx context.Context) {
	defer sm.asyncWG.Done()
	t := time.NewTicker(loginJanitorInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sm.sweepLoginAttempts(time.Now())
		}
	}
}

// sweepLoginAttempts removes stale non-locked tracker entries from the LRU.
// Walks from Back (oldest) toward Front (newest), skipping currently-locked
// entries and stopping as soon as we hit a fresh entry (LRU ordering guarantees
// everything newer is also fresh). Returns the number of entries removed —
// exposed for tests.
func (sm *SessionManager) sweepLoginAttempts(now time.Time) int {
	staleCutoff := now.Add(-loginStaleAge)
	removed := 0
	sm.loginMu.Lock()
	defer sm.loginMu.Unlock()
	e := sm.loginLRU.Back()
	for e != nil {
		t := e.Value.(*loginTracker)
		prev := e.Prev()
		// Preserve a currently-active lockout regardless of age; the rate
		// limiter is worth keeping even at the cost of a little retained
		// memory. Expired lockouts (lockedUntil != 0 but in the past) are
		// treated like any other stale entry and fall through to the age check.
		if !t.lockedUntil.IsZero() && now.Before(t.lockedUntil) {
			e = prev
			continue
		}
		if !t.lastTry.Before(staleCutoff) {
			break // newer than cutoff → so is everything ahead of us in LRU order
		}
		sm.loginLRU.Remove(e)
		delete(sm.loginAttempts, t.ip)
		removed++
		e = prev
	}
	return removed
}

// evictOneLocked removes the oldest non-locked entry to make room for a new
// one. Caller must hold loginMu. Returns true if an entry was evicted, false
// if every entry is currently locked (rare pathological case — we let the
// map grow past cap rather than drop a live lockout, since that would let an
// attacker reset their own rate limit by flooding unique source IPs).
func (sm *SessionManager) evictOneLocked() bool {
	now := time.Now()
	for e := sm.loginLRU.Back(); e != nil; e = e.Prev() {
		t := e.Value.(*loginTracker)
		if !t.lockedUntil.IsZero() && now.Before(t.lockedUntil) {
			continue
		}
		sm.loginLRU.Remove(e)
		delete(sm.loginAttempts, t.ip)
		return true
	}
	return false
}

// Authenticate checks whether the staff password is correct using bcrypt.
// Returns false immediately if the IP is currently locked out.
func (sm *SessionManager) Authenticate(password, remoteIP string) bool {
	ip := normalizeIP(remoteIP)

	sm.loginMu.Lock()
	tracker := sm.touchTrackerLocked(ip)

	// Check lockout
	if !tracker.lockedUntil.IsZero() && time.Now().Before(tracker.lockedUntil) {
		sm.loginMu.Unlock()
		return false
	}

	// Reset counter if it's been quiet long enough
	if time.Since(tracker.lastTry) > loginWindowReset {
		tracker.failures = 0
	}
	tracker.lastTry = time.Now()
	sm.loginMu.Unlock()

	// bcrypt compare (constant-time internally)
	err := bcrypt.CompareHashAndPassword(sm.passwordHash, []byte(password))
	if err != nil {
		// Failed — record failure
		sm.loginMu.Lock()
		tracker.failures++
		if tracker.failures >= maxLoginFailures {
			tracker.lockedUntil = time.Now().Add(loginLockDuration)
		}
		sm.loginMu.Unlock()
		return false
	}

	// Success — reset failures
	sm.loginMu.Lock()
	tracker.failures = 0
	tracker.lockedUntil = time.Time{}
	sm.loginMu.Unlock()
	return true
}

// IsLockedOut returns true if the IP is currently locked out from login attempts.
// This is a read-only check; it intentionally does NOT bump the LRU, so a
// probing attacker can't keep their own lockout entry alive forever by hitting
// this endpoint — the entry drifts back toward the janitor's sweep horizon
// only via Authenticate calls.
func (sm *SessionManager) IsLockedOut(remoteIP string) bool {
	ip := normalizeIP(remoteIP)
	sm.loginMu.Lock()
	defer sm.loginMu.Unlock()
	elem, ok := sm.loginAttempts[ip]
	if !ok {
		return false
	}
	tracker := elem.Value.(*loginTracker)
	return !tracker.lockedUntil.IsZero() && time.Now().Before(tracker.lockedUntil)
}

// touchTrackerLocked returns the tracker for ip, creating one and pushing it
// to the LRU front if necessary. Evicts the oldest non-locked entry when the
// map is at capacity. Caller must hold loginMu.
func (sm *SessionManager) touchTrackerLocked(ip string) *loginTracker {
	if elem, ok := sm.loginAttempts[ip]; ok {
		sm.loginLRU.MoveToFront(elem)
		return elem.Value.(*loginTracker)
	}
	if sm.loginLRU.Len() >= maxLoginAttemptEntries {
		sm.evictOneLocked()
	}
	t := &loginTracker{ip: ip}
	elem := sm.loginLRU.PushFront(t)
	sm.loginAttempts[ip] = elem
	return t
}

// CreateSession generates a new session token and a bound CSRF token.
// Both are returned; the caller is responsible for setting both cookies.
func (sm *SessionManager) CreateSession() (sessionToken, csrfToken string, err error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("generate token: %w", err)
	}
	raw := hex.EncodeToString(b)

	// HMAC sign the session token. The version prefix (sessionTokenPrefix) is
	// embedded in the issued token so ValidateSession can reject any token
	// that doesn't carry the current version string. This gives us a clean
	// cut-over if we ever change the signing algorithm again.
	sig := sm.signToken(raw)
	sessionToken = sessionTokenPrefix + raw + "." + sig

	// Generate an independent CSRF token (32 hex chars / 16 bytes entropy).
	// Not HMAC-signed — its value is the secret, and we double-submit-compare
	// it against the X-CSRF-Token header on mutating requests.
	csrfBytes := make([]byte, 16)
	if _, err := rand.Read(csrfBytes); err != nil {
		return "", "", fmt.Errorf("generate csrf token: %w", err)
	}
	csrfToken = hex.EncodeToString(csrfBytes)

	sm.mu.Lock()
	sm.sessions[sessionToken] = sessionEntry{
		createdAt: time.Now(),
		csrfToken: csrfToken,
	}
	sm.mu.Unlock()

	// Clean expired sessions periodically
	go sm.cleanup()

	return sessionToken, csrfToken, nil
}

// CSRFTokenFor returns the CSRF token bound to a session, or "" if the session
// is unknown or expired. Used by the UI layer to inject the token into
// server-rendered HTML so HTMX can echo it back in the X-CSRF-Token header.
func (sm *SessionManager) CSRFTokenFor(sessionToken string) string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	entry, ok := sm.sessions[sessionToken]
	if !ok {
		return ""
	}
	if time.Since(entry.createdAt) > sm.sessionMaxAge {
		return ""
	}
	return entry.csrfToken
}

// CSRFTokenFromRequest returns the CSRF token bound to the request's session,
// or "" if the request has no valid session.
func (sm *SessionManager) CSRFTokenFromRequest(r *http.Request) string {
	c, err := r.Cookie(sm.cookieName)
	if err != nil {
		return ""
	}
	return sm.CSRFTokenFor(c.Value)
}

// VerifyCSRF implements the double-submit check: the mosaic_csrf cookie must
// match the X-CSRF-Token request header (or the `csrf_token` form field for
// non-HTMX form posts), AND both must equal the token bound to the session.
//
// The three-way check prevents two attack shapes:
//   - pure CSRF via form post: attacker can't set a custom header, and can't
//     read our HttpOnly cookie to stuff the form field.
//   - cookie-only match: an attacker who plants a cookie via subdomain (rare
//     on a LAN) still can't read the server-side binding, so their cookie
//     value won't match the stored csrfToken.
func (sm *SessionManager) VerifyCSRF(r *http.Request) bool {
	cookie, err := r.Cookie(sm.csrfCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}

	// Accept either header (HTMX path) or form field (plain HTML form fallback).
	submitted := r.Header.Get("X-CSRF-Token")
	if submitted == "" {
		// Parse form for the fallback. This is cheap and safe — the body size
		// limit middleware already caps the request body.
		if r.ParseForm() == nil {
			submitted = r.Form.Get("csrf_token")
		}
	}
	if submitted == "" {
		return false
	}

	// All three must agree. Constant-time equality on the two hex strings.
	expected := sm.CSRFTokenFromRequest(r)
	if expected == "" {
		return false
	}
	if !hmac.Equal([]byte(cookie.Value), []byte(expected)) {
		return false
	}
	if !hmac.Equal([]byte(submitted), []byte(expected)) {
		return false
	}
	return true
}

// ValidateSession checks if a session token is valid:
// 0. Token carries the current sessionTokenPrefix (rejects legacy formats).
// 1. HMAC signature is correct (prevents forged tokens).
// 2. Token exists in the session store.
// 3. Session has not expired.
func (sm *SessionManager) ValidateSession(token string) bool {
	// Step 0: Reject any token missing the current version prefix. This
	// covers v1 tokens (16-hex truncated signature) minted before S3 landed,
	// which should fail cleanly rather than pass a weaker signature check.
	if !strings.HasPrefix(token, sessionTokenPrefix) {
		return false
	}
	body := token[len(sessionTokenPrefix):]

	// Step 1: Verify HMAC signature
	parts := strings.SplitN(body, ".", 2)
	if len(parts) != 2 {
		return false
	}
	raw, providedSig := parts[0], parts[1]
	expectedSig := sm.signToken(raw)
	if !hmac.Equal([]byte(providedSig), []byte(expectedSig)) {
		return false
	}

	// Step 2: Check session store
	sm.mu.RLock()
	entry, ok := sm.sessions[token]
	sm.mu.RUnlock()
	if !ok {
		return false
	}

	// Step 3: Check expiry
	if time.Since(entry.createdAt) > sm.sessionMaxAge {
		sm.mu.Lock()
		delete(sm.sessions, token)
		sm.mu.Unlock()
		return false
	}
	return true
}

// DestroySession removes a session.
func (sm *SessionManager) DestroySession(token string) {
	sm.mu.Lock()
	delete(sm.sessions, token)
	sm.mu.Unlock()
}

// SetCookie writes the session cookie to the response.
func (sm *SessionManager) SetCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sm.cookieName,
		Value:    token,
		Path:     sessionCookiePath,
		MaxAge:   int(sm.sessionMaxAge.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   sm.secureCookies,
	})
}

// ClearCookie removes the session cookie.
func (sm *SessionManager) ClearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sm.cookieName,
		Value:    "",
		Path:     sessionCookiePath,
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   sm.secureCookies,
	})
}

// S9: CSRF double-submit with HttpOnly cookie
//
// This code uses a double-submit CSRF pattern: the CSRF token is set in a cookie AND
// echoed via an X-CSRF-Token request header; the server compares them at line ~XXX.
// The cookie is HttpOnly: true even though "classic" double-submit designs assume JS
// can read the cookie. This is safe here: the server injects the CSRF token directly
// into rendered HTML at render time, so client-side HTMX code reads it from the DOM,
// not from document.cookie. Keeping HttpOnly is a strict security improvement over the
// classic pattern — it prevents CSS-exfil and DOM-scraping attacks on the cookie itself.
// The session cookie is separately HttpOnly and server-bound; an XSS on /ui cannot
// exfiltrate the session because the browser blocks script-side reads of HttpOnly cookies.
// Do not "fix" this by removing HttpOnly from the CSRF cookie.
func (sm *SessionManager) SetCSRFCookie(w http.ResponseWriter, csrfToken string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sm.csrfCookieName,
		Value:    csrfToken,
		Path:     csrfCookiePath,
		MaxAge:   int(sm.sessionMaxAge.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   sm.secureCookies,
	})
}

// ClearCSRFCookie removes the CSRF cookie.
func (sm *SessionManager) ClearCSRFCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sm.csrfCookieName,
		Value:    "",
		Path:     csrfCookiePath,
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   sm.secureCookies,
	})
}

// GetSessionFromRequest reads and validates the session cookie.
func (sm *SessionManager) GetSessionFromRequest(r *http.Request) bool {
	c, err := r.Cookie(sm.cookieName)
	if err != nil {
		return false
	}
	return sm.ValidateSession(c.Value)
}

// signToken produces the full-length hex HMAC-SHA256 signature for a raw
// token value. Previously truncated to 16 hex chars (64 bits); that was enough
// to be impractical over HTTP but still below the 128-bit comfort line, and
// truncation saved nothing — 64 hex chars on a cookie is 48 extra bytes.
// Callers must always compare signatures through hmac.Equal.
func (sm *SessionManager) signToken(raw string) string {
	mac := hmac.New(sha256.New, sm.signingKey)
	mac.Write([]byte(raw))
	return hex.EncodeToString(mac.Sum(nil))
}

func (sm *SessionManager) cleanup() {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	now := time.Now()
	for token, entry := range sm.sessions {
		if now.Sub(entry.createdAt) > sm.sessionMaxAge {
			delete(sm.sessions, token)
		}
	}
}

// normalizeIP strips the port from a remote address (e.g. "10.0.1.5:54321" → "10.0.1.5").
func normalizeIP(remoteAddr string) string {
	if idx := strings.LastIndex(remoteAddr, ":"); idx != -1 {
		// Check if it's an IPv6 address in brackets
		if strings.Contains(remoteAddr, "]") {
			// [::1]:8080 → [::1]
			if bracketEnd := strings.Index(remoteAddr, "]"); bracketEnd != -1 {
				return remoteAddr[1:bracketEnd]
			}
		}
		return remoteAddr[:idx]
	}
	return remoteAddr
}
