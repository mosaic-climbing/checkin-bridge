// Control-plane handlers (POST /unlock/{doorId}). Reachable only on
// the control listener (cfg.Bridge.ControlBindAddr, default 127.0.0.1)
// so the routes that cause physical-world side effects are pinned to
// the bridge host. Split out of server.go in PR5.

package api

import (
	"net/http"
	"strings"
)
func (s *Server) handleUnlock(w http.ResponseWriter, r *http.Request) {
	doorID := sanitizeID(r.PathValue("doorId"))
	if doorID == "" {
		writeError(w, http.StatusBadRequest, "invalid door ID")
		return
	}
	if err := s.unifi.UnlockDoor(r.Context(), doorID); err != nil {
		writeError(w, http.StatusBadGateway, "unlock failed")
		return
	}
	s.audit.Log("manual_unlock", r.RemoteAddr, map[string]any{"doorId": doorID})
	writeJSON(w, map[string]any{"success": true, "doorId": doorID})
}

// sanitizeID strips path traversal attempts and non-alphanumeric characters
// from resource identifiers. IDs from UniFi/Redpoint are hex/alphanumeric.
func sanitizeID(id string) string {
	// Strip any path components
	if idx := strings.LastIndex(id, "/"); idx >= 0 {
		id = id[idx+1:]
	}
	// Allow only alphanumeric, dash, underscore
	for _, c := range id {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			return ""
		}
	}
	return id
}

// ─── Staff Auth ─────────────────────────────────────────────

// POST /ui/login — staff login, returns a session cookie.
//
// IMPORTANT: per-IP lockout MUST key off s.clientIP(r), not r.RemoteAddr
// directly. Otherwise, with a trusted proxy in front of the bridge, every
// request would appear to come from the proxy's IP and a single failed
// login from any real user would lock out the entire customer base. And
// without a trusted proxy, using s.clientIP(r) still matches the allowlist
// identity so lockout and allowlist can't disagree about "who is this".
