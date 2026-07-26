package ui

// The /ui/health page fragment — the curated replacement for the old
// Metrics page, which dumped raw uppercased Prometheus names, unix-
// seconds gauges, and Go duration strings into cards that reshuffled on
// every poll (map iteration order). Health answers the two operator
// questions the metrics dump never did: "is the bridge healthy right
// now?" and "which go-live capabilities are actually live?"

import (
	"fmt"
	"strings"
	"time"
)

// HealthJobRun is the last-run summary for one background job type.
type HealthJobRun struct {
	Type      string
	Status    string // "", "running", "completed", "failed"
	CreatedAt string // raw store timestamp; rendered relative
}

// HealthData is the view-model for HealthFragment. The three capability
// fields carry RESOLVED values (config.CheckinRecordingLive() etc.), not
// the raw override strings — the fragment renders effect, not config.
type HealthData struct {
	InstanceName string
	ShadowMode   bool // the master switch, shown for context

	CheckinRecordingLive bool
	StatusWritesMode     string // "off" | "activate-only" | "full"
	RecheckUnlockLive    bool

	UnifiConnected bool
	JobRuns        []HealthJobRun
	PendingMatches int
	TapsToday      int
	Uptime         time.Duration
	// AlertingConfigured is true when an ntfy topic is set. The topic
	// value itself is a bearer capability and must never reach the UI.
	AlertingConfigured bool
}

// capabilityRung is one row of the go-live ladder.
type capabilityRung struct {
	Label string
	Desc  string
	Live  bool
}

// ladderRungs resolves the four-rung go-live ladder from the resolved
// capability values. Order matches the intended flip sequence in
// internal/config/config.go.
func ladderRungs(d HealthData) []capabilityRung {
	return []capabilityRung{
		{
			Label: "1 · Check-in recording",
			Desc:  "Taps are recorded in Redpoint",
			Live:  d.CheckinRecordingLive,
		},
		{
			Label: "2 · Status writes: activate-only",
			Desc:  "Renewals regain UA-Hub access automatically",
			Live:  d.StatusWritesMode == "activate-only" || d.StatusWritesMode == "full",
		},
		{
			Label: "3 · Status writes: full",
			Desc:  "Lapsed members lose UA-Hub access (mass-deactivation guard applies)",
			Live:  d.StatusWritesMode == "full",
		},
		{
			Label: "4 · Recheck unlock",
			Desc:  "Denied tap + live Redpoint confirm reactivates and unlocks the door",
			Live:  d.RecheckUnlockLive,
		},
	}
}

// humanizeUptime renders an uptime duration with the noisy sub-second
// tail stripped: "3d 4h 12m", "2h 5m", "45s".
func humanizeUptime(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	secs := int(d.Seconds()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh %dm", days, hours, mins)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, mins)
	case mins > 0:
		return fmt.Sprintf("%dm %ds", mins, secs)
	default:
		return fmt.Sprintf("%ds", secs)
	}
}

// healthJobLabel maps a jobs.Type* string to its staff-facing name.
func healthJobLabel(jobType string) string {
	switch jobType {
	case "cache_sync":
		return "Cache sync"
	case "status_sync":
		return "Status sync"
	case "directory_sync":
		return "Directory sync"
	case "unifi_ingest":
		return "UniFi ingest"
	case "ua_hub_sync":
		return "UA-Hub sync"
	}
	return jobType
}

// HealthFragment renders the /ui/frag/health-summary body: the go-live
// capability ladder, connection/alerting state, last-run times, and the
// day's headline numbers, in one card grid reusing the existing badge
// and stat styles.
func HealthFragment(d HealthData) string {
	var sb strings.Builder

	// ── Capability ladder card ──────────────────────────────────────
	sb.WriteString(`<div class="card">`)
	instance := d.InstanceName
	if instance == "" {
		instance = "prod"
	}
	shadowChip := `<span class="badge badge-active" title="BRIDGE_SHADOW_MODE=false">shadow off</span>`
	if d.ShadowMode {
		shadowChip = `<span class="badge badge-pending" title="BRIDGE_SHADOW_MODE=true">shadow on</span>`
	}
	sb.WriteString(fmt.Sprintf(
		`<h3>Go-live ladder</h3>`+
			`<p style="color: var(--text-muted); margin-bottom: 12px; font-size: 13px">`+
			`Instance <code>%s</code> · master switch: %s. Capabilities flip on one rung at a time; each rung below shows its <em>resolved</em> state.`+
			`</p>`,
		HTMLEscape(instance), shadowChip))

	sb.WriteString(`<div style="display: flex; flex-direction: column; gap: 8px">`)
	for _, rung := range ladderRungs(d) {
		dot := `<span class="status-dot disconnected" style="background: var(--border)"></span>`
		state := `<span class="badge badge-muted">held</span>`
		if rung.Live {
			dot = `<span class="status-dot connected"></span>`
			state = `<span class="badge badge-active">live</span>`
		}
		sb.WriteString(fmt.Sprintf(
			`<div data-rung style="display: flex; align-items: center; gap: 10px; padding: 8px 12px; border: 1px solid var(--border); border-radius: 6px">`+
				`%s<div style="flex: 1"><div style="font-weight: 600; font-size: 13px">%s</div>`+
				`<div style="color: var(--text-muted); font-size: 12px">%s</div></div>%s`+
				`</div>`,
			dot, HTMLEscape(rung.Label), HTMLEscape(rung.Desc), state))
	}
	sb.WriteString(`</div></div>`)

	// ── Now / today card ────────────────────────────────────────────
	connDot := `<span class="status-dot disconnected"></span>Disconnected`
	if d.UnifiConnected {
		connDot = `<span class="status-dot connected"></span>Connected`
	}
	alerting := `<span class="badge badge-pending" title="Set NTFY_TOPIC to enable push alerts">not configured</span>`
	if d.AlertingConfigured {
		alerting = `<span class="badge badge-active">configured</span>`
	}
	sb.WriteString(`<div class="card"><h3>Right now</h3><div class="stats-grid">`)
	sb.WriteString(fmt.Sprintf(
		`<div class="stat-card"><div class="stat-value" style="font-size: 16px">%s</div><div class="stat-label">UniFi WebSocket</div></div>`,
		connDot))
	sb.WriteString(fmt.Sprintf(
		`<div class="stat-card"><div class="stat-value">%d</div><div class="stat-label">Taps Today</div></div>`,
		d.TapsToday))
	pending := fmt.Sprintf(`<a href="/ui/needs-match" style="text-decoration: none; color: inherit">%d</a>`, d.PendingMatches)
	sb.WriteString(fmt.Sprintf(
		`<div class="stat-card"><div class="stat-value">%s</div><div class="stat-label">Pending Needs Match</div></div>`,
		pending))
	sb.WriteString(fmt.Sprintf(
		`<div class="stat-card"><div class="stat-value" style="font-size: 16px">%s</div><div class="stat-label">Uptime</div></div>`,
		HTMLEscape(humanizeUptime(d.Uptime))))
	sb.WriteString(fmt.Sprintf(
		`<div class="stat-card"><div class="stat-value" style="font-size: 16px">%s</div><div class="stat-label">Push Alerting</div></div>`,
		alerting))
	sb.WriteString(`</div></div>`)

	// ── Last runs card ──────────────────────────────────────────────
	sb.WriteString(`<div class="card"><h3>Background jobs</h3>`)
	if len(d.JobRuns) == 0 {
		sb.WriteString(`<p style="color: var(--text-muted); font-size: 13px; margin: 0">No job history yet.</p>`)
	} else {
		sb.WriteString(`<div style="display: grid; grid-template-columns: max-content max-content 1fr; gap: 6px 16px; font-size: 13px; align-items: center">`)
		for _, j := range d.JobRuns {
			glyph := `<span class="badge badge-muted">never run</span>`
			switch j.Status {
			case "completed":
				glyph = fmt.Sprintf(`<span class="badge badge-completed" title="%s">✓ %s</span>`,
					HTMLEscape(j.CreatedAt), HTMLEscape(FormatRelative(j.CreatedAt)))
			case "failed":
				glyph = fmt.Sprintf(`<span class="badge badge-failed" title="%s">✗ %s</span>`,
					HTMLEscape(j.CreatedAt), HTMLEscape(FormatRelative(j.CreatedAt)))
			case "running":
				glyph = fmt.Sprintf(`<span class="badge badge-running" title="%s">⟳ started %s</span>`,
					HTMLEscape(j.CreatedAt), HTMLEscape(FormatRelative(j.CreatedAt)))
			}
			sb.WriteString(fmt.Sprintf(
				`<div style="font-weight: 600">%s</div><div>%s</div><div style="color: var(--text-muted); font-size: 12px">details on <a href="/ui/sync">Sync &amp; Jobs</a></div>`,
				HTMLEscape(healthJobLabel(j.Type)), glyph))
		}
		sb.WriteString(`</div>`)
	}
	sb.WriteString(`</div>`)

	// ── Debug footer ────────────────────────────────────────────────
	sb.WriteString(`<p style="color: var(--text-muted); font-size: 12px">` +
		`Raw metrics for debugging → <a href="/metrics" style="color: var(--text-muted)">/metrics</a></p>`)

	return sb.String()
}
