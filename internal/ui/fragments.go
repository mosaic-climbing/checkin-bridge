package ui

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// StatsFragment renders the dashboard stats grid.
func StatsFragment(membersActive, membersTotal, checkinsToday, deniedToday int, wsConnected bool) string {
	connClass := "disconnected"
	connText := "Disconnected"
	if wsConnected {
		connClass = "connected"
		connText = "Connected"
	}
	return fmt.Sprintf(`<div class="stats-grid">
        <div class="stat-card"><div class="stat-value">%d</div><div class="stat-label">Active Members</div></div>
        <div class="stat-card"><div class="stat-value">%d</div><div class="stat-label">Total Enrolled</div></div>
        <div class="stat-card"><div class="stat-value">%d</div><div class="stat-label">Check-ins Today</div></div>
        <div class="stat-card"><div class="stat-value">%d</div><div class="stat-label">Denied Today</div></div>
        <div class="stat-card"><div class="stat-value"><span class="status-dot %s"></span>%s</div><div class="stat-label">UniFi WebSocket</div></div>
    </div>`, membersActive, membersTotal, checkinsToday, deniedToday, connClass, connText)
}

// CheckInTableFragment renders a table of recent check-in events.
type CheckInRow struct {
	Time       string
	Name       string
	NfcUID     string
	Door       string
	Result     string
	DenyReason string
}

func CheckInTableFragment(rows []CheckInRow) string {
	if len(rows) == 0 {
		return `<p style="color: var(--text-muted); padding: 12px 0">No check-ins recorded yet.</p>`
	}

	var sb strings.Builder
	sb.WriteString(`<table><thead><tr><th>Time</th><th>Member</th><th>Card</th><th>Door</th><th>Result</th></tr></thead><tbody>`)
	for _, r := range rows {
		badgeClass := "badge-allowed"
		if r.Result == "denied" {
			badgeClass = "badge-denied"
		}
		resultText := r.Result
		if r.DenyReason != "" {
			resultText += ": " + HTMLEscape(r.DenyReason)
		}
		sb.WriteString(fmt.Sprintf(`<tr><td>%s</td><td>%s</td><td><code>%s</code></td><td>%s</td><td><span class="badge %s">%s</span></td></tr>`,
			HTMLEscape(r.Time), HTMLEscape(r.Name), HTMLEscape(r.NfcUID), HTMLEscape(r.Door), badgeClass, HTMLEscape(resultText)))
	}
	sb.WriteString(`</tbody></table>`)
	return sb.String()
}

// MemberTableFragment renders the member list table.
type MemberRow struct {
	NfcUID      string
	Name        string
	BadgeStatus string
	BadgeName   string
	LastCheckIn string
	CustomerID  string
}

func MemberTableFragment(rows []MemberRow) string {
	if len(rows) == 0 {
		return `<p style="color: var(--text-muted); padding: 12px 0">No members enrolled. Add users in UniFi Access and run an ingest.</p>`
	}

	var sb strings.Builder
	sb.WriteString(`<table><thead><tr><th>Name</th><th>NFC UID</th><th>Status</th><th>Membership</th><th>Last Check-in</th><th></th></tr></thead><tbody>`)
	for _, r := range rows {
		sb.WriteString(memberRow(r))
	}
	sb.WriteString(`</tbody></table>`)
	return sb.String()
}

// memberRow renders one <tr> shared by MemberTableFragment and
// MemberTableFragmentPaged. Extracted so the Details + Remove button
// wiring stays consistent across the two render paths.
func memberRow(r MemberRow) string {
	badgeClass := "badge-active"
	switch r.BadgeStatus {
	case "FROZEN":
		badgeClass = "badge-frozen"
	case "EXPIRED":
		badgeClass = "badge-expired"
	case "PENDING_SYNC":
		badgeClass = "badge-pending"
	}
	lastCI := r.LastCheckIn
	if lastCI == "" {
		lastCI = "Never"
	}
	// The Details button opens the v0.5.9 recovery panel (hx-target
	// "#member-detail"). The Remove button stays on the row because
	// the swap:0.3s row-removal animation is the cheapest UX for the
	// common case; clicking Remove from inside the detail panel works
	// too (that button targets "#member-detail" instead).
	return fmt.Sprintf(`<tr>
            <td>%s</td><td><code>%s</code></td>
            <td><span class="badge %s">%s</span></td>
            <td>%s</td><td>%s</td>
            <td style="white-space: nowrap">
                <button class="btn btn-primary btn-sm"
                    hx-get="/ui/frag/member/%s/detail"
                    hx-target="#member-detail"
                    hx-swap="innerHTML"
                    hx-headers='{"X-Requested-With":"XMLHttpRequest"}'>Details</button>
                <button class="btn btn-danger btn-sm"
                    hx-delete="/members/%s"
                    hx-target="closest tr"
                    hx-swap="outerHTML swap:0.3s"
                    hx-confirm="Remove %s from enrolled members?"
                    hx-headers='{"X-Requested-With":"XMLHttpRequest"}'>Remove</button>
            </td>
        </tr>`,
		HTMLEscape(r.Name), HTMLEscape(r.NfcUID),
		badgeClass, HTMLEscape(r.BadgeStatus),
		HTMLEscape(r.BadgeName), HTMLEscape(lastCI),
		HTMLEscape(r.NfcUID),
		HTMLEscape(r.NfcUID), HTMLEscape(r.Name))
}

// MemberTableFragmentPaged renders either:
//   - A full <table> (when offset == 0), including <thead> and a load-more <tr>
//     at the bottom if more rows remain; or
//   - Just the extra <tr> rows + a fresh load-more <tr> (when offset > 0),
//     suitable for hx-swap="outerHTML" on #member-table-load-more.
//
// nextOffset is offset+len(rows). If nextOffset >= total, no load-more row
// is emitted.
func MemberTableFragmentPaged(rows []MemberRow, offset, total int) string {
	if len(rows) == 0 && offset == 0 {
		return `<p style="color: var(--text-muted); padding: 12px 0">No members enrolled. Add users in UniFi Access and run an ingest.</p>`
	}

	var sb strings.Builder

	// Full table on first page (offset == 0)
	if offset == 0 {
		sb.WriteString(`<table><thead><tr><th>Name</th><th>NFC UID</th><th>Status</th><th>Membership</th><th>Last Check-in</th><th></th></tr></thead><tbody>`)
	}

	// Render rows
	for _, r := range rows {
		sb.WriteString(memberRow(r))
	}

	// Add load-more row if there are more pages
	nextOffset := offset + len(rows)
	if nextOffset < total {
		remaining := total - nextOffset
		sb.WriteString(fmt.Sprintf(`<tr id="member-table-load-more">
  <td colspan="6" style="text-align:center; padding: 1em">
    <button
      hx-get="/ui/frag/member-table?offset=%d&limit=50"
      hx-target="#member-table-load-more"
      hx-swap="outerHTML"
      class="btn btn-secondary">Load more (%d remaining)</button>
  </td>
</tr>`, nextOffset, remaining))
	}

	// Close table if on first page
	if offset == 0 {
		sb.WriteString(`</tbody></table>`)
	}

	return sb.String()
}

// SearchResultsFragment renders customer directory search results.
type SearchResult struct {
	RedpointID string
	Name       string
	Email      string
	InCache    bool
	NfcUID     string
}

func SearchResultsFragment(results []SearchResult) string {
	if len(results) == 0 {
		return `<p style="color: var(--text-muted); padding: 8px 0">No results found.</p>`
	}

	// v0.5.9: the search is now read-only. Before this release the
	// trailing column held a "Select" button that populated the inline
	// Add Member form. The form is gone (members are provisioned in
	// UniFi Access, not the bridge), so the button went with it. For
	// enrolled rows we render a "View details" action that hx-get's the
	// member detail panel so staff can jump from a name/email search
	// into the recovery toolkit. For non-enrolled rows we render a
	// hint that tells the operator to add the user in UniFi Access
	// first (which will then surface in Needs Match on next sync).
	var sb strings.Builder
	sb.WriteString(`<table><thead><tr><th>Name</th><th>Email</th><th>Status</th><th></th></tr></thead><tbody>`)
	for _, r := range results {
		status := `<span class="badge badge-denied">Not enrolled</span>`
		action := `<span style="color: var(--text-muted); font-size: 12px">Add in UniFi Access</span>`
		if r.InCache {
			status = fmt.Sprintf(`<span class="badge badge-active">Enrolled (%s)</span>`, HTMLEscape(r.NfcUID))
			// Jump into the detail panel; hx-target is the sticky
			// #member-detail sink that lives above the member table.
			action = fmt.Sprintf(
				`<button type="button" class="btn btn-primary btn-sm"`+
					` hx-get="/ui/frag/member/%s/detail"`+
					` hx-target="#member-detail" hx-swap="innerHTML"`+
					` hx-headers='{"X-Requested-With":"XMLHttpRequest"}'>View details</button>`,
				HTMLEscape(r.NfcUID))
		}
		sb.WriteString(fmt.Sprintf(`<tr><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>`,
			HTMLEscape(r.Name), HTMLEscape(r.Email), status, action))
	}
	sb.WriteString(`</tbody></table>`)
	return sb.String()
}

// JobTableFragment renders background job status.
type JobRow struct {
	ID        string
	Type      string
	Status    string
	CreatedAt string
	Error     string
}

func JobTableFragment(rows []JobRow) string {
	if len(rows) == 0 {
		return `<p style="color: var(--text-muted); padding: 12px 0">No jobs have run yet.</p>`
	}

	var sb strings.Builder
	sb.WriteString(`<table><thead><tr><th>ID</th><th>Type</th><th>Status</th><th>Started</th><th>Error</th></tr></thead><tbody>`)
	for _, r := range rows {
		badgeClass := "badge-pending"
		switch r.Status {
		case "running":
			badgeClass = "badge-running"
		case "completed":
			badgeClass = "badge-completed"
		case "failed":
			badgeClass = "badge-failed"
		}
		sb.WriteString(fmt.Sprintf(`<tr><td><code>%s</code></td><td>%s</td><td><span class="badge %s">%s</span></td><td>%s</td><td>%s</td></tr>`,
			HTMLEscape(r.ID), HTMLEscape(r.Type), badgeClass, HTMLEscape(r.Status), HTMLEscape(r.CreatedAt), HTMLEscape(r.Error)))
	}
	sb.WriteString(`</tbody></table>`)
	return sb.String()
}

// PolicyTableFragment renders door policies.
type PolicyRow struct {
	DoorID        string
	DoorName      string
	Policy        string
	AllowedBadges string
}

func PolicyTableFragment(rows []PolicyRow) string {
	if len(rows) == 0 {
		return `<p style="color: var(--text-muted); padding: 12px 0">No door policies configured. All doors use default membership check.</p>`
	}

	var sb strings.Builder
	sb.WriteString(`<table><thead><tr><th>Door</th><th>Policy</th><th>Allowed Badges</th><th></th></tr></thead><tbody>`)
	for _, r := range rows {
		badges := r.AllowedBadges
		if badges == "" {
			badges = "All"
		}
		sb.WriteString(fmt.Sprintf(`<tr>
            <td>%s<br><code style="font-size:11px; color: var(--text-muted)">%s</code></td>
            <td><span class="badge badge-active">%s</span></td>
            <td>%s</td>
            <td><button class="btn btn-danger btn-sm"
                hx-delete="/ui/frag/door-policy/%s"
                hx-target="closest tr"
                hx-swap="outerHTML"
                hx-confirm="Remove policy for %s?"
                hx-headers='{"X-Requested-With":"XMLHttpRequest"}'>Remove</button></td>
        </tr>`, HTMLEscape(r.DoorName), HTMLEscape(r.DoorID), HTMLEscape(r.Policy), HTMLEscape(badges), HTMLEscape(r.DoorID), HTMLEscape(r.DoorName)))
	}
	sb.WriteString(`</tbody></table>`)
	return sb.String()
}

// AlertFragment renders a success or error alert.
func AlertFragment(success bool, message string) string {
	class := "alert-success"
	if !success {
		class = "alert-error"
	}
	return fmt.Sprintf(`<div class="alert %s">%s</div>`, class, HTMLEscape(message))
}

// ─── Sync page fragments (v0.5.1) ─────────────────────────────
//
// Three helpers support the rewired /ui/sync page:
//
//   1. SyncResultFragment: the rich confirmation that swaps into
//      #sync-result after a staff click. Title + body + optional
//      list of "kind: count" stat rows. Replaces the raw-JSON
//      swap that v0.5.0 accidentally shipped.
//
//   2. SyncLastRunPill: the inline badge rendered inside each
//      sync card showing the most recent run's age and outcome.
//      Polled via hx-trigger="load, every 15s" per card so staff
//      can see a scheduled sync fire without reloading the page.
//
//   3. FormatRelative: parses an RFC3339 timestamp and returns a
//      compact relative string ("just now", "12m ago", "2h ago",
//      "3d ago"). Empty or unparseable input returns "never".

// SyncStat is a single labelled count row shown inside a
// SyncResultFragment. Value is rendered as-is (free text) so
// callers can pass "883" or "0.4s" or "dry run — no writes".
type SyncStat struct {
	Label string
	Value string
}

// SyncResultFragment renders the staff-facing confirmation of a sync
// action. success=true uses green styling; false uses red. title is
// the one-line headline ("Cache sync complete"), body is one or two
// sentences of context, and stats is an optional bullet list of
// "label: value" pairs rendered as a table inside the alert.
//
// The fragment targets #sync-result on the /ui/sync page (see
// internal/ui/templates/pages/sync.html). It also carries an
// hx-swap-oob span that refreshes the per-card "Last run" pill so
// the page-level state visibly advances without a second HTTP round
// trip.
func SyncResultFragment(success bool, title, body string, stats []SyncStat, pillJobType string) string {
	kind := "alert-success"
	icon := "✓"
	if !success {
		kind = "alert-error"
		icon = "✗"
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(`<div class="alert %s" style="display:flex;flex-direction:column;gap:8px">`, kind))
	sb.WriteString(fmt.Sprintf(`<div style="font-weight:600;font-size:14px">%s %s</div>`,
		icon, HTMLEscape(title)))
	if body != "" {
		sb.WriteString(fmt.Sprintf(`<div style="font-size:13px;line-height:1.4">%s</div>`,
			HTMLEscape(body)))
	}
	if len(stats) > 0 {
		sb.WriteString(`<div style="margin-top:4px;display:grid;grid-template-columns:max-content 1fr;gap:4px 12px;font-size:12px">`)
		for _, st := range stats {
			sb.WriteString(fmt.Sprintf(
				`<div style="color:var(--text-muted);text-transform:uppercase;letter-spacing:0.5px">%s</div><div style="font-variant-numeric:tabular-nums"><code>%s</code></div>`,
				HTMLEscape(st.Label), HTMLEscape(st.Value)))
		}
		sb.WriteString(`</div>`)
	}
	sb.WriteString(`</div>`)

	// Out-of-band pill refresh. hx-swap-oob="true" on a root element
	// with a matching id causes HTMX to replace the same-id target on
	// the page without disturbing the primary swap. The pill handler
	// re-reads jobs table so even a failing sync renders its error
	// badge this way.
	if pillJobType != "" {
		sb.WriteString(fmt.Sprintf(
			`<span id="sync-pill-%s" hx-get="/ui/frag/sync-last-run/%s" hx-trigger="load" hx-swap-oob="true"></span>`,
			HTMLEscape(pillJobType), HTMLEscape(pillJobType)))
	}
	return sb.String()
}

// SyncLastRunPill renders the "Last run: 12m ago · ✓" badge that sits
// inside each sync card. jobType is used for the id so the OOB swap in
// SyncResultFragment can target it. status is one of "completed",
// "failed", "running", or empty. createdAt is the timestamp the jobs
// table stamps at CreateJob (either RFC3339 from Go-side writes or
// SQLite's `YYYY-MM-DD HH:MM:SS` from the CreateJob default; both
// are accepted by FormatRelative). A nil/empty job renders the
// "Never run" pill.
//
// Backwards-compatible signature delegates to SyncLastRunPillFull
// with empty progress so older call sites keep compiling.
func SyncLastRunPill(jobType, status, createdAt, errMsg string) string {
	return SyncLastRunPillFull(jobType, status, createdAt, errMsg, "")
}

// unstickAgeThreshold is how long a job has to have been "running"
// before SyncLastRunPillFull renders the "Clear stuck" affordance.
// Picked to comfortably exceed the longest legitimate refresh
// observed at LEF (UA-Hub mirror walk: ~4-5min for ~1.6k users +
// 75ms hydrate spacing). A row that's still 'running' beyond this
// is overwhelmingly likely to be wedged. The link is non-destructive
// — staff click it, the row flips to 'failed' with a clear note,
// and they can click Run again.
const unstickAgeThreshold = 10 * time.Minute

// SyncLastRunPillFull is the full-fat renderer; SyncLastRunPill
// preserves the old four-arg signature for legacy callers. progress
// is the latest jobs.progress payload — either a quoted JSON string
// or a plain phrase ("hydrating 450/1500"); when present and status
// is "running" it's shown inline so staff see the pill twitch
// through phases instead of staring at a static "⟳ Running".
//
// v0.5.7.1: when status is "running" and the job has been alive for
// at least unstickAgeThreshold, a "Clear stuck" link is appended
// that POSTs to /ui/sync/unstick/{type}. The link is keyed on the
// pill id so HTMX can swap the response in-place, identical
// mechanism to the load/every-15s auto-refresh.
func SyncLastRunPillFull(jobType, status, createdAt, errMsg, progress string) string {
	id := fmt.Sprintf("sync-pill-%s", HTMLEscape(jobType))
	if status == "" {
		return fmt.Sprintf(
			`<span id="%s" class="badge" style="background:#eceff1;color:var(--text-muted);font-weight:500">Never run</span>`,
			id)
	}
	rel := FormatRelative(createdAt)
	switch status {
	case "running":
		// Progress and unstick are independent: a fresh refresh
		// shows "⟳ Running · hydrating 450/1500 · started just
		// now"; a wedged refresh shows "⟳ Running · started 47m
		// ago · Clear stuck" (with progress dropped because
		// stale phase strings tend to mislead more than help).
		// A fresh refresh with no progress yet just shows "⟳
		// Running · started just now".
		stuck := isStuckRunning(createdAt)
		var phase string
		if !stuck {
			phase = trimPhase(progress)
		}
		var sb strings.Builder
		fmt.Fprintf(&sb,
			`<span id="%s" class="badge badge-running" title="Started %s">⟳ Running`,
			id, HTMLEscape(createdAt))
		if phase != "" {
			fmt.Fprintf(&sb, ` · %s`, HTMLEscape(phase))
		}
		fmt.Fprintf(&sb, ` · started %s`, HTMLEscape(rel))
		if stuck {
			// Render the unstick link as part of the same pill
			// so the OOB swap target stays unique. hx-target
			// matches the pill's own id; hx-swap=outerHTML
			// replaces the whole badge with whatever pill the
			// unstick handler returns (typically a fresh ✗
			// Failed badge).
			fmt.Fprintf(&sb,
				` · <a href="#" class="link-unstick" `+
					`hx-post="/ui/sync/unstick/%s" `+
					`hx-target="#%s" hx-swap="outerHTML" `+
					`hx-headers='{"X-Requested-With":"XMLHttpRequest"}' `+
					`hx-confirm="Mark this stuck job as failed? The next click will start a fresh run.">Clear stuck</a>`,
				HTMLEscape(jobType), id)
		}
		sb.WriteString(`</span>`)
		return sb.String()
	case "failed":
		tooltip := "Failed"
		if errMsg != "" {
			tooltip = "Failed: " + errMsg
		}
		return fmt.Sprintf(
			`<span id="%s" class="badge badge-failed" title="%s">✗ Failed · %s</span>`,
			id, HTMLEscape(tooltip), HTMLEscape(rel))
	case "completed":
		return fmt.Sprintf(
			`<span id="%s" class="badge badge-completed" title="Completed %s">✓ %s</span>`,
			id, HTMLEscape(createdAt), HTMLEscape(rel))
	default:
		return fmt.Sprintf(
			`<span id="%s" class="badge" style="background:#eceff1;color:var(--text-muted)">%s · %s</span>`,
			id, HTMLEscape(status), HTMLEscape(rel))
	}
}

// isStuckRunning returns true when the running job's created_at is
// older than unstickAgeThreshold. Falls back to "not stuck" on
// unparseable input so a fresh row with a malformed timestamp
// doesn't immediately surface the unstick link.
func isStuckRunning(createdAt string) bool {
	t, ok := parseStoreTimestamp(createdAt)
	if !ok {
		return false
	}
	return time.Since(t) >= unstickAgeThreshold
}

// trimPhase strips the JSON quoting around a progress payload
// written by Store.UpdateJobProgress. The store marshals the value
// as JSON before storing, so a plain phase "hydrating 450/1500"
// arrives as `"hydrating 450/1500"`. Render the bare phrase rather
// than the leaky implementation detail. Empty / non-string payloads
// fall through to "" so the running-case renderer just omits the
// phase segment.
func trimPhase(progress string) string {
	if progress == "" {
		return ""
	}
	var s string
	if err := json.Unmarshal([]byte(progress), &s); err == nil {
		return s
	}
	// Not a JSON-quoted string — show whatever's there, trimmed,
	// so a future caller that writes plain text doesn't get
	// silently dropped.
	return strings.TrimSpace(progress)
}

// FormatRelative takes a jobs-table timestamp and returns a compact
// human-readable relative-time string suitable for the sync pill:
// "just now", "12m ago", "2h ago", "3d ago". Empty or unparseable
// input returns "never".
//
// Accepts both RFC3339 (the format Go-side writes use:
// CompleteJob/FailJob/UpdateJobProgress all stamp updated_at via
// `time.Now().UTC().Format(time.RFC3339)`) AND SQLite's
// `YYYY-MM-DD HH:MM:SS` (the format the CreateJob default-stamps
// for created_at — the table column declares
// `DEFAULT CURRENT_TIMESTAMP` and SQLite renders that in space-
// separated form). Pre-v0.5.7.1 this only handled RFC3339, which
// silently broke "Last run: ⟳ Running · started Xm ago" for
// every running pill (created_at always failed to parse, and the
// nil-zero-value cascade short-circuited to "never"). Caught when
// staff reported the pill stuck on "running" with no timestamp.
func FormatRelative(ts string) string {
	t, ok := parseStoreTimestamp(ts)
	if !ok {
		return "never"
	}
	d := time.Since(t)
	if d < 0 {
		// Future timestamps happen in tests + clock skew; pin to "just
		// now" rather than rendering a confusing "-2s ago".
		return "just now"
	}
	switch {
	case d < 45*time.Second:
		return "just now"
	case d < 90*time.Second:
		return "1m ago"
	case d < 60*time.Minute:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Format("Jan 2")
	}
}

// parseStoreTimestamp decodes a jobs-table timestamp into a
// time.Time. Accepts RFC3339 (what CompleteJob/FailJob/
// UpdateJobProgress write via time.Now().UTC().Format(time.RFC3339))
// and SQLite's `YYYY-MM-DD HH:MM:SS` form (what the
// `DEFAULT CURRENT_TIMESTAMP` on created_at renders). Returns
// (t, true) on success, (zero, false) on unparseable input.
//
// The SQLite default is UTC — CURRENT_TIMESTAMP is documented as
// "UTC DATETIME('now')", same semantics our Go-side writes use —
// so parsing in UTC keeps the "started 3m ago" math honest.
func parseStoreTimestamp(ts string) (time.Time, bool) {
	if ts == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		return t, true
	}
	// SQLite default format. Note `2006-01-02 15:04:05` (space,
	// no T, no Z). time.Parse interprets a format without a zone
	// as UTC when the layout has no zone token.
	if t, err := time.Parse("2006-01-02 15:04:05", ts); err == nil {
		return t.UTC(), true
	}
	return time.Time{}, false
}

// UnmatchedRow is a single UniFi user that the ingest couldn't pair with a
// Redpoint customer (either zero hits or multiple ambiguous hits). Surfaced
// in the Sync page so staff can eyeball each one and either fix it in
// Redpoint (correct the name/email) or add them manually via Members.
type UnmatchedRow struct {
	UniFiUserID string
	UniFiName   string
	UniFiEmail  string
	NfcTokens   []string
	Category    string // "no_match" or "multiple_match"
	Warning     string
}

// UnmatchedTableFragment renders the list of unresolvable UniFi users with
// the info needed to fix each one (name, email, the NFC tokens on record)
// plus a jump-link into the Members search prefilled with their name.
func UnmatchedTableFragment(totalUnifi, matched, unmatched int, rows []UnmatchedRow) string {
	var sb strings.Builder

	// Counter strip — lets staff see at a glance how much work is left.
	sb.WriteString(`<div class="stats-grid" style="margin-bottom: 16px">`)
	sb.WriteString(fmt.Sprintf(
		`<div class="stat-card"><div class="stat-value">%d</div><div class="stat-label">UniFi Users</div></div>`,
		totalUnifi))
	sb.WriteString(fmt.Sprintf(
		`<div class="stat-card"><div class="stat-value" style="color:var(--success, #00b894)">%d</div><div class="stat-label">Matched</div></div>`,
		matched))
	sb.WriteString(fmt.Sprintf(
		`<div class="stat-card"><div class="stat-value" style="color:var(--danger, #d63031)">%d</div><div class="stat-label">Unmatched</div></div>`,
		unmatched))
	sb.WriteString(`</div>`)

	if len(rows) == 0 {
		sb.WriteString(`<p style="color: var(--text-muted); padding: 12px 0">` +
			`No unmatched users. Every UniFi account with an NFC tag has a Redpoint match.` +
			`</p>`)
		return sb.String()
	}

	sb.WriteString(`<table><thead><tr>`)
	sb.WriteString(`<th>UniFi Name</th><th>Email</th><th>NFC Tokens</th><th>Why</th><th>Fix</th>`)
	sb.WriteString(`</tr></thead><tbody>`)
	for _, r := range rows {
		catClass := "badge-denied"
		catText := "No match"
		if r.Category == "multiple_match" {
			catClass = "badge-pending"
			catText = "Ambiguous"
		}
		email := r.UniFiEmail
		if email == "" {
			email = `<span style="color: var(--text-muted)">(no email)</span>`
		} else {
			email = HTMLEscape(email)
		}
		tokens := `<span style="color: var(--text-muted)">(none)</span>`
		if len(r.NfcTokens) > 0 {
			parts := make([]string, len(r.NfcTokens))
			for i, t := range r.NfcTokens {
				parts[i] = `<code>` + HTMLEscape(t) + `</code>`
			}
			tokens = strings.Join(parts, " ")
		}
		warning := r.Warning
		if warning == "" {
			warning = "no Redpoint customer with matching name/email"
		}
		// Jump to Members page with search prefilled. The Members search box
		// hits /ui/frag/search-results against the local Redpoint directory
		// cache, so the staffer can scan likely hits and use the Select
		// button → Add Member form to enroll the NFC token shown above.
		searchQuery := r.UniFiEmail
		if searchQuery == "" {
			searchQuery = r.UniFiName
		}
		fixHref := fmt.Sprintf(`/ui/members?q=%s`, HTMLEscape(searchQuery))
		sb.WriteString(fmt.Sprintf(
			`<tr>`+
				`<td>%s</td>`+
				`<td>%s</td>`+
				`<td>%s</td>`+
				`<td><span class="badge %s">%s</span><br><span style="color: var(--text-muted); font-size: 12px">%s</span></td>`+
				`<td><a class="btn btn-primary btn-sm" href="%s">Search Redpoint →</a></td>`+
				`</tr>`,
			HTMLEscape(r.UniFiName),
			email,
			tokens,
			catClass, HTMLEscape(catText),
			HTMLEscape(warning),
			fixHref,
		))
	}
	sb.WriteString(`</tbody></table>`)
	return sb.String()
}

// ShadowDecisionRow is a single UA-Hub-vs-bridge comparison row.
type ShadowDecisionRow struct {
	Time        string
	Name        string
	NfcUID      string
	Door        string
	UnifiResult string // ACCESS | BLOCKED
	OurResult   string // allowed | denied | recheck_allowed
	DenyReason  string
}

// Kind labels the shape of the disagreement for humans.
func (r ShadowDecisionRow) Kind() string {
	u := strings.ToUpper(r.UnifiResult)
	ours := strings.ToLower(r.OurResult)
	switch {
	case u == "ACCESS" && ours == "denied":
		return "Would miss" // paying member we would have turned away
	case u == "BLOCKED" && (ours == "allowed" || ours == "recheck_allowed"):
		return "Would admit" // UniFi rejected, bridge would have let them in
	default:
		return "—"
	}
}

// ShadowDecisionsFragment renders the disagreement table plus an at-a-glance
// counter row. This is the panel Chris watches during the shadow-mode burn-in
// before flipping the bridge to live: every row is a tap where UniFi's own
// ruleset disagrees with what the bridge would have done.
func ShadowDecisionsFragment(
	total, agree, disagree, unknown, wouldMiss, wouldAdmit int,
	rows []ShadowDecisionRow,
) string {
	var sb strings.Builder

	// Headline counters. Agree/Disagree/Unknown partition every tap that has
	// a UniFi result; WouldMiss + WouldAdmit are the two flavours of Disagree.
	sb.WriteString(`<div class="stats-grid" style="margin-bottom: 16px">`)
	sb.WriteString(fmt.Sprintf(
		`<div class="stat-card"><div class="stat-value">%d</div><div class="stat-label">Taps Today</div></div>`,
		total))
	sb.WriteString(fmt.Sprintf(
		`<div class="stat-card"><div class="stat-value" style="color:var(--success, #4caf50)">%d</div><div class="stat-label">Agree</div></div>`,
		agree))
	sb.WriteString(fmt.Sprintf(
		`<div class="stat-card"><div class="stat-value" style="color:var(--danger, #e53935)">%d</div><div class="stat-label">Disagree</div></div>`,
		disagree))
	sb.WriteString(fmt.Sprintf(
		`<div class="stat-card"><div class="stat-value">%d</div><div class="stat-label">Would miss (member→denied)</div></div>`,
		wouldMiss))
	sb.WriteString(fmt.Sprintf(
		`<div class="stat-card"><div class="stat-value">%d</div><div class="stat-label">Would admit (UniFi blocked)</div></div>`,
		wouldAdmit))
	if unknown > 0 {
		sb.WriteString(fmt.Sprintf(
			`<div class="stat-card"><div class="stat-value">%d</div><div class="stat-label">Unknown (no UniFi verdict)</div></div>`,
			unknown))
	}
	sb.WriteString(`</div>`)

	if len(rows) == 0 {
		sb.WriteString(`<p style="color: var(--text-muted); padding: 12px 0">` +
			`No disagreements recorded. UniFi and the bridge are making the same call on every tap.` +
			`</p>`)
		return sb.String()
	}

	sb.WriteString(`<table><thead><tr>`)
	sb.WriteString(`<th>Time</th><th>Member</th><th>Card</th><th>Door</th>`)
	sb.WriteString(`<th>UniFi</th><th>Bridge</th><th>Kind</th><th>Reason</th>`)
	sb.WriteString(`</tr></thead><tbody>`)
	for _, r := range rows {
		unifiClass := "badge-allowed"
		if strings.ToUpper(r.UnifiResult) == "BLOCKED" {
			unifiClass = "badge-denied"
		}
		bridgeClass := "badge-allowed"
		if strings.ToLower(r.OurResult) == "denied" {
			bridgeClass = "badge-denied"
		}
		sb.WriteString(fmt.Sprintf(
			`<tr>`+
				`<td>%s</td><td>%s</td><td><code>%s</code></td><td>%s</td>`+
				`<td><span class="badge %s">%s</span></td>`+
				`<td><span class="badge %s">%s</span></td>`+
				`<td>%s</td><td>%s</td>`+
				`</tr>`,
			HTMLEscape(r.Time), HTMLEscape(r.Name), HTMLEscape(r.NfcUID), HTMLEscape(r.Door),
			unifiClass, HTMLEscape(r.UnifiResult),
			bridgeClass, HTMLEscape(r.OurResult),
			HTMLEscape(r.Kind()),
			HTMLEscape(r.DenyReason),
		))
	}
	sb.WriteString(`</tbody></table>`)
	return sb.String()
}

// MetricsSummaryFragment renders the metrics overview.
func MetricsSummaryFragment(uptime string, counters map[string]int64, gauges map[string]float64) string {
	var sb strings.Builder

	sb.WriteString(`<div class="stats-grid">`)
	sb.WriteString(fmt.Sprintf(`<div class="stat-card"><div class="stat-value" style="font-size:18px">%s</div><div class="stat-label">Uptime</div></div>`, HTMLEscape(uptime)))
	for name, val := range counters {
		sb.WriteString(fmt.Sprintf(`<div class="stat-card"><div class="stat-value">%d</div><div class="stat-label">%s</div></div>`, val, HTMLEscape(name)))
	}
	for name, val := range gauges {
		sb.WriteString(fmt.Sprintf(`<div class="stat-card"><div class="stat-value">%.0f</div><div class="stat-label">%s</div></div>`, val, HTMLEscape(name)))
	}
	sb.WriteString(`</div>`)
	return sb.String()
}

// ─── "Needs Match" staff UI (C2) ──────────────────────────────────────
// Everything below backs the /ui/page/needs-match panel. It replaces the
// ingest-dry-run-driven "unmatched" panel with a DB-backed view of rows
// the sync left in ua_user_mappings_pending. Each row is an actionable
// ticket: match to a Redpoint customer, skip (deactivate now), or defer.

// NeedsMatchRow is one pending-bucket row enriched with whatever UA-Hub
// context we have on hand. UA-Hub name/email may be blank — in that case
// the template renders a placeholder so staff can still click into the
// detail view and fix it from there.
type NeedsMatchRow struct {
	UAUserID       string
	UAName         string
	UAEmail        string
	Reason         string // PendingReason* — rendered as a human-friendly chip
	FirstSeen      string
	GraceUntil     string
	CandidateCount int // len(Candidates) for the headline count; 0 for no_match
}

// NeedsMatchListFragment renders the table of pending rows plus a
// summary chip count. Empty state is explicit — "Nothing to match" is a
// positive signal for staff so the fragment shouldn't render an empty
// table that reads as "something broke".
func NeedsMatchListFragment(rows []NeedsMatchRow) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf(
		`<div class="stats-grid" style="margin-bottom: 16px">`+
			`<div class="stat-card"><div class="stat-value">%d</div><div class="stat-label">Pending Match</div></div>`+
			`</div>`, len(rows)))

	if len(rows) == 0 {
		sb.WriteString(`<p style="color: var(--text-muted); padding: 12px 0">` +
			`Nothing to match. Every UA-Hub user is bound to a Redpoint customer.` +
			`</p>`)
		return sb.String()
	}

	sb.WriteString(`<table><thead><tr>`)
	sb.WriteString(`<th>UA-Hub User</th><th>Email</th><th>Reason</th>` +
		`<th>First Seen</th><th>Deactivates</th><th></th>`)
	sb.WriteString(`</tr></thead><tbody>`)

	for _, r := range rows {
		reasonClass := "badge-denied"
		reasonText := r.Reason
		switch r.Reason {
		case "no_email":
			reasonText = "no email"
		case "no_match":
			reasonText = "no match"
		case "ambiguous_email":
			reasonClass = "badge-pending"
			reasonText = fmt.Sprintf("ambiguous email (%d)", r.CandidateCount)
		case "ambiguous_name":
			reasonClass = "badge-pending"
			reasonText = fmt.Sprintf("ambiguous name (%d)", r.CandidateCount)
		}

		name := r.UAName
		if name == "" {
			name = `<span style="color: var(--text-muted)">(no name)</span>`
		} else {
			name = HTMLEscape(name)
		}
		email := r.UAEmail
		if email == "" {
			email = `<span style="color: var(--text-muted)">(no email)</span>`
		} else {
			email = HTMLEscape(email)
		}

		sb.WriteString(fmt.Sprintf(
			`<tr>`+
				`<td>%s<br><code style="font-size:11px; color: var(--text-muted)">%s</code></td>`+
				`<td>%s</td>`+
				`<td><span class="badge %s">%s</span></td>`+
				`<td>%s</td><td>%s</td>`+
				`<td><button class="btn btn-primary btn-sm"`+
				` hx-get="/ui/frag/unmatched/%s/detail"`+
				` hx-target="#needs-match-detail"`+
				` hx-swap="innerHTML show:window:top"`+
				` hx-headers='{"X-Requested-With":"XMLHttpRequest"}'>Open</button></td>`+
				`</tr>`,
			name, HTMLEscape(r.UAUserID),
			email,
			reasonClass, HTMLEscape(reasonText),
			HTMLEscape(r.FirstSeen), HTMLEscape(r.GraceUntil),
			HTMLEscape(r.UAUserID),
		))
	}
	sb.WriteString(`</tbody></table>`)
	return sb.String()
}

// NeedsMatchCandidate is a Redpoint customer that the bridge wants to
// offer as a potential match. Ranked by the handler (pre-emailed or
// name-nearby candidates float to the top; free-text search results
// are appended).
type NeedsMatchCandidate struct {
	RedpointCustomerID string
	Name               string
	Email              string
	Active             bool
	BadgeName          string
	BadgeStatus        string
	Reason             string // "email-match" | "name-match" | "search" — rendered as a chip
}

// NeedsMatchDetailFragment renders the per-user detail panel: the
// UA-Hub side, the candidate list (checkbox/radio-style select + match
// button), and the skip/defer escape hatches. This is a single fragment
// so mutating actions can hx-swap the whole panel back into place
// without extra orchestration.
func NeedsMatchDetailFragment(
	uaUserID, uaName, uaEmail string,
	firstSeen, graceUntil, reason string,
	candidates []NeedsMatchCandidate,
	searchQuery string,
) string {
	var sb strings.Builder

	sb.WriteString(`<div class="card">`)

	// Header: UA-Hub side.
	displayName := uaName
	if displayName == "" {
		displayName = "(no name)"
	}
	displayEmail := uaEmail
	if displayEmail == "" {
		displayEmail = "(no email)"
	}
	sb.WriteString(fmt.Sprintf(
		`<h3>%s</h3>`+
			`<p style="color: var(--text-muted); margin-bottom: 8px">`+
			`UA-Hub ID: <code>%s</code> · Email: %s · Reason: <code>%s</code> · First seen: %s · Grace until: %s`+
			`</p>`,
		HTMLEscape(displayName),
		HTMLEscape(uaUserID),
		HTMLEscape(displayEmail),
		HTMLEscape(reason),
		HTMLEscape(firstSeen), HTMLEscape(graceUntil),
	))

	// Inline search box — POSTs (CSRF-protected) to the candidate
	// search endpoint; response replaces this whole fragment so the
	// state reflects the most-recent query.
	sb.WriteString(fmt.Sprintf(
		`<form hx-post="/ui/frag/unmatched/%s/search"`+
			` hx-target="#needs-match-detail" hx-swap="innerHTML"`+
			` hx-headers='{"X-Requested-With":"XMLHttpRequest"}' style="margin: 12px 0">`+
			`<input type="text" name="q" value="%s" placeholder="Search Redpoint by name or email…" style="width: 60%%; margin-right: 8px">`+
			`<button class="btn btn-primary btn-sm" type="submit">Search</button>`+
			`</form>`,
		HTMLEscape(uaUserID), HTMLEscape(searchQuery),
	))

	if len(candidates) == 0 {
		sb.WriteString(`<p style="color: var(--text-muted); padding: 8px 0">` +
			`No Redpoint candidates to suggest. Use the search box above, or skip/defer below.` +
			`</p>`)
	} else {
		// Candidate list + match form. A single form wraps the whole
		// table with a radio group, so staff can pick any candidate
		// and click one "Match" button.
		sb.WriteString(fmt.Sprintf(
			`<form hx-post="/ui/frag/unmatched/%s/match"`+
				` hx-target="#needs-match-detail" hx-swap="innerHTML"`+
				` hx-headers='{"X-Requested-With":"XMLHttpRequest"}'>`,
			HTMLEscape(uaUserID),
		))
		sb.WriteString(`<table><thead><tr>`)
		sb.WriteString(`<th></th><th>Name</th><th>Email</th><th>Badge</th><th>Active</th><th>Why</th>`)
		sb.WriteString(`</tr></thead><tbody>`)
		for i, c := range candidates {
			checked := ""
			if i == 0 {
				checked = " checked"
			}
			activeBadge := `<span class="badge badge-denied">inactive</span>`
			if c.Active {
				activeBadge = `<span class="badge badge-active">active</span>`
			}
			badgeChip := HTMLEscape(c.BadgeName)
			if c.BadgeStatus != "" {
				badgeChip = fmt.Sprintf(`%s <span style="color: var(--text-muted); font-size: 11px">(%s)</span>`,
					HTMLEscape(c.BadgeName), HTMLEscape(c.BadgeStatus))
			}
			reasonChip := HTMLEscape(c.Reason)
			sb.WriteString(fmt.Sprintf(
				`<tr>`+
					`<td><input type="radio" name="redpointCustomerId" value="%s"%s></td>`+
					`<td>%s</td><td>%s</td><td>%s</td><td>%s</td>`+
					`<td><code style="font-size: 11px">%s</code></td>`+
					`</tr>`,
				HTMLEscape(c.RedpointCustomerID), checked,
				HTMLEscape(c.Name), HTMLEscape(c.Email),
				badgeChip, activeBadge, reasonChip,
			))
		}
		sb.WriteString(`</tbody></table>`)
		sb.WriteString(`<div style="margin-top: 12px">`)
		sb.WriteString(`<button class="btn btn-success btn-sm" type="submit">Match selected</button>`)
		sb.WriteString(`</div></form>`)
	}

	// Escape hatches — these don't need a candidate selection.
	sb.WriteString(`<hr style="margin: 16px 0; border: none; border-top: 1px solid var(--border, #333)">`)
	sb.WriteString(`<div style="display: flex; gap: 8px">`)
	sb.WriteString(fmt.Sprintf(
		`<button class="btn btn-danger btn-sm"`+
			` hx-post="/ui/frag/unmatched/%s/skip"`+
			` hx-target="#needs-match-detail" hx-swap="innerHTML"`+
			` hx-headers='{"X-Requested-With":"XMLHttpRequest"}'`+
			` hx-confirm="Skip = immediately deactivate this UA-Hub user. Continue?">Skip (deactivate now)</button>`,
		HTMLEscape(uaUserID),
	))
	sb.WriteString(fmt.Sprintf(
		`<button class="btn btn-primary btn-sm"`+
			` hx-post="/ui/frag/unmatched/%s/defer"`+
			` hx-target="#needs-match-detail" hx-swap="innerHTML"`+
			` hx-headers='{"X-Requested-With":"XMLHttpRequest"}'>Defer 7 days</button>`,
		HTMLEscape(uaUserID),
	))
	sb.WriteString(`</div></div>`)
	return sb.String()
}

// ─── Member detail panel (v0.5.9) ─────────────────────────────────────
//
// MemberDetailData is the view-model for the per-member recovery panel
// that opens above the member table on the Members page. It's populated
// by handleFragMemberDetail and rendered by MemberDetailFragment.
//
// Nilability notes:
//   - Mapping == nil means the member row exists in cache but no
//     ua_user_mappings row points at its customer_id. Orphaned members
//     are rare (usually a half-completed ingest) and the panel
//     surfaces them for manual cleanup. Unbind and Reactivate are
//     disabled in that state because both key on UAUserID.
//   - UAUser == nil means the mapping points at a UA-Hub user we
//     haven't mirrored yet (sync-lag). Reactivate and Reassign are
//     disabled; the audit trail + mapping info still render.
//   - Audit may be empty for freshly-bound members. The panel shows a
//     neutral "no audit entries" placeholder rather than an error.
type MemberDetailData struct {
	Member  MemberDetailMember
	Mapping *MemberDetailMapping
	UAUser  *MemberDetailUAUser
	Audit   []MemberAuditRow
}

type MemberDetailMember struct {
	NfcUID      string
	Name        string
	CustomerID  string
	BadgeStatus string
	BadgeName   string
	Active      bool
	LastCheckIn string
	CachedAt    string
}

type MemberDetailMapping struct {
	UAUserID  string
	MatchedAt string
	MatchedBy string
}

type MemberDetailUAUser struct {
	ID     string
	Name   string
	Email  string
	Status string // "ACTIVE" | "DEACTIVATED" | ""
}

type MemberAuditRow struct {
	Timestamp string
	Field     string
	BeforeVal string
	AfterVal  string
	Source    string
}

// MemberDetailFragment renders the sticky detail panel. Mirrors
// NeedsMatchDetailFragment: one self-contained card that action
// mutations hx-swap back into place. Every mutation emits
// `HX-Trigger: member-updated` from the server so the member table
// auto-refreshes after a successful action.
//
// Button enabling rules (documented on the struct above):
//   - Unbind:     requires Mapping != nil
//   - Reactivate: requires Mapping != nil && UAUser != nil && UAUser.Status == "DEACTIVATED"
//   - Remove:     always available
//   - Reassign:   requires Mapping != nil && UAUser != nil
//
// Disabled buttons still render — with a tooltip explaining why —
// so staff don't wonder "where did the button go" on a sync-lag row.
func MemberDetailFragment(d MemberDetailData) string {
	var sb strings.Builder

	sb.WriteString(`<div class="card" id="member-detail-panel">`)

	// ── Header: member identity ─────────────────────────────────────
	activeBadge := `<span class="badge badge-denied">inactive</span>`
	if d.Member.Active {
		activeBadge = `<span class="badge badge-active">active</span>`
	}
	badgeStatusChip := HTMLEscape(d.Member.BadgeStatus)
	if d.Member.BadgeStatus == "" {
		badgeStatusChip = `<span style="color: var(--text-muted)">—</span>`
	}
	lastCI := d.Member.LastCheckIn
	if lastCI == "" {
		lastCI = "Never"
	}

	sb.WriteString(fmt.Sprintf(
		`<div style="display: flex; justify-content: space-between; align-items: flex-start">`+
			`<div>`+
			`<h3 style="margin: 0">%s</h3>`+
			`<p style="color: var(--text-muted); margin: 4px 0 0">`+
			`NFC <code>%s</code> · Redpoint <code>%s</code> · Badge %s · %s · Last tap: %s`+
			`</p>`+
			`</div>`+
			`<button type="button" class="btn btn-sm"`+
			` onclick="document.getElementById('member-detail').innerHTML = ''"`+
			` title="Close panel">✕</button>`+
			`</div>`,
		HTMLEscape(d.Member.Name),
		HTMLEscape(d.Member.NfcUID),
		HTMLEscape(d.Member.CustomerID),
		badgeStatusChip,
		activeBadge,
		HTMLEscape(lastCI),
	))

	// ── Mapping / UA-Hub identity ───────────────────────────────────
	sb.WriteString(`<div style="margin-top: 12px; padding: 12px; background: var(--bg-muted, #1a1a1a); border-radius: 4px">`)
	if d.Mapping == nil {
		sb.WriteString(`<strong style="color: var(--warn, #d4a72c)">No UA-Hub mapping</strong>`)
		sb.WriteString(`<p style="margin: 4px 0 0; color: var(--text-muted); font-size: 13px">`)
		sb.WriteString(`This member is in the bridge cache but no ua_user_mappings row points at their Redpoint customer. Unbind and Reactivate are disabled; use Remove to drop the orphan, then re-ingest from UniFi Access to re-create the binding.`)
		sb.WriteString(`</p>`)
	} else {
		uaName := `<span style="color: var(--text-muted)">(mirror row missing)</span>`
		uaEmail := `<span style="color: var(--text-muted)">—</span>`
		uaStatus := `<span style="color: var(--text-muted)">unknown</span>`
		if d.UAUser != nil {
			if d.UAUser.Name != "" {
				uaName = HTMLEscape(d.UAUser.Name)
			}
			if d.UAUser.Email != "" {
				uaEmail = HTMLEscape(d.UAUser.Email)
			}
			switch d.UAUser.Status {
			case "ACTIVE":
				uaStatus = `<span class="badge badge-active">ACTIVE</span>`
			case "DEACTIVATED":
				uaStatus = `<span class="badge badge-denied">DEACTIVATED</span>`
			case "":
				uaStatus = `<span style="color: var(--text-muted)">(no status)</span>`
			default:
				uaStatus = HTMLEscape(d.UAUser.Status)
			}
		}
		sb.WriteString(fmt.Sprintf(
			`<div style="display: grid; grid-template-columns: max-content 1fr; gap: 4px 12px; font-size: 13px">`+
				`<div style="color: var(--text-muted)">UA-Hub user</div><div>%s <code style="font-size: 11px">(%s)</code></div>`+
				`<div style="color: var(--text-muted)">Email</div><div>%s</div>`+
				`<div style="color: var(--text-muted)">UA-Hub status</div><div>%s</div>`+
				`<div style="color: var(--text-muted)">Matched at</div><div>%s</div>`+
				`<div style="color: var(--text-muted)">Matched by</div><div><code>%s</code></div>`+
				`</div>`,
			uaName, HTMLEscape(d.Mapping.UAUserID),
			uaEmail,
			uaStatus,
			HTMLEscape(d.Mapping.MatchedAt),
			HTMLEscape(d.Mapping.MatchedBy),
		))
	}
	sb.WriteString(`</div>`)

	// ── Recovery actions ────────────────────────────────────────────
	sb.WriteString(`<div style="margin-top: 12px; display: flex; flex-wrap: wrap; gap: 8px">`)

	// Unbind: requires Mapping != nil
	if d.Mapping != nil {
		sb.WriteString(fmt.Sprintf(
			`<button type="button" class="btn btn-warning btn-sm"`+
				` hx-post="/ui/frag/member/%s/unbind"`+
				` hx-target="#member-detail" hx-swap="innerHTML"`+
				` hx-headers='{"X-Requested-With":"XMLHttpRequest"}'`+
				` hx-confirm="Unbind keeps the UA-Hub user and the bridge cache row, but clears the mapping and re-queues them in Needs Match so you can pick the correct Redpoint customer. Continue?">Unbind (re-queue)</button>`,
			HTMLEscape(d.Member.NfcUID),
		))
	} else {
		sb.WriteString(`<button type="button" class="btn btn-warning btn-sm" disabled` +
			` title="No UA-Hub mapping to unbind">Unbind (re-queue)</button>`)
	}

	// Reactivate: only when the UA user is currently deactivated
	if d.Mapping != nil && d.UAUser != nil && d.UAUser.Status == "DEACTIVATED" {
		sb.WriteString(fmt.Sprintf(
			`<button type="button" class="btn btn-primary btn-sm"`+
				` hx-post="/ui/frag/member/%s/reactivate"`+
				` hx-target="#member-detail" hx-swap="innerHTML"`+
				` hx-headers='{"X-Requested-With":"XMLHttpRequest"}'`+
				` hx-confirm="Flip the UA-Hub user status from DEACTIVATED back to ACTIVE? This undoes a prior Skip.">Reactivate UA-Hub user</button>`,
			HTMLEscape(d.Member.NfcUID),
		))
	} else {
		reason := "User is not deactivated"
		if d.Mapping == nil {
			reason = "No UA-Hub mapping"
		} else if d.UAUser == nil {
			reason = "UA-Hub mirror row missing — run a sync first"
		}
		sb.WriteString(fmt.Sprintf(
			`<button type="button" class="btn btn-primary btn-sm" disabled title="%s">Reactivate UA-Hub user</button>`,
			HTMLEscape(reason),
		))
	}

	// Reassign NFC: requires Mapping + UAUser (we need both sides for the audit trail)
	if d.Mapping != nil && d.UAUser != nil {
		sb.WriteString(fmt.Sprintf(
			`<button type="button" class="btn btn-primary btn-sm"`+
				` hx-get="/ui/frag/member/%s/reassign"`+
				` hx-target="#member-detail" hx-swap="innerHTML"`+
				` hx-headers='{"X-Requested-With":"XMLHttpRequest"}'>Reassign NFC card</button>`,
			HTMLEscape(d.Member.NfcUID),
		))
	} else {
		reason := "UA-Hub mirror row missing — run a sync first"
		if d.Mapping == nil {
			reason = "No UA-Hub mapping"
		}
		sb.WriteString(fmt.Sprintf(
			`<button type="button" class="btn btn-primary btn-sm" disabled title="%s">Reassign NFC card</button>`,
			HTMLEscape(reason),
		))
	}

	// Remove is always available.
	sb.WriteString(fmt.Sprintf(
		`<button type="button" class="btn btn-danger btn-sm"`+
			` hx-delete="/members/%s"`+
			` hx-target="#member-detail" hx-swap="innerHTML"`+
			` hx-headers='{"X-Requested-With":"XMLHttpRequest"}'`+
			` hx-confirm="Remove %s from the bridge cache? The UA-Hub user survives — next ingest will either re-bind or drop them in Needs Match.">Remove from cache</button>`,
		HTMLEscape(d.Member.NfcUID),
		HTMLEscape(d.Member.Name),
	))

	sb.WriteString(`</div>`)

	// ── Audit trail ─────────────────────────────────────────────────
	sb.WriteString(`<hr style="margin: 16px 0; border: none; border-top: 1px solid var(--border, #333)">`)
	sb.WriteString(`<h4 style="margin: 0 0 8px">Audit trail</h4>`)
	if d.Mapping == nil || len(d.Audit) == 0 {
		sb.WriteString(`<p style="color: var(--text-muted); font-size: 13px; margin: 0">`)
		if d.Mapping == nil {
			sb.WriteString(`No audit rows — audit is keyed on UA-Hub user ID and this member has no mapping.`)
		} else {
			sb.WriteString(`No audit entries yet for this user.`)
		}
		sb.WriteString(`</p>`)
	} else {
		sb.WriteString(`<table style="font-size: 12px"><thead><tr>`)
		sb.WriteString(`<th>When</th><th>Field</th><th>Before</th><th>After</th><th>Source</th>`)
		sb.WriteString(`</tr></thead><tbody>`)
		for _, a := range d.Audit {
			beforeVal := HTMLEscape(a.BeforeVal)
			if a.BeforeVal == "" {
				beforeVal = `<span style="color: var(--text-muted)">—</span>`
			}
			afterVal := HTMLEscape(a.AfterVal)
			if a.AfterVal == "" {
				afterVal = `<span style="color: var(--text-muted)">—</span>`
			}
			sb.WriteString(fmt.Sprintf(
				`<tr><td>%s</td><td><code>%s</code></td><td>%s</td><td>%s</td><td><code>%s</code></td></tr>`,
				HTMLEscape(a.Timestamp),
				HTMLEscape(a.Field),
				beforeVal,
				afterVal,
				HTMLEscape(a.Source),
			))
		}
		sb.WriteString(`</tbody></table>`)
	}

	sb.WriteString(`</div>`)
	return sb.String()
}

// ─── Member reassign panel (v0.5.9 #10) ───────────────────────────────
//
// MemberReassignData drives the NFC reassignment picker. Flow:
//
//   1. Operator clicks "Reassign NFC card" in the detail panel →
//      handleFragMemberReassign renders this fragment with the NFC UID,
//      the current owner's name/UA ID, an empty candidate list, and an
//      empty search query.
//
//   2. Operator types in the search box + submits →
//      handleFragMemberReassignSearch walks ua_users (via
//      store.SearchUAUsers), filters out the current owner (no point
//      reassigning to themselves), and re-renders this fragment with
//      the candidate list populated.
//
//   3. Operator clicks "Reassign here" on a candidate row →
//      handleFragMemberReassignConfirm calls unifi.AssignNFCCard with
//      forceAdd=true, writes two audit rows (old+new UA user IDs), and
//      swaps in an AlertFragment plus the HX-Trigger:member-updated
//      header so the member table refreshes.
//
// Cancel button goes back to the detail panel via hx-get on
// /ui/frag/member/{nfcUid}/detail. Nothing is committed until step 3,
// so the operator can back out at any point.
type MemberReassignData struct {
	NfcUID         string
	CurrentUserID  string // UA-Hub ID of the current card owner
	CurrentName    string // display name of the current owner
	CurrentMember  string // bridge-side member name (from the member row)
	Query          string
	Candidates     []MemberReassignCandidate
	ErrorMessage   string // non-empty → rendered as an inline alert above the search box
}

// MemberReassignCandidate is one UA-Hub user in the reassign picker.
// HasExistingCard is surfaced in the UI as a "will reassign from other
// user too" hint — the UA-Hub API handles the atomic swap, but staff
// benefit from seeing that it's a three-way move.
type MemberReassignCandidate struct {
	UAUserID        string
	Name            string
	Email           string
	Status          string // "ACTIVE" | "DEACTIVATED" | ""
	HasExistingCard bool
}

// MemberReassignFragment renders the reassign picker. Swapped into the
// #member-detail sink, so Cancel rehydrates the detail panel.
func MemberReassignFragment(d MemberReassignData) string {
	var sb strings.Builder

	sb.WriteString(`<div class="card" id="member-reassign-panel">`)
	sb.WriteString(fmt.Sprintf(
		`<div style="display: flex; justify-content: space-between; align-items: flex-start">`+
			`<div>`+
			`<h3 style="margin: 0">Reassign NFC card</h3>`+
			`<p style="color: var(--text-muted); margin: 4px 0 0; font-size: 13px">`+
			`Card <code>%s</code> is currently bound to <strong>%s</strong> `+
			`<code>(%s)</code>. Pick a different UA-Hub user below to move the card.`+
			`</p>`+
			`</div>`+
			`<button type="button" class="btn btn-sm"`+
			` hx-get="/ui/frag/member/%s/detail"`+
			` hx-target="#member-detail" hx-swap="innerHTML"`+
			` hx-headers='{"X-Requested-With":"XMLHttpRequest"}'`+
			` title="Cancel reassignment and return to the detail panel">Cancel</button>`+
			`</div>`,
		HTMLEscape(d.NfcUID),
		HTMLEscape(d.CurrentName),
		HTMLEscape(d.CurrentUserID),
		HTMLEscape(d.NfcUID),
	))

	if d.ErrorMessage != "" {
		sb.WriteString(fmt.Sprintf(
			`<div class="alert alert-error" style="margin-top: 12px">%s</div>`,
			HTMLEscape(d.ErrorMessage),
		))
	}

	// Inline search form — POSTs to the search endpoint; response
	// replaces the whole fragment so the state reflects the most
	// recent query. CSRF-protected via X-Requested-With + the double-
	// submit token from layout.html.
	sb.WriteString(fmt.Sprintf(
		`<form hx-post="/ui/frag/member/%s/reassign/search"`+
			` hx-target="#member-detail" hx-swap="innerHTML"`+
			` hx-headers='{"X-Requested-With":"XMLHttpRequest"}' style="margin: 12px 0; display: flex; gap: 8px">`+
			`<input type="text" name="q" value="%s" placeholder="Search UA-Hub by name or email…" style="flex: 1" autofocus>`+
			`<button class="btn btn-primary btn-sm" type="submit">Search</button>`+
			`</form>`,
		HTMLEscape(d.NfcUID), HTMLEscape(d.Query),
	))

	if d.Query == "" {
		sb.WriteString(`<p style="color: var(--text-muted); padding: 8px 0; font-size: 13px">` +
			`Type a name or email above to find the intended card owner. The current owner is excluded automatically.` +
			`</p>`)
	} else if len(d.Candidates) == 0 {
		sb.WriteString(fmt.Sprintf(
			`<p style="color: var(--text-muted); padding: 8px 0; font-size: 13px">`+
				`No UA-Hub users matched <code>%s</code>. Try a shorter query, or make sure the user has been synced from UA-Hub.`+
				`</p>`,
			HTMLEscape(d.Query),
		))
	} else {
		sb.WriteString(`<table><thead><tr>`)
		sb.WriteString(`<th>Name</th><th>Email</th><th>Status</th><th>UA-Hub ID</th><th></th>`)
		sb.WriteString(`</tr></thead><tbody>`)
		for _, c := range d.Candidates {
			statusBadge := `<span style="color: var(--text-muted)">(no status)</span>`
			switch c.Status {
			case "ACTIVE":
				statusBadge = `<span class="badge badge-active">ACTIVE</span>`
			case "DEACTIVATED":
				statusBadge = `<span class="badge badge-denied">DEACTIVATED</span>`
			default:
				if c.Status != "" {
					statusBadge = HTMLEscape(c.Status)
				}
			}

			// Swap hint — if the target already has a card, UA-Hub's
			// force_add will atomically unbind it from the target's
			// previous card too. Surfacing this up-front stops a "why
			// did my own card stop working?" support ticket.
			swapHint := ""
			if c.HasExistingCard {
				swapHint = ` <span title="This user already has an NFC card. Reassigning will unbind their existing card in UA-Hub." style="color: var(--warn, #d4a72c); font-size: 12px">⚠ has existing card</span>`
			}

			// Confirm prompt surfaces the two-sided effect so staff
			// doesn't reassign by accident.
			confirmMsg := fmt.Sprintf(
				"Move card %s from %s to %s? This updates UA-Hub and writes two audit rows.",
				d.NfcUID, d.CurrentName, c.Name,
			)
			if c.HasExistingCard {
				confirmMsg += " The target's existing card will be unbound."
			}

			sb.WriteString(fmt.Sprintf(
				`<tr>`+
					`<td>%s%s</td>`+
					`<td>%s</td>`+
					`<td>%s</td>`+
					`<td><code style="font-size: 11px">%s</code></td>`+
					`<td>`+
					`<form hx-post="/ui/frag/member/%s/reassign/confirm"`+
					` hx-target="#member-detail" hx-swap="innerHTML"`+
					` hx-headers='{"X-Requested-With":"XMLHttpRequest"}'`+
					` hx-confirm="%s" style="margin: 0">`+
					`<input type="hidden" name="targetUaUserId" value="%s">`+
					`<button type="submit" class="btn btn-primary btn-sm">Reassign here</button>`+
					`</form>`+
					`</td>`+
					`</tr>`,
				HTMLEscape(c.Name), swapHint,
				HTMLEscape(c.Email),
				statusBadge,
				HTMLEscape(c.UAUserID),
				HTMLEscape(d.NfcUID),
				HTMLEscape(confirmMsg),
				HTMLEscape(c.UAUserID),
			))
		}
		sb.WriteString(`</tbody></table>`)
	}

	sb.WriteString(`</div>`)
	return sb.String()
}

