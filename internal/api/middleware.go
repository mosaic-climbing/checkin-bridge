package api

import (
	"crypto/subtle"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// maxRequestBodySize is the maximum size of a JSON request body (1MB).
const maxRequestBodySize = 1 << 20

// SecurityConfig holds all parameters for the security middleware stack.
type SecurityConfig struct {
	AdminAPIKey     string          // Bearer token for admin API endpoints
	Sessions        *SessionManager // session manager for staff UI cookie auth
	AllowedNetworks []*net.IPNet    // if non-empty, only these CIDRs can connect
	// TrustedProxies are the CIDRs the bridge considers trustworthy
	// upstream forwarders. X-Forwarded-For / X-Real-IP are honoured only
	// when r.RemoteAddr's IP falls inside one of these networks. Empty
	// (the default) means "no proxy in front of us" — headers are ignored
	// and r.RemoteAddr is the definitive client identity.
	TrustedProxies []*net.IPNet
	Logger         *slog.Logger
	HTTPS          bool // if true, emit Strict-Transport-Security header
}

// SecurityMiddleware wraps the handler with auth, IP allowlist, and request body limits.
func SecurityMiddleware(cfg SecurityConfig, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// ── IP allowlist ─────────────────────────────────────
		if len(cfg.AllowedNetworks) > 0 {
			clientIP := extractClientIP(r, cfg.TrustedProxies)
			if !isAllowedIP(clientIP, cfg.AllowedNetworks) {
				if cfg.Logger != nil {
					cfg.Logger.Warn("request blocked by IP allowlist",
						"ip", clientIP,
						"path", r.URL.Path,
					)
				}
				// Return 403 with no details (don't leak that the service exists)
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}

		// ── Security headers ────────────────────────────────
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Cache-Control", "no-store")
		if cfg.HTTPS {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}

		// ── Request body size limit ──────────────────────────
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
		}

		// ── Auth: determine which routes are public ──────────
		path := r.URL.Path

		// Health check is always public (monitoring, load balancers)
		if path == "/health" {
			next.ServeHTTP(w, r)
			return
		}

		// Login endpoint and the login page itself are public.
		// The staff UI app (/ui, /ui/) redirects to login if no session — that redirect
		// happens client-side in the JS, so we serve the HTML publicly but the
		// API endpoints behind it are all authenticated.
		if path == "/ui/login" || path == "/ui/logout" {
			next.ServeHTTP(w, r)
			return
		}

		// Staff UI pages and fragments: require session cookie
		if path == "/ui" || path == "/ui/" || strings.HasPrefix(path, "/ui/page/") || strings.HasPrefix(path, "/ui/frag/") {
			if cfg.Sessions != nil && cfg.Sessions.GetSessionFromRequest(r) {
				// Mutating /ui/frag/* routes (door-policy add/delete,
				// unmatched-queue match/skip/defer) need the same CSRF
				// gate as the root-level session path. Prior to this,
				// the /ui/* branch returned immediately after session
				// auth, which meant an attacker page lured to a logged-in
				// staff browser could fire those mutations cross-origin.
				// GETs are pure reads and don't need CSRF.
				if isMutatingMethod(r.Method) {
					if r.Header.Get("X-Requested-With") == "" {
						writeError(w, http.StatusForbidden, "missing X-Requested-With header")
						return
					}
					if cfg.Sessions == nil || !cfg.Sessions.VerifyCSRF(r) {
						if cfg.Logger != nil {
							cfg.Logger.Warn("CSRF check failed",
								"path", r.URL.Path,
								"method", r.Method,
								"ip", extractClientIP(r, cfg.TrustedProxies),
							)
						}
						writeError(w, http.StatusForbidden, "CSRF token mismatch")
						return
					}
				}
				next.ServeHTTP(w, r)
				return
			}
			// For HTMX fragment requests without session, return 401 to trigger redirect
			if r.Header.Get("HX-Request") == "true" {
				w.Header().Set("HX-Redirect", "/ui/")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			// Full page — serve the login page
			next.ServeHTTP(w, r)
			return
		}

		// All other routes require either admin API key OR valid session.
		// config.validate() enforces AdminAPIKey != "" at boot, so the key path
		// is always live; we still gate checkBearerAuth on the key being set so
		// a programmer mistake can never produce an always-match comparison.
		hasAPIKey := cfg.AdminAPIKey != "" && checkBearerAuth(r, cfg.AdminAPIKey)
		hasSession := cfg.Sessions != nil && cfg.Sessions.GetSessionFromRequest(r)

		if !hasAPIKey && !hasSession {
			w.Header().Set("WWW-Authenticate", `Bearer realm="mosaic-bridge"`)
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		// ── CSRF protection for session-authenticated mutations ──
		// Two layers, both required on session+mutating requests:
		//
		//  1. X-Requested-With header. Browsers block cross-origin custom
		//     headers, so this prevents naive CSRF attacks that rely on a
		//     plain HTML form auto-submitting from a malicious page.
		//
		//  2. Double-submit CSRF token. The mosaic_csrf cookie must match the
		//     X-CSRF-Token header (HTMX path) or the `csrf_token` form field,
		//     and both must equal the token bound to this session server-side.
		//     This catches the stronger attack shape where the browser has
		//     already sent the session cookie (e.g. via a subdomain stuffing
		//     trick) but the attacker can't read the CSRF cookie's contents.
		//
		// API key auth is not susceptible (keys aren't sent by browsers), so
		// the CSRF gate applies only to the session-only path.
		if hasSession && !hasAPIKey && isMutatingMethod(r.Method) {
			if r.Header.Get("X-Requested-With") == "" {
				writeError(w, http.StatusForbidden, "missing X-Requested-With header")
				return
			}
			if cfg.Sessions == nil || !cfg.Sessions.VerifyCSRF(r) {
				if cfg.Logger != nil {
					cfg.Logger.Warn("CSRF check failed",
						"path", r.URL.Path,
						"method", r.Method,
						"ip", extractClientIP(r, cfg.TrustedProxies),
					)
				}
				writeError(w, http.StatusForbidden, "CSRF token mismatch")
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

// ControlSecurityConfig is the control-plane subset of SecurityConfig.
// The control plane exists to host privileged operator-initiated routes on
// a separate listener that's normally bound to loopback — it deliberately
// has no /ui paths, no session auth, and no CSRF. Admin API key is the
// only supported credential.
type ControlSecurityConfig struct {
	AdminAPIKey    string
	AllowedNetworks []*net.IPNet
	TrustedProxies []*net.IPNet
	Logger         *slog.Logger
	HTTPS          bool
}

// ControlSecurityMiddleware is the security stack for the control-plane
// http.Server. It is intentionally simpler than SecurityMiddleware: no
// /ui carve-outs, no session cookies, no CSRF. Every request must carry
// the admin Bearer token.
//
// This is belt-and-braces defence on top of the listener binding. cmd/bridge
// binds this handler to 127.0.0.1:ControlPort by default, which is the
// primary defence — an attacker on the gym LAN can't even TCP-connect. The
// middleware ensures that, even if the control listener is exposed wider
// (a misconfiguration, a debugging experiment, a bad reverse-proxy rule),
// a bare request without the admin key still gets rejected. A matching
// IP allowlist applies so operators can further scope who can reach the
// loopback-bound endpoint when fronted by sshd / socat / reverse proxy.
func ControlSecurityMiddleware(cfg ControlSecurityConfig, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(cfg.AllowedNetworks) > 0 {
			clientIP := extractClientIP(r, cfg.TrustedProxies)
			if !isAllowedIP(clientIP, cfg.AllowedNetworks) {
				if cfg.Logger != nil {
					cfg.Logger.Warn("control-plane request blocked by IP allowlist",
						"ip", clientIP,
						"path", r.URL.Path,
					)
				}
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}

		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Cache-Control", "no-store")
		if cfg.HTTPS {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}

		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
		}

		// Admin API key is mandatory. config.validate() enforces AdminAPIKey
		// != "" at boot; the extra guard here prevents a zero-value config
		// from silently producing an always-match comparison.
		if cfg.AdminAPIKey == "" || !checkBearerAuth(r, cfg.AdminAPIKey) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="mosaic-bridge-control"`)
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		next.ServeHTTP(w, r)
	})
}

// checkBearerAuth validates the Authorization: Bearer header only.
// No query string auth — API keys in URLs leak into logs and browser history.
func checkBearerAuth(r *http.Request, apiKey string) bool {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return false
	}
	token := strings.TrimPrefix(auth, "Bearer ")
	return subtle.ConstantTimeCompare([]byte(token), []byte(apiKey)) == 1
}

// extractClientIP returns the effective client IP for policy decisions
// (IP allowlist, per-IP login rate-limit, audit trails).
//
// Trust model:
//   - If trustedProxies is empty OR r.RemoteAddr is not in that set, the
//     caller is the real peer: we return r.RemoteAddr's IP and ignore
//     X-Forwarded-For / X-Real-IP entirely. This is the correct stance
//     when nothing proxies the bridge — any XFF header seen here is
//     attacker-supplied by definition.
//   - If r.RemoteAddr IS a trusted proxy, we walk X-Forwarded-For right-
//     to-left (RFC 7239 §5.2 "forwarded element ordering") and return
//     the first IP that is NOT itself in the trusted set. That's the
//     "last untrusted hop" — the real client as seen by the last trusted
//     forwarder. Walking from the right rather than the left matters:
//     an attacker who controls the pre-proxy side can inject arbitrary
//     left-most entries ("X-Forwarded-For: 10.0.0.1, real-attacker-ip"),
//     and those entries would be taken as truth by the naive first-
//     element read the code used to do.
//   - If the XFF chain is all trusted (or missing), fall back to
//     X-Real-IP, then r.RemoteAddr.
//
// Return value is always an IP string without port.
func extractClientIP(r *http.Request, trustedProxies []*net.IPNet) string {
	remoteHost := remoteIPHost(r.RemoteAddr)

	// If no trusted proxies configured, or the peer itself isn't a
	// trusted proxy, don't trust any forwarding header. Using XFF from
	// an untrusted peer would let an attacker spoof an allowlisted IP.
	if len(trustedProxies) == 0 || !isAllowedIP(remoteHost, trustedProxies) {
		return remoteHost
	}

	// RemoteAddr is a trusted proxy. Honour the forwarding headers.
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		// Walk right-to-left: the rightmost entries are the ones the
		// immediate (trusted) proxy added; earlier entries may be
		// attacker-controlled upstream forgeries.
		for i := len(parts) - 1; i >= 0; i-- {
			candidate := strings.TrimSpace(parts[i])
			if candidate == "" {
				continue
			}
			if !isAllowedIP(candidate, trustedProxies) {
				return candidate
			}
		}
		// Every entry in XFF was itself a trusted proxy. That's
		// unusual but legitimate in a chain-of-trust fronting setup;
		// fall through to X-Real-IP / RemoteAddr.
	}

	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return strings.TrimSpace(xri)
	}
	return remoteHost
}

// remoteIPHost strips the :port suffix off r.RemoteAddr. Falls back to
// the raw string when there's no port (unix sockets, malformed input),
// which keeps the allowlist/logging paths from emitting "" on edge
// cases that otherwise wouldn't reach this code.
func remoteIPHost(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

// isAllowedIP checks whether the client IP falls within any of the allowed CIDRs.
func isAllowedIP(ipStr string, allowedNets []*net.IPNet) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	for _, n := range allowedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// ParseAllowedNetworks parses a comma-separated list of CIDRs into net.IPNet slices.
// Supports individual IPs ("10.0.1.5" → "10.0.1.5/32") and CIDR notation ("10.0.1.0/24").
// Returns nil if the input is empty (meaning all IPs are allowed).
func ParseAllowedNetworks(raw string) ([]*net.IPNet, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	var nets []*net.IPNet
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		// If it's just an IP (no /mask), make it a /32 or /128
		if !strings.Contains(entry, "/") {
			ip := net.ParseIP(entry)
			if ip == nil {
				return nil, &net.ParseError{Type: "IP address", Text: entry}
			}
			if ip.To4() != nil {
				entry += "/32"
			} else {
				entry += "/128"
			}
		}

		_, ipNet, err := net.ParseCIDR(entry)
		if err != nil {
			return nil, err
		}
		nets = append(nets, ipNet)
	}
	return nets, nil
}

// isMutatingMethod returns true for HTTP methods that modify state.
func isMutatingMethod(method string) bool {
	return method == "POST" || method == "PUT" || method == "PATCH" || method == "DELETE"
}

// RecoveryMiddleware catches panics in handlers and returns a 500 instead of crashing.
func RecoveryMiddleware(logger interface{ Error(msg string, args ...any) }, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				logger.Error("handler panic recovered", "panic", rec, "path", r.URL.Path)
				writeError(w, http.StatusInternalServerError, "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// RequestLogger logs all incoming requests with timing.
//
// trustedProxies mirrors SecurityConfig.TrustedProxies so the "ip" field
// in access logs matches the identity used for allowlist / rate-limit
// decisions downstream. Pass nil when the bridge is deployed without a
// proxy in front.
func RequestLogger(logger interface {
	Info(msg string, args ...any)
}, trustedProxies []*net.IPNet, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		logger.Info("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"ip", extractClientIP(r, trustedProxies),
			"duration", time.Since(start).Round(time.Microsecond),
		)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (sw *statusWriter) WriteHeader(code int) {
	sw.status = code
	sw.ResponseWriter.WriteHeader(code)
}
