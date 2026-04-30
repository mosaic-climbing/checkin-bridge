// Staff /ui/* handlers (login, logout, page shells, search, member
// management). Split out of server.go in PR5; the HTMX fragments these
// pages depend on live in routes_fragments.go.

package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/mosaic-climbing/checkin-bridge/internal/ingest"
	"github.com/mosaic-climbing/checkin-bridge/internal/store"
	"github.com/mosaic-climbing/checkin-bridge/internal/ui"
)
func (s *Server) handleUILogin(w http.ResponseWriter, r *http.Request) {
	peer := s.clientIP(r)
	// Check if IP is locked out before even reading the body
	if s.sessions.IsLockedOut(peer) {
		s.logger.Warn("login attempt from locked-out IP", "ip", peer)
		if r.Header.Get("HX-Request") == "true" {
			w.WriteHeader(http.StatusTooManyRequests)
			ui.RenderFragment(w, `<span class="error show">Too many failed attempts — try again in a few minutes</span>`)
			return
		}
		writeError(w, http.StatusTooManyRequests, "too many failed attempts — try again in a few minutes")
		return
	}

	// Parse password - support both JSON and form-encoded
	var password string
	if r.Header.Get("Content-Type") == "application/x-www-form-urlencoded" {
		r.ParseForm()
		password = r.FormValue("password")
	} else {
		var body struct {
			Password string `json:"password"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		password = body.Password
	}

	if !s.sessions.Authenticate(password, peer) {
		s.logger.Warn("failed staff login attempt", "ip", peer)
		s.audit.Log("login_failed", peer, nil)
		// HTMX response: return error text into the error div
		if r.Header.Get("HX-Request") == "true" {
			w.WriteHeader(http.StatusUnauthorized)
			ui.RenderFragment(w, `<span class="error show">Invalid password</span>`)
			return
		}
		writeError(w, http.StatusUnauthorized, "invalid password")
		return
	}

	token, csrf, err := s.sessions.CreateSession()
	if err != nil {
		if r.Header.Get("HX-Request") == "true" {
			w.WriteHeader(http.StatusInternalServerError)
			ui.RenderFragment(w, `<span class="error show">Session error</span>`)
			return
		}
		writeError(w, http.StatusInternalServerError, "session error")
		return
	}

	s.sessions.SetCookie(w, token)
	s.sessions.SetCSRFCookie(w, csrf)
	s.logger.Info("staff logged in via UI", "ip", peer)
	s.audit.Log("login_success", peer, nil)

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/ui/")
		w.WriteHeader(http.StatusOK)
		return
	}
	writeJSON(w, map[string]any{"success": true})
}

// POST /ui/logout — destroy the session.
func (s *Server) handleUILogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(s.sessions.cookieName); err == nil {
		s.sessions.DestroySession(c.Value)
	}
	s.sessions.ClearCookie(w)
	s.sessions.ClearCSRFCookie(w)
	s.audit.Log("logout", s.clientIP(r), nil)
	writeJSON(w, map[string]any{"success": true})
}

// GET /ui and /ui/ — serve the staff UI dashboard or login.
func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	if s.ui == nil {
		writeError(w, http.StatusServiceUnavailable, "UI not available")
		return
	}
	// Check if user has a valid session
	if s.sessions != nil && s.sessions.GetSessionFromRequest(r) {
		s.ui.ServePage(w, r, "dashboard", s.sessions.CSRFTokenFromRequest(r))
		return
	}
	// No session — show login
	s.ui.ServeLogin(w)
}

// GET /ui/page/{page} — serve a specific UI page.
func (s *Server) handleUIPage(w http.ResponseWriter, r *http.Request) {
	page := r.PathValue("page")
	if s.ui != nil {
		csrf := ""
		if s.sessions != nil {
			csrf = s.sessions.CSRFTokenFromRequest(r)
		}
		s.ui.ServePage(w, r, page, csrf)
	} else {
		writeError(w, http.StatusServiceUnavailable, "UI not available")
	}
}

// ─── Member Management (Staff UI) ────────────────────────────

// GET /directory/search?q=smith — search the local customer directory.
//
// Backed by the `customers_fts` FTS5 virtual table (migration 6). One
// indexed query replaces the previous fan-out of three sequential LIKE
// scans (email, name, last-name) — the planner walks the FTS index in
// O(log N) and BM25-ranks results across name/email/external_id/barcode in
// a single pass. Triggers on `customers` keep the FTS table in sync, so
// search reflects writes immediately.
func (s *Server) handleDirectorySearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeError(w, http.StatusBadRequest, "q parameter required")
		return
	}
	if s.store == nil {
		writeJSON(w, map[string]any{"query": q, "results": []any{}, "count": 0})
		return
	}

	ctx := r.Context()
	results, err := s.store.SearchCustomersFTS(ctx, q, 50)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Annotate each result with cache status so the UI can show whether the
	// customer already has an enrolled NFC card.
	type searchResult struct {
		store.Customer
		InCache     bool   `json:"inCache"`
		CacheNfcUID string `json:"cacheNfcUid,omitempty"`
	}
	annotated := make([]searchResult, len(results))
	for i, rec := range results {
		annotated[i] = searchResult{Customer: rec}
		if member, err := s.store.GetMemberByCustomerID(ctx, rec.RedpointID); err == nil && member != nil {
			annotated[i].InCache = true
			annotated[i].CacheNfcUID = member.NfcUID
		}
	}

	writeJSON(w, map[string]any{
		"query":   q,
		"results": annotated,
		"count":   len(annotated),
	})
}

// DELETE /members/{externalId} — remove a member from the bridge cache.
//
// v0.5.9: response is HTMX-friendly. On success we return an empty 200 body
// plus `HX-Trigger: member-updated` so:
//   - the row-level Remove button (hx-swap="outerHTML swap:0.3s" on closest tr)
//     gets its row replaced with nothing, animating the row out
//   - the detail-panel Remove button (hx-swap="innerHTML" on #member-detail)
//     clears the panel
//   - any element listening for `member-updated` (e.g. the member table
//     container) re-fetches itself
//
// On error we render an AlertFragment so the operator sees what went wrong
// without a raw 500 replacing a table row. The pre-v0.5.9 JSON shape is
// gone: the UI was the only caller, and the v0.5.8 audit found the JSON
// response was landing inside an HTMX swap target as raw text.
func (s *Server) handleRemoveMember(w http.ResponseWriter, r *http.Request) {
	extID := strings.ToUpper(r.PathValue("externalId"))
	if extID == "" {
		ui.RenderFragment(w, ui.AlertFragment(false, "externalId required"))
		return
	}

	if s.store == nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Store not available"))
		return
	}

	existing, err := s.store.GetMemberByNFC(r.Context(), extID)
	if err != nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Lookup failed: "+err.Error()))
		return
	}
	if existing == nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Member not found"))
		return
	}

	if err := s.store.RemoveMember(r.Context(), extID); err != nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Delete failed: "+err.Error()))
		return
	}

	s.logger.Info("member removed via UI", "externalId", extID, "name", existing.FullName())
	s.audit.Log("member_remove", r.RemoteAddr, map[string]any{
		"externalId": extID, "name": existing.FullName(),
	})
	s.htmlCache.Invalidate()

	// HX-Trigger lets the member table (and anything else subscribed)
	// re-fetch without the detail panel needing to know about it.
	w.Header().Set("HX-Trigger", "member-updated")
	w.WriteHeader(http.StatusOK)
}

// GET /ingest/unmatched — get the list of unmatched/conflicted UniFi users from the last ingest.
func (s *Server) handleUnmatched(w http.ResponseWriter, r *http.Request) {
	result, err := s.ingester.Run(r.Context(), true) // always dry run
	if err != nil {
		writeError(w, http.StatusBadGateway, "ingest scan failed: "+err.Error())
		return
	}

	type unmatchedEntry struct {
		UniFiUserID string   `json:"unifiUserId"`
		UniFiName   string   `json:"unifiName"`
		UniFiEmail  string   `json:"unifiEmail"`
		NfcTokens   []string `json:"nfcTokens"`
		Warning     string   `json:"warning"`
		Category    string   `json:"category"` // "no_match" or "multiple_match"
	}

	var entries []unmatchedEntry
	for _, m := range result.Mappings {
		if m.Method != ingest.MatchNone {
			continue
		}
		cat := "no_match"
		if strings.Contains(m.Warning, "multiple") {
			cat = "multiple_match"
		}
		entries = append(entries, unmatchedEntry{
			UniFiUserID: m.UniFiUserID,
			UniFiName:   m.UniFiName,
			UniFiEmail:  m.UniFiEmail,
			NfcTokens:   m.NfcTokens,
			Warning:     m.Warning,
			Category:    cat,
		})
	}

	writeJSON(w, map[string]any{
		"totalUnifi":  result.UniFiUsers,
		"matched":     result.Matched,
		"unmatched":   result.Unmatched,
		"entries":     entries,
	})
}

// ─── UniFi Status Sync ──────────────────────────────────────

// POST /status-sync — trigger a manual sync of Redpoint membership status → UniFi user status.
//
// Runs asynchronously in the supervised background group so that a long sync
// (a matching pass against a large UA-Hub population can take many minutes)
// isn't killed when the triggering HTTP client disconnects. The request
// returns 202 with a pointer to GET /status-sync for progress polling. If a
// sync is already in flight, returns 200 with a "sync already in progress"
// message rather than queueing another one — RunSync enforces the
// single-runner invariant internally too, but returning early here avoids
// logging a bogus "start" event.
