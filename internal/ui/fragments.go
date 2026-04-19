package ui

import (
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
		return `<p style="color: var(--text-muted); padding: 12px 0">No members enrolled. Run an ingest or add members manually.</p>`
	}

	var sb strings.Builder
	sb.WriteString(`<table><thead><tr><th>Name</th><th>NFC UID</th><th>Status</th><th>Membership</th><th>Last Check-in</th><th></th></tr></thead><tbody>`)
	for _, r := range rows {
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
		sb.WriteString(fmt.Sprintf(`<tr>
            <td>%s</td><td><code>%s</code></td>
            <td><span class="badge %s">%s</span></td>
            <td>%s</td><td>%s</td>
            <td><button class="btn btn-danger btn-sm"
                hx-delete="/members/%s"
                hx-target="closest tr"
                hx-swap="outerHTML swap:0.3s"
                hx-confirm="Remove %s from enrolled members?"
                hx-headers='{"X-Requested-With":"XMLHttpRequest"}'>Remove</button></td>
        </tr>`,
			HTMLEscape(r.Name), HTMLEscape(r.NfcUID),
			badgeClass, HTMLEscape(r.BadgeStatus),
			HTMLEscape(r.BadgeName), HTMLEscape(lastCI),
			HTMLEscape(r.NfcUID), HTMLEscape(r.Name)))
	}
	sb.WriteString(`</tbody></table>`)
	return sb.String()
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
		return `<p style="color: var(--text-muted); padding: 12px 0">No members enrolled. Run an ingest or add members manually.</p>`
	}

	var sb strings.Builder

	// Full table on first page (offset == 0)
	if offset == 0 {
		sb.WriteString(`<table><thead><tr><th>Name</th><th>NFC UID</th><th>Status</th><th>Membership</th><th>Last Check-in</th><th></th></tr></thead><tbody>`)
	}

	// Render rows
	for _, r := range rows {
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
		sb.WriteString(fmt.Sprintf(`<tr>
            <td>%s</td><td><code>%s</code></td>
            <td><span class="badge %s">%s</span></td>
            <td>%s</td><td>%s</td>
            <td><button class="btn btn-danger btn-sm"
                hx-delete="/members/%s"
                hx-target="closest tr"
                hx-swap="outerHTML swap:0.3s"
                hx-confirm="Remove %s from enrolled members?"
                hx-headers='{"X-Requested-With":"XMLHttpRequest"}'>Remove</button></td>
        </tr>`,
			HTMLEscape(r.Name), HTMLEscape(r.NfcUID),
			badgeClass, HTMLEscape(r.BadgeStatus),
			HTMLEscape(r.BadgeName), HTMLEscape(lastCI),
			HTMLEscape(r.NfcUID), HTMLEscape(r.Name)))
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

	var sb strings.Builder
	sb.WriteString(`<table><thead><tr><th>Name</th><th>Email</th><th>Status</th><th></th></tr></thead><tbody>`)
	for _, r := range results {
		status := `<span class="badge badge-denied">Not enrolled</span>`
		action := fmt.Sprintf(`<button class="btn btn-success btn-sm" onclick="document.querySelector('[name=redpointId]').value='%s'; document.querySelector('[name=firstName]').value='%s'">Select</button>`,
			HTMLEscape(r.RedpointID), HTMLEscape(r.Name))
		if r.InCache {
			status = fmt.Sprintf(`<span class="badge badge-active">Enrolled (%s)</span>`, HTMLEscape(r.NfcUID))
			action = ""
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
// "failed", "running", or empty. createdAt is the RFC3339 string the
// jobs table stamps at CreateJob. A nil/empty job renders the "Never
// run" pill.
func SyncLastRunPill(jobType, status, createdAt, errMsg string) string {
	id := fmt.Sprintf("sync-pill-%s", HTMLEscape(jobType))
	if status == "" {
		return fmt.Sprintf(
			`<span id="%s" class="badge" style="background:#eceff1;color:var(--text-muted);font-weight:500">Never run</span>`,
			id)
	}
	rel := FormatRelative(createdAt)
	switch status {
	case "running":
		return fmt.Sprintf(
			`<span id="%s" class="badge badge-running" title="Started %s">⟳ Running · started %s</span>`,
			id, HTMLEscape(createdAt), HTMLEscape(rel))
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

// FormatRelative takes an RFC3339 timestamp and returns a compact
// human-readable relative-time string suitable for the sync pill:
// "just now", "12m ago", "2h ago", "3d ago". Empty or unparseable
// input returns "never".
func FormatRelative(ts string) string {
	if ts == "" {
		return "never"
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
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

// ─── "New Member" provisioning UI (C2 Layer 4d) ───────────────────────
// The five fragments below back the /ui/members/new orchestration in
// internal/api/server.go. Each fragment is the response body for one
// rung of the staff workflow:
//
//   1. EmailLookupOK / EmailLookupError       — live validation result
//   2. PostCreateTapWidget                    — "user created, pick reader"
//   3. EnrollmentPollingFragment              — running poll, waits for tap
//   4. EnrollmentCompleteFragment             — card bound, done
//   5. EnrollmentFailedFragment               — timeout / cancel / error
//
// All five are intentionally self-contained: each carries the `id` of
// the swap target so the next mutation can re-render the same slot
// without the calling page needing to thread state. This matches the
// "Needs Match" panel pattern that the staff already know.

// MemberLookupResult is the single Redpoint customer surfaced by the
// live email validation step. Empty UserID means "no clean match" — the
// fragment renders a blocking validation error instead.
type MemberLookupResult struct {
	RedpointCustomerID string
	Name               string
	Email              string
	Active             bool
	BadgeName          string
	BadgeStatus        string
	// AmbiguousCount > 0 means email matched multiple Redpoint customers
	// and name disambiguation didn't land on one. Drives a different
	// error message (so staff knows to pass first+last too) than a flat
	// "no match found".
	AmbiguousCount int
}

// EmailLookupFragment renders the live email-validation result that
// /ui/members/new/lookup returns. ok==true → green "this is who I'd
// create them as" hint; ok==false → red blocking validation message
// the staff must resolve before submitting the form.
func EmailLookupFragment(ok bool, msg string, hit *MemberLookupResult) string {
	if !ok {
		return fmt.Sprintf(`<div class="alert alert-error" data-lookup="error">%s</div>`,
			HTMLEscape(msg))
	}
	if hit == nil {
		// Defensive: ok==true must come with a non-nil hit. Render an
		// alert so the operator sees the bug rather than a silent blank.
		return `<div class="alert alert-error" data-lookup="error">internal: lookup ok=true but no result</div>`
	}
	activeBadge := `<span class="badge badge-denied">inactive</span>`
	if hit.Active {
		activeBadge = `<span class="badge badge-active">active</span>`
	}
	badgeChip := HTMLEscape(hit.BadgeName)
	if hit.BadgeStatus != "" {
		badgeChip = fmt.Sprintf(`%s <span style="color: var(--text-muted); font-size: 11px">(%s)</span>`,
			HTMLEscape(hit.BadgeName), HTMLEscape(hit.BadgeStatus))
	}
	return fmt.Sprintf(
		`<div class="alert alert-success" data-lookup="ok"`+
			` data-redpoint-customer-id="%s">`+
			`Will create UA-Hub user → Redpoint <strong>%s</strong> `+
			`(%s) %s · %s`+
			`</div>`,
		HTMLEscape(hit.RedpointCustomerID),
		HTMLEscape(hit.Name),
		HTMLEscape(hit.Email),
		badgeChip,
		activeBadge,
	)
}

// DoorOption is one entry in the reader picker for the post-create
// fragment. Surfaces the human-friendly door name plus the device ID
// the §6.2 enrollment call needs.
type DoorOption struct {
	DeviceID string
	Name     string
}

// PostCreateFragment renders the "user created, pick a reader and tap"
// widget that POST /ui/members/new returns. It's the bridge between
// "user exists in UA-Hub" and "card is enrolled and bound" — staff
// picks a reader and clicks "Start enrollment", which triggers the
// §6.2 call.
func PostCreateFragment(uaUserID, displayName, redpointCustomerID string, readers []DoorOption) string {
	var sb strings.Builder
	sb.WriteString(`<div class="card" id="members-new-result">`)
	sb.WriteString(fmt.Sprintf(
		`<div class="alert alert-success">`+
			`Created UA-Hub user <strong>%s</strong> `+
			`(<code>%s</code>) → Redpoint <code>%s</code>. `+
			`Now bind their NFC card.`+
			`</div>`,
		HTMLEscape(displayName),
		HTMLEscape(uaUserID),
		HTMLEscape(redpointCustomerID),
	))
	sb.WriteString(`<form hx-post="/ui/members/new/`)
	sb.WriteString(HTMLEscape(uaUserID))
	sb.WriteString(`/enroll" hx-target="#members-new-result" hx-swap="outerHTML"`)
	sb.WriteString(` hx-headers='{"X-Requested-With":"XMLHttpRequest"}'>`)
	sb.WriteString(`<div class="form-row">`)
	sb.WriteString(`<div class="form-group"><label>Reader</label>`)
	sb.WriteString(`<select name="device_id" required>`)
	if len(readers) == 0 {
		sb.WriteString(`<option value="">(no readers found)</option>`)
	}
	for _, r := range readers {
		sb.WriteString(fmt.Sprintf(`<option value="%s">%s</option>`,
			HTMLEscape(r.DeviceID), HTMLEscape(r.Name)))
	}
	sb.WriteString(`</select></div>`)
	sb.WriteString(`<button type="submit" class="btn btn-primary">Start enrollment</button>`)
	sb.WriteString(`</div></form></div>`)
	return sb.String()
}

// EnrollmentPollingFragment renders the "waiting for tap" panel that
// POST /ui/members/new/{id}/enroll returns. It contains an HTMX
// hx-trigger="every 500ms" pointing at the poll endpoint, so the same
// slot will re-render itself with a new fragment as soon as the tap
// is detected (or the timeout fires).
func EnrollmentPollingFragment(uaUserID, displayName, sessionID string) string {
	pollURL := fmt.Sprintf(`/ui/members/new/%s/enroll/%s/poll`,
		HTMLEscape(uaUserID), HTMLEscape(sessionID))
	cancelURL := fmt.Sprintf(`/ui/members/new/%s/enroll/%s`,
		HTMLEscape(uaUserID), HTMLEscape(sessionID))
	return fmt.Sprintf(`<div class="card" id="members-new-result">`+
		`<div class="alert" style="background: var(--info-bg, #1e293b)">`+
		`<strong>Tap a card on the reader now.</strong> `+
		`Bridge is waiting for %s — UA-Hub session <code>%s</code>.`+
		`</div>`+
		`<div hx-get="%s" hx-trigger="load, every 500ms" hx-swap="outerHTML"`+
		` hx-target="#members-new-result"`+
		` hx-headers='{"X-Requested-With":"XMLHttpRequest"}'>`+
		`<span class="spinner"></span> waiting for card tap…`+
		`</div>`+
		`<button class="btn btn-danger btn-sm" style="margin-top: 12px"`+
		` hx-delete="%s" hx-target="#members-new-result" hx-swap="outerHTML"`+
		` hx-headers='{"X-Requested-With":"XMLHttpRequest"}'`+
		` hx-confirm="Cancel enrollment? The UA-Hub user stays, but the card is not bound.">Cancel</button>`+
		`</div>`,
		HTMLEscape(displayName),
		HTMLEscape(sessionID),
		pollURL,
		cancelURL,
	)
}

// EnrollmentCompleteFragment renders the terminal "all done" panel
// after AssignNFCCard succeeds. No more polling — staff can close the
// page or click "Add another".
func EnrollmentCompleteFragment(uaUserID, displayName, token string) string {
	return fmt.Sprintf(`<div class="card" id="members-new-result">`+
		`<div class="alert alert-success">`+
		`<strong>%s</strong> is enrolled — UA-Hub user <code>%s</code>, `+
		`card token <code>%s</code>. They can tap in now.`+
		`</div>`+
		`<a class="btn btn-primary" href="/ui/members/new">Add another</a>`+
		`</div>`,
		HTMLEscape(displayName),
		HTMLEscape(uaUserID),
		HTMLEscape(token),
	)
}

// EnrollmentFailedFragment renders the terminal error/cancel panel.
// Used by both the timeout path (poll exceeds the deadline) and the
// "card already bound to someone else" guard (§6.7 result conflicts
// with §3.7's intent). Staff can retry the enrollment without losing
// the just-created UA-Hub user.
func EnrollmentFailedFragment(uaUserID, displayName, message string, readers []DoorOption) string {
	var sb strings.Builder
	sb.WriteString(`<div class="card" id="members-new-result">`)
	sb.WriteString(fmt.Sprintf(`<div class="alert alert-error">%s</div>`, HTMLEscape(message)))
	sb.WriteString(fmt.Sprintf(
		`<p style="margin: 8px 0">UA-Hub user <code>%s</code> (<strong>%s</strong>) was created. Pick a reader and try again, or close this page.</p>`,
		HTMLEscape(uaUserID), HTMLEscape(displayName)))
	sb.WriteString(`<form hx-post="/ui/members/new/`)
	sb.WriteString(HTMLEscape(uaUserID))
	sb.WriteString(`/enroll" hx-target="#members-new-result" hx-swap="outerHTML"`)
	sb.WriteString(` hx-headers='{"X-Requested-With":"XMLHttpRequest"}'>`)
	sb.WriteString(`<div class="form-row">`)
	sb.WriteString(`<div class="form-group"><label>Reader</label>`)
	sb.WriteString(`<select name="device_id" required>`)
	if len(readers) == 0 {
		sb.WriteString(`<option value="">(no readers found)</option>`)
	}
	for _, r := range readers {
		sb.WriteString(fmt.Sprintf(`<option value="%s">%s</option>`,
			HTMLEscape(r.DeviceID), HTMLEscape(r.Name)))
	}
	sb.WriteString(`</select></div>`)
	sb.WriteString(`<button type="submit" class="btn btn-primary">Retry enrollment</button>`)
	sb.WriteString(`</div></form></div>`)
	return sb.String()
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
				` hx-swap="innerHTML"`+
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
			`<input type="text" name="q" value="%s" placeholder="Search Redpoint by name…" style="width: 60%%; margin-right: 8px">`+
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

