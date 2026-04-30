// HTMX fragment handlers and the "Needs Match" staff workflow that
// powers /ui/needs-match. Split out of server.go in PR5 — the
// fragments are a coherent group with their own helper set
// (splitCandidates, badgeNameFor, etc.) and stripping them from
// server.go drops that file from 2661 lines to ~1800.

package api

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mosaic-climbing/checkin-bridge/internal/ingest"
	"github.com/mosaic-climbing/checkin-bridge/internal/redpoint"
	"github.com/mosaic-climbing/checkin-bridge/internal/statusync"
	"github.com/mosaic-climbing/checkin-bridge/internal/store"
	"github.com/mosaic-climbing/checkin-bridge/internal/ui"
)
// ─── HTMX Fragment Handlers ──────────────────────────────────

func (s *Server) handleFragStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var active, total, allowedToday, deniedToday int

	if s.store != nil {
		memberStats, _ := s.store.MemberStats(ctx)
		checkinStats, _ := s.store.CheckInStats(ctx)

		if memberStats != nil {
			active = memberStats.Active
			total = memberStats.Total
		}
		if checkinStats != nil {
			allowedToday = checkinStats.AllowedToday
			deniedToday = checkinStats.DeniedToday
		}
	}

	html := ui.StatsFragment(active, total, allowedToday, deniedToday, s.unifi.Connected())
	ui.RenderFragment(w, html)
}

func (s *Server) handleFragRecentCheckins(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		ui.RenderFragment(w, "")
		return
	}
	ctx := r.Context()
	events, _ := s.store.RecentCheckIns(ctx, 20)

	rows := make([]ui.CheckInRow, len(events))
	for i, e := range events {
		name := e.CustomerName
		if name == "" {
			name = "Unknown"
		}
		// Format time to HH:MM:SS
		t := e.Timestamp
		if len(t) > 19 {
			t = t[11:19]
		}
		rows[i] = ui.CheckInRow{
			Time: t, Name: name, NfcUID: e.NfcUID,
			Door: e.DoorName, Result: e.Result, DenyReason: e.DenyReason,
		}
	}
	ui.RenderFragment(w, ui.CheckInTableFragment(rows))
}

// intParam is a small helper to extract integer query parameters.
func intParam(r *http.Request, name string, def int) int {
	if v := r.URL.Query().Get(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func (s *Server) handleFragMemberTable(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		ui.RenderFragment(w, "")
		return
	}
	limit := intParam(r, "limit", 50)
	offset := intParam(r, "offset", 0)
	if limit > 200 {
		limit = 200 // hard cap to prevent abuse
	}

	key := fmt.Sprintf("frag-member-table:offset=%d:limit=%d", offset, limit)
	if body, hit := s.htmlCache.Get(key); hit {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "private, max-age=5")
		w.Write(body)
		return
	}

	ctx := r.Context()
	members, total, err := s.store.AllMembersPaged(ctx, limit, offset)
	if err != nil {
		s.logger.Warn("AllMembersPaged failed", "error", err)
		ui.RenderFragment(w, ui.AlertFragment(false, "Failed to load members"))
		return
	}

	rows := make([]ui.MemberRow, len(members))
	for i, m := range members {
		rows[i] = ui.MemberRow{
			NfcUID: m.NfcUID, Name: m.FullName(),
			BadgeStatus: m.BadgeStatus, BadgeName: m.BadgeName,
			LastCheckIn: m.LastCheckIn, CustomerID: m.CustomerID,
		}
	}
	html := ui.MemberTableFragmentPaged(rows, offset, total)
	s.htmlCache.Set(key, []byte(html), 30*time.Second)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, max-age=5")
	w.Write([]byte(html))
}

func (s *Server) handleFragSearchResults(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if len(q) < 2 {
		ui.RenderFragment(w, "")
		return
	}

	if s.store == nil {
		ui.RenderFragment(w, "")
		return
	}

	ctx := r.Context()
	var results []ui.SearchResult

	if strings.Contains(q, "@") {
		cust, err := s.store.SearchCustomersByEmail(ctx, q)
		if err == nil && cust != nil {
			member, _ := s.store.GetMemberByCustomerID(ctx, cust.RedpointID)
			sr := ui.SearchResult{
				RedpointID: cust.RedpointID,
				Name:       cust.FirstName + " " + cust.LastName,
				Email:      cust.Email,
			}
			if member != nil {
				sr.InCache = true
				sr.NfcUID = member.NfcUID
			}
			results = append(results, sr)
		}
	} else {
		parts := strings.Fields(q)
		var first, last string
		if len(parts) >= 2 {
			first, last = parts[0], parts[len(parts)-1]
		} else if len(parts) == 1 {
			first = parts[0]
		}

		customers, err := s.store.SearchCustomersByName(ctx, first, last)
		if err == nil {
			for _, c := range customers {
				member, _ := s.store.GetMemberByCustomerID(ctx, c.RedpointID)
				sr := ui.SearchResult{
					RedpointID: c.RedpointID,
					Name:       c.FirstName + " " + c.LastName,
					Email:      c.Email,
				}
				if member != nil {
					sr.InCache = true
					sr.NfcUID = member.NfcUID
				}
				results = append(results, sr)
			}
		}

		// Also try last name search for single-word queries
		if len(parts) == 1 {
			more, err := s.store.SearchCustomersByLastName(ctx, first)
			if err == nil {
				existing := make(map[string]bool)
				for _, r := range results {
					existing[r.RedpointID] = true
				}
				for _, c := range more {
					if existing[c.RedpointID] {
						continue
					}
					member, _ := s.store.GetMemberByCustomerID(ctx, c.RedpointID)
					sr := ui.SearchResult{
						RedpointID: c.RedpointID,
						Name:       c.FirstName + " " + c.LastName,
						Email:      c.Email,
					}
					if member != nil {
						sr.InCache = true
						sr.NfcUID = member.NfcUID
					}
					results = append(results, sr)
				}
			}
		}
	}

	ui.RenderFragment(w, ui.SearchResultsFragment(results))
}

func (s *Server) handleFragCheckinStats(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		ui.RenderFragment(w, "")
		return
	}
	stats, _ := s.store.CheckInStats(r.Context())
	if stats == nil {
		ui.RenderFragment(w, "")
		return
	}
	html := fmt.Sprintf(`<div class="stats-grid">
        <div class="stat-card"><div class="stat-value">%d</div><div class="stat-label">Total Today</div></div>
        <div class="stat-card"><div class="stat-value">%d</div><div class="stat-label">Allowed</div></div>
        <div class="stat-card"><div class="stat-value">%d</div><div class="stat-label">Denied</div></div>
        <div class="stat-card"><div class="stat-value">%d</div><div class="stat-label">Unique Members</div></div>
        <div class="stat-card"><div class="stat-value">%d</div><div class="stat-label">All Time</div></div>
    </div>`, stats.TotalToday, stats.AllowedToday, stats.DeniedToday, stats.UniqueToday, stats.TotalAllTime)
	ui.RenderFragment(w, html)
}

func (s *Server) handleFragCheckinTable(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		ui.RenderFragment(w, "")
		return
	}
	events, _ := s.store.RecentCheckIns(r.Context(), 50)
	rows := make([]ui.CheckInRow, len(events))
	for i, e := range events {
		name := e.CustomerName
		if name == "" {
			name = "Unknown"
		}
		t := e.Timestamp
		if len(t) > 19 {
			t = t[11:19]
		}
		rows[i] = ui.CheckInRow{
			Time: t, Name: name, NfcUID: e.NfcUID,
			Door: e.DoorName, Result: e.Result, DenyReason: e.DenyReason,
		}
	}
	ui.RenderFragment(w, ui.CheckInTableFragment(rows))
}

func (s *Server) handleFragJobTable(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		ui.RenderFragment(w, "")
		return
	}
	jobs, _ := s.store.RecentJobs(r.Context(), 20)
	rows := make([]ui.JobRow, len(jobs))
	for i, j := range jobs {
		rows[i] = ui.JobRow{
			ID: j.ID, Type: j.Type, Status: j.Status,
			CreatedAt: j.CreatedAt, Error: j.Error,
		}
	}
	ui.RenderFragment(w, ui.JobTableFragment(rows))
}

func (s *Server) handleFragPolicyTable(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		ui.RenderFragment(w, "")
		return
	}
	policies, _ := s.store.AllDoorPolicies(r.Context())
	rows := make([]ui.PolicyRow, len(policies))
	for i, p := range policies {
		rows[i] = ui.PolicyRow{
			DoorID: p.DoorID, DoorName: p.DoorName,
			Policy: p.Policy, AllowedBadges: p.AllowedBadges,
		}
	}
	ui.RenderFragment(w, ui.PolicyTableFragment(rows))
}

func (s *Server) handleFragMetricsSummary(w http.ResponseWriter, r *http.Request) {
	if s.metrics == nil {
		ui.RenderFragment(w, "")
		return
	}
	summary := s.metrics.JSONSummary()
	uptime := "unknown"
	if u, ok := summary["uptime"].(string); ok {
		uptime = u
	}
	counters := make(map[string]int64)
	if c, ok := summary["counters"].(map[string]int64); ok {
		counters = c
	}
	gauges := make(map[string]float64)
	if g, ok := summary["gauges"].(map[string]float64); ok {
		gauges = g
	}
	ui.RenderFragment(w, ui.MetricsSummaryFragment(uptime, counters, gauges))
}

// handleFragShadowDecisions renders the panel comparing UniFi's native
// verdict (event.Result: ACCESS|BLOCKED) against the bridge's decision.
// It is the primary signal during shadow-mode burn-in — every row is a tap
// that would behave differently between UniFi and the bridge, which must be
// investigated before flipping to live.
func (s *Server) handleFragShadowDecisions(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		ui.RenderFragment(w, "")
		return
	}
	ctx := r.Context()

	stats, err := s.store.ShadowDecisionStatsToday(ctx)
	if err != nil {
		s.logger.Error("shadow decision stats failed", "error", err)
		stats = &store.ShadowDecisionStats{}
	}

	events, err := s.store.DisagreementEvents(ctx, 50)
	if err != nil {
		s.logger.Error("disagreement events fetch failed", "error", err)
	}

	rows := make([]ui.ShadowDecisionRow, len(events))
	for i, e := range events {
		name := e.CustomerName
		if name == "" {
			name = "Unknown"
		}
		t := e.Timestamp
		if len(t) > 19 {
			t = t[11:19]
		}
		rows[i] = ui.ShadowDecisionRow{
			Time:        t,
			Name:        name,
			NfcUID:      e.NfcUID,
			Door:        e.DoorName,
			UnifiResult: e.UnifiResult,
			OurResult:   e.Result,
			DenyReason:  e.DenyReason,
		}
	}

	ui.RenderFragment(w, ui.ShadowDecisionsFragment(
		stats.Total, stats.Agree, stats.Disagree, stats.Unknown,
		stats.WouldMiss, stats.WouldAdmit,
		rows,
	))
}

// handleFragUnmatchedTable renders the list of UniFi users with NFC tags
// that couldn't be paired with a Redpoint customer. Backed by a dry-run
// ingest against UniFi — slow-ish (hits the UA-Hub for every user) so this
// fragment is load-triggered once per page visit, not on a poll. Each row
// ends in a "Search Redpoint →" button that deep-links into the Members
// page with the UniFi name/email prefilled.
func (s *Server) handleFragUnmatchedTable(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, max-age=5")

	if s.ingester == nil {
		ui.RenderFragment(w, `<p style="color: var(--text-muted)">Ingester not configured.</p>`)
		return
	}

	const key = "frag-unmatched-table"
	if body, hit := s.htmlCache.Get(key); hit {
		w.Write(body)
		return
	}

	result, err := s.ingester.Run(r.Context(), true) // always dry run
	if err != nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Ingest scan failed: "+err.Error()))
		return
	}

	rows := make([]ui.UnmatchedRow, 0)
	for _, m := range result.Mappings {
		if m.Method != ingest.MatchNone {
			continue
		}
		cat := "no_match"
		if strings.Contains(m.Warning, "multiple") {
			cat = "multiple_match"
		}
		rows = append(rows, ui.UnmatchedRow{
			UniFiUserID: m.UniFiUserID,
			UniFiName:   m.UniFiName,
			UniFiEmail:  m.UniFiEmail,
			NfcTokens:   m.NfcTokens,
			Category:    cat,
			Warning:     m.Warning,
		})
	}

	html := ui.UnmatchedTableFragment(
		result.UniFiUsers, result.Matched, result.Unmatched, rows,
	)
	s.htmlCache.Set(key, []byte(html), 30*time.Second)

	w.Write([]byte(html))
}

// ─── "Needs Match" staff UI (C2) ─────────────────────────────
//
// Backed by ua_user_mappings_pending (written by the statusync matching
// phase). Mutating actions go:
//
//   match:  store.UpsertMapping + DeletePending +
//           AppendMatchAudit(field=mapping, source=staff)
//   skip:   UA-Hub UpdateUserStatus(DEACTIVATED) + DeletePending +
//           AppendMatchAudit(field=user_status, source=staff:skip)
//   defer:  UpsertPending(grace_until=now+7d) + AppendMatchAudit(source=staff:defer)
//
// All three actions hx-swap the detail panel back into place with the
// post-mutation state.
//
// Note (v0.5.1): the match action does NOT call UA-Hub UpdateUser(email).
// The original design (docs/architecture-review.md C2 §Matching, pre-v0.5.1)
// mirrored Redpoint email into UA-Hub on every match. We dropped that —
// Redpoint is the source of truth, and the bridge reads UA-Hub email only
// to drive matching. UA-Hub email stays whatever staff typed when creating
// the user. TouchMappingEmailSynced + last_email_synced_at are dead
// columns today; slated for migration-5 cleanup.

// Note (v0.5.2): the lookupUAUser / lookupUAUsersByID helpers that used
// to enrich the Needs Match views with a live UA-Hub ListUsers walk were
// removed. Both call sites now read ua_name + ua_email off the pending
// row (migration 5), so the Needs Match page no longer has any runtime
// UA-Hub dependency. If a future view needs a UA-Hub user detail that
// isn't cached on the pending row, add a dedicated single-user fetch
// path rather than resurrecting the whole-directory walk.

// handleFragUnmatchedList — table view of ua_user_mappings_pending.
//
// Pre-v0.5.2 this handler made a live UA-Hub ListUsers call on every
// render to enrich each row with a display name + email. That walk
// (17 pages × 100/page at LEF, 10s per-page HTTP timeout) hung the
// entire /ui/needs-match page whenever UA-Hub was slow, because the
// fragment is loaded via hx-trigger="load" — a stuck XHR leaves the
// "Loading unmatched users…" placeholder on screen indefinitely.
//
// The fix is to persist ua_name + ua_email onto the pending row at
// UpsertPending time (see auditMigration5_pending_ua_identity and
// statusync.Syncer.persistDecision). This handler now reads every
// column straight off the local row with zero UA-Hub dependency —
// rendering is dominated by the SQLite query, which is ~milliseconds
// on the ~dozen-row pending table. UA-Hub can be completely offline
// and the page still paints.
func (s *Server) handleFragUnmatchedList(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Store not available"))
		return
	}
	pending, err := s.store.AllPending(r.Context())
	if err != nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Failed to read pending list: "+err.Error()))
		return
	}

	rows := make([]ui.NeedsMatchRow, 0, len(pending))
	for _, p := range pending {
		row := ui.NeedsMatchRow{
			UAUserID:   p.UAUserID,
			UAName:     p.UAName,
			UAEmail:    p.UAEmail,
			Reason:     p.Reason,
			FirstSeen:  p.FirstSeen,
			GraceUntil: p.GraceUntil,
		}
		if p.Candidates != "" {
			row.CandidateCount = len(strings.Split(p.Candidates, "|"))
		}
		rows = append(rows, row)
	}

	ui.RenderFragment(w, ui.NeedsMatchListFragment(rows))
}

// handleFragUnmatchedDetail — per-user detail panel with candidate list.
// Candidates come from two sources:
//
//  1. store.Pending.Candidates (the list the matcher captured when it
//     couldn't auto-pick). These are labelled "email-match" or
//     "name-match" depending on the Pending.Reason.
//  2. Optional free-text search (handleFragUnmatchedSearch replaces
//     this fragment with one where Candidates is the search hit set).
//
// Detail and search both share NeedsMatchDetailFragment as the renderer.
func (s *Server) handleFragUnmatchedDetail(w http.ResponseWriter, r *http.Request) {
	s.renderNeedsMatchDetail(w, r, "" /* searchQuery */)
}

// handleFragUnmatchedSearch — POST: free-text name search against the
// Redpoint client. Replaces the detail panel with the search results.
func (s *Server) handleFragUnmatchedSearch(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	q := strings.TrimSpace(r.FormValue("q"))
	s.renderNeedsMatchDetail(w, r, q)
}

func (s *Server) renderNeedsMatchDetail(w http.ResponseWriter, r *http.Request, searchQuery string) {
	if s.store == nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Store not available"))
		return
	}
	uaUserID := r.PathValue("uaUserId")
	if uaUserID == "" {
		ui.RenderFragment(w, ui.AlertFragment(false, "Missing UA user ID"))
		return
	}

	pending, err := s.store.GetPending(r.Context(), uaUserID)
	if err != nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Failed to load pending row: "+err.Error()))
		return
	}
	if pending == nil {
		ui.RenderFragment(w, ui.AlertFragment(false,
			"This user is no longer in the pending list — another staff member may have just resolved it. Refresh the list."))
		return
	}

	// Read the UA-Hub display identity straight off the pending row
	// instead of talking to UA-Hub live. See handleFragUnmatchedList
	// (v0.5.2 comment) and auditMigration5_pending_ua_identity for
	// the rationale. statusync refreshes these fields on every
	// observation, so they lag UA-Hub by at most one sync interval.
	uaName := pending.UAName
	uaEmail := pending.UAEmail

	// Build candidate list. Order matters — matcher-suggested candidates
	// first (they're the most likely hit; the pending row captured them
	// during the original decision), then search-query hits.
	candidates := []ui.NeedsMatchCandidate{}
	seen := map[string]bool{}

	if s.redpoint != nil {
		for _, rpID := range splitCandidates(pending.Candidates) {
			if rpID == "" || seen[rpID] {
				continue
			}
			cust, err := s.redpoint.GetCustomer(r.Context(), rpID)
			if err != nil || cust == nil {
				continue
			}
			candidates = append(candidates, ui.NeedsMatchCandidate{
				RedpointCustomerID: cust.ID,
				Name:               strings.TrimSpace(cust.FirstName + " " + cust.LastName),
				Email:              cust.Email,
				Active:             cust.Active,
				BadgeName:          badgeNameFor(cust),
				BadgeStatus:        badgeStatusFor(cust),
				Reason:             matcherReasonLabel(pending.Reason),
			})
			seen[rpID] = true
		}

		if searchQuery != "" {
			// Hit the local Redpoint mirror (cache.customers) via FTS5
			// rather than the live Redpoint client. Rationale:
			//
			//  - The live `redpoint.SearchCustomersByName` uses Redpoint's
			//    "Last, First" search filter, which is finicky: hyphenated
			//    names, single-token queries, and typos all miss. Chris
			//    reported "it doesn't always work" — that's why.
			//  - The FTS5 index already covers name, email, external_id,
			//    and barcode in one table with prefix-AND semantics, so
			//    one search box can accept "alice", "alice smith",
			//    "alice@example.com", or "12345" and DTRT for each.
			//  - It's also resilient to Redpoint outages and the 429
			//    storms we've been seeing — the pending-match workflow
			//    should not go dark just because upstream is wobbly.
			//  - The mirror is refreshed on every cache sync, so
			//    "customer in Redpoint but not in cache" is a window of
			//    at most one sync interval; if staff hits that, they
			//    can run a cache sync and retry.
			hits, err := s.store.SearchCustomersFTS(r.Context(), searchQuery, 50)
			if err != nil {
				s.logger.Warn("needs-match search failed", "q", searchQuery, "error", err)
			}
			for _, cust := range hits {
				if seen[cust.RedpointID] {
					continue
				}
				candidates = append(candidates, ui.NeedsMatchCandidate{
					RedpointCustomerID: cust.RedpointID,
					Name:               strings.TrimSpace(cust.FirstName + " " + cust.LastName),
					Email:              cust.Email,
					Active:             cust.Active,
					BadgeName:          cust.BadgeName,
					BadgeStatus:        cust.BadgeStatus,
					Reason:             "search",
				})
				seen[cust.RedpointID] = true
			}
		}
	}

	ui.RenderFragment(w, ui.NeedsMatchDetailFragment(
		uaUserID, uaName, uaEmail,
		pending.FirstSeen, pending.GraceUntil, pending.Reason,
		candidates, searchQuery,
	))
}

// handleFragUnmatchedMatch — POST: bind UA user to a staff-selected
// Redpoint customer. Writes audit, upserts mapping, deletes pending,
// and (in live mode) mirrors the Redpoint email into UA-Hub.
func (s *Server) handleFragUnmatchedMatch(w http.ResponseWriter, r *http.Request) {
	if s.store == nil || s.redpoint == nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Store or Redpoint client not configured"))
		return
	}
	uaUserID := r.PathValue("uaUserId")
	if err := r.ParseForm(); err != nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Bad form: "+err.Error()))
		return
	}
	customerID := strings.TrimSpace(r.FormValue("redpointCustomerId"))
	if uaUserID == "" || customerID == "" {
		ui.RenderFragment(w, ui.AlertFragment(false, "Missing UA user ID or Redpoint customer ID"))
		return
	}

	// Collision check — can't bind one Redpoint customer to two UA users.
	if existing, err := s.store.GetMappingByCustomerID(r.Context(), customerID); err == nil && existing != nil && existing.UAUserID != uaUserID {
		ui.RenderFragment(w, ui.AlertFragment(false,
			fmt.Sprintf("Redpoint customer %s is already bound to a different UA-Hub user (%s). Un-match that user first or pick a different customer.",
				customerID, existing.UAUserID)))
		return
	}

	cust, err := s.redpoint.GetCustomer(r.Context(), customerID)
	if err != nil || cust == nil {
		ui.RenderFragment(w, ui.AlertFragment(false,
			"Couldn't read the selected Redpoint customer: "+errOrMissing(err)))
		return
	}

	// Persist mapping + audit + drop pending. Order: audit first so the
	// forensic trail records even if the subsequent write fails.
	if err := s.store.AppendMatchAudit(r.Context(), &store.MatchAudit{
		UAUserID:  uaUserID,
		Field:     "mapping",
		BeforeVal: "",
		AfterVal:  customerID,
		Source:    statusync.MatchSourceStaff,
	}); err != nil {
		s.logger.Error("match audit write failed", "uaUserId", uaUserID, "error", err)
	}
	if err := s.store.UpsertMapping(r.Context(), &store.Mapping{
		UAUserID:         uaUserID,
		RedpointCustomer: customerID,
		MatchedBy:        statusync.MatchSourceStaff,
	}); err != nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Failed to write mapping: "+err.Error()))
		return
	}
	if err := s.store.DeletePending(r.Context(), uaUserID); err != nil {
		s.logger.Warn("pending row delete after match failed", "uaUserId", uaUserID, "error", err)
	}

	// Backfill the members table so the matched customer appears as
	// "Enrolled" in the directory-search view (and on the All Enrolled
	// Members table). The /ingest/unifi pipeline does this for users
	// matched at ingest time (ingest/unifi.go step 4); when staff
	// resolves the match manually here, the members row never got
	// written, so the customer kept showing the badge_denied "Not
	// enrolled" state — matching invisibly. Symmetric fix: pull the
	// UA-Hub user's NFC tokens and UpsertMember for each, mirroring
	// the ingest path's member shape.
	//
	// Best-effort: if UA-Hub is unreachable or the user has no tokens,
	// log and continue. The mapping is the source of truth; the members
	// row is a UI/cache concern that the daily syncer + check-in path
	// will eventually populate. We don't want to fail the match because
	// of a transient UA-Hub blip.
	memberCount := 0
	if uaUser, err := s.unifi.FetchUser(r.Context(), uaUserID); err != nil {
		s.logger.Warn("UA-Hub user fetch after match failed; members row not backfilled",
			"uaUserId", uaUserID, "redpointCustomerId", customerID, "error", err)
	} else if uaUser != nil && len(uaUser.NfcTokens) > 0 {
		badgeStatus := "PENDING_SYNC"
		badgeName := ""
		if cust.Badge != nil {
			if cust.Badge.Status != "" {
				badgeStatus = cust.Badge.Status
			}
			if cust.Badge.CustomerBadge != nil {
				badgeName = cust.Badge.CustomerBadge.Name
			}
		} else if cust.Active {
			// Customer is active in Redpoint but the badge subgraph
			// didn't come back — daily syncer will refresh.
			badgeStatus = "ACTIVE"
		}
		now := time.Now().UTC().Format(time.RFC3339)
		for _, token := range uaUser.NfcTokens {
			m := &store.Member{
				NfcUID:      strings.ToUpper(token),
				CustomerID:  customerID,
				FirstName:   cust.FirstName,
				LastName:    cust.LastName,
				BadgeStatus: badgeStatus,
				BadgeName:   badgeName,
				Active:      cust.Active,
				CachedAt:    now,
			}
			if err := s.store.UpsertMember(r.Context(), m); err != nil {
				s.logger.Warn("members row upsert after match failed",
					"uaUserId", uaUserID, "redpointCustomerId", customerID,
					"nfcUid", m.NfcUID, "error", err)
				continue
			}
			memberCount++
		}
	} else {
		// No NFC tokens on the UA-Hub user — they're managed in UA-Hub
		// but haven't been issued a card yet. Mapping is recorded;
		// they'll appear on the members list once their card is
		// provisioned and the next ingest runs.
		s.logger.Info("matched UA-Hub user has no NFC tokens; members row deferred to ingest",
			"uaUserId", uaUserID, "redpointCustomerId", customerID)
	}

	s.audit.Log("staff_match", r.RemoteAddr, map[string]any{
		"uaUserId":           uaUserID,
		"redpointCustomerId": customerID,
		"customerName":       strings.TrimSpace(cust.FirstName + " " + cust.LastName),
		"memberRowsWritten":  memberCount,
	})

	// Invalidate the fragment cache and trigger a member-table refresh
	// so the directory search and the All Enrolled Members table reflect
	// the new state immediately. Mirrors the unbind/reactivate/reassign
	// handlers (members_detail.go:208, 287; members_reassign.go:285).
	s.htmlCache.Invalidate()
	w.Header().Set("HX-Trigger", "member-updated")

	// Rebuild the detail fragment with a confirmation alert. Since the
	// pending row is gone, GetPending returns nil — render a success
	// alert in its place.
	//
	// Note: v0.5.1 dropped the "next sync will mirror the email" line.
	// The bridge does NOT push Redpoint email into UA-Hub — it only
	// reads UA-Hub emails to drive its own matching. See
	// docs/architecture-review.md C2 §Matching for the decision.
	ui.RenderFragment(w, ui.AlertFragment(true,
		fmt.Sprintf("Matched UA-Hub user %s → Redpoint %s (%s). Saved.",
			uaUserID, customerID, strings.TrimSpace(cust.FirstName+" "+cust.LastName))))
}

// handleFragUnmatchedSkip — POST: deactivate UA-Hub user immediately and
// drop the pending row. Used for ex-member cards predating the bridge.
func (s *Server) handleFragUnmatchedSkip(w http.ResponseWriter, r *http.Request) {
	if s.store == nil || s.unifi == nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Store or UniFi client not configured"))
		return
	}
	uaUserID := r.PathValue("uaUserId")
	if uaUserID == "" {
		ui.RenderFragment(w, ui.AlertFragment(false, "Missing UA user ID"))
		return
	}
	if err := s.unifi.UpdateUserStatus(r.Context(), uaUserID, "DEACTIVATED"); err != nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "UA-Hub deactivate failed: "+err.Error()))
		return
	}
	if err := s.store.AppendMatchAudit(r.Context(), &store.MatchAudit{
		UAUserID:  uaUserID,
		Field:     "user_status",
		BeforeVal: "ACTIVE",
		AfterVal:  "DEACTIVATED",
		Source:    statusync.MatchSourceStaffSkip,
	}); err != nil {
		s.logger.Error("skip audit write failed", "uaUserId", uaUserID, "error", err)
	}
	if err := s.store.DeletePending(r.Context(), uaUserID); err != nil {
		s.logger.Warn("pending delete after skip failed", "uaUserId", uaUserID, "error", err)
	}
	s.audit.Log("staff_skip", r.RemoteAddr, map[string]any{"uaUserId": uaUserID})

	ui.RenderFragment(w, ui.AlertFragment(true,
		fmt.Sprintf("Skipped UA-Hub user %s — deactivated now. The user will not tap in until staff re-activates.", uaUserID)))
}

// handleFragUnmatchedDefer — POST: extend pending grace window by 7 days.
func (s *Server) handleFragUnmatchedDefer(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Store not configured"))
		return
	}
	uaUserID := r.PathValue("uaUserId")
	if uaUserID == "" {
		ui.RenderFragment(w, ui.AlertFragment(false, "Missing UA user ID"))
		return
	}
	existing, err := s.store.GetPending(r.Context(), uaUserID)
	if err != nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Read pending failed: "+err.Error()))
		return
	}
	if existing == nil {
		ui.RenderFragment(w, ui.AlertFragment(false,
			"This user is not in the pending list anymore — nothing to defer."))
		return
	}
	newGrace := time.Now().Add(7 * 24 * time.Hour).UTC().Format(time.RFC3339)
	existing.GraceUntil = newGrace
	existing.LastSeen = time.Now().UTC().Format(time.RFC3339)
	if err := s.store.UpsertPending(r.Context(), existing); err != nil {
		ui.RenderFragment(w, ui.AlertFragment(false, "Defer failed: "+err.Error()))
		return
	}
	if err := s.store.AppendMatchAudit(r.Context(), &store.MatchAudit{
		UAUserID:  uaUserID,
		Field:     "grace_until",
		BeforeVal: "",
		AfterVal:  newGrace,
		Source:    statusync.MatchSourceStaffDefer,
	}); err != nil {
		s.logger.Warn("defer audit write failed", "uaUserId", uaUserID, "error", err)
	}
	s.audit.Log("staff_defer", r.RemoteAddr, map[string]any{"uaUserId": uaUserID, "graceUntil": newGrace})

	ui.RenderFragment(w, ui.AlertFragment(true,
		fmt.Sprintf("Deferred — grace window extended to %s.", newGrace)))
}

// splitCandidates parses the pipe-separated Candidates field from a
// Pending row into a slice of Redpoint customer IDs. Returns nil for
// empty input.
func splitCandidates(raw string) []string {
	if raw == "" {
		return nil
	}
	return strings.Split(raw, "|")
}

// badgeNameFor / badgeStatusFor pull badge info off the Redpoint customer
// shape without knowing whether the badge struct is populated.
func badgeNameFor(c *redpoint.Customer) string {
	if c == nil || c.Badge == nil || c.Badge.CustomerBadge == nil {
		return ""
	}
	return c.Badge.CustomerBadge.Name
}

func badgeStatusFor(c *redpoint.Customer) string {
	if c == nil || c.Badge == nil {
		return ""
	}
	return c.Badge.Status
}

// matcherReasonLabel maps the symbolic pending reason to a short chip
// string used in the candidate "Why" column.
func matcherReasonLabel(reason string) string {
	switch reason {
	case store.PendingReasonAmbiguousEmail:
		return "email-match"
	case store.PendingReasonAmbiguousName, store.PendingReasonNoEmail, store.PendingReasonNoMatch:
		return "name-match"
	default:
		return reason
	}
}

func errOrMissing(err error) string {
	if err == nil {
		return "customer not found"
	}
	return err.Error()
}
