package ui

// Tests for the Health page fragment (capability ladder + curated
// stats), the Needs Match sidebar badge, the members-new locked-form
// stub, and the PR-B navigation invariants.

import (
	"io/fs"
	"strings"
	"testing"
	"time"
)

// rungStates extracts the live/held state of the four ladder rungs in
// render order by walking the data-rung blocks.
func rungStates(t *testing.T, html string) []bool {
	t.Helper()
	parts := strings.Split(html, "data-rung")
	if len(parts) != 5 { // preamble + 4 rungs
		t.Fatalf("want 4 ladder rungs, found %d; html:\n%s", len(parts)-1, html)
	}
	states := make([]bool, 0, 4)
	for _, p := range parts[1:] {
		live := strings.Contains(p, `>live</span>`)
		held := strings.Contains(p, `>held</span>`)
		if live == held {
			t.Fatalf("rung must be exactly one of live/held; segment:\n%s", p)
		}
		states = append(states, live)
	}
	return states
}

// TestHealthFragment_CapabilityLadder — table-driven over the resolved
// capability values: full shadow, each intermediate rung, and fully
// live. The ladder renders EFFECT, so e.g. StatusWritesMode "full"
// lights both status rungs.
func TestHealthFragment_CapabilityLadder(t *testing.T) {
	tests := []struct {
		name string
		d    HealthData
		want [4]bool // checkin, activate-only, full, recheck
	}{
		{
			name: "full shadow",
			d:    HealthData{ShadowMode: true, StatusWritesMode: "off"},
			want: [4]bool{false, false, false, false},
		},
		{
			name: "rung 1: checkin recording only",
			d: HealthData{ShadowMode: true, CheckinRecordingLive: true,
				StatusWritesMode: "off"},
			want: [4]bool{true, false, false, false},
		},
		{
			name: "rung 2: activate-only",
			d: HealthData{ShadowMode: true, CheckinRecordingLive: true,
				StatusWritesMode: "activate-only"},
			want: [4]bool{true, true, false, false},
		},
		{
			name: "rung 3: full status writes lights rung 2 as well",
			d: HealthData{CheckinRecordingLive: true,
				StatusWritesMode: "full"},
			want: [4]bool{true, true, true, false},
		},
		{
			name: "fully live",
			d: HealthData{CheckinRecordingLive: true,
				StatusWritesMode: "full", RecheckUnlockLive: true},
			want: [4]bool{true, true, true, true},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			html := HealthFragment(tc.d)
			got := rungStates(t, html)
			for i, want := range tc.want {
				if got[i] != want {
					t.Errorf("rung %d live = %v, want %v", i+1, got[i], want)
				}
			}
		})
	}
}

// TestHealthFragment_HeaderAndStats — instance name, shadow chip,
// connection dot, counts, humanized uptime, alerting state, and the
// raw-metrics debug link.
func TestHealthFragment_HeaderAndStats(t *testing.T) {
	d := HealthData{
		InstanceName:       "stage",
		ShadowMode:         true,
		StatusWritesMode:   "off",
		UnifiConnected:     true,
		PendingMatches:     3,
		TapsToday:          41,
		Uptime:             73*time.Hour + 5*time.Minute + 7*time.Second + 123*time.Millisecond,
		AlertingConfigured: true,
		JobRuns: []HealthJobRun{
			{Type: "cache_sync", Status: "completed",
				CreatedAt: time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)},
			{Type: "ua_hub_sync"}, // never run
		},
	}
	html := HealthFragment(d)

	for _, want := range []string{
		"<code>stage</code>",
		"shadow on",
		"Connected",
		">41<",         // taps today
		">3</a>",       // pending count links to needs-match
		"3d 1h 5m",     // humanized, sub-second stripped
		">configured<", // alerting
		`href="/metrics"`,
		"Cache sync",
		"✓ 10m ago",
		"never run",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("HealthFragment missing %q; html:\n%s", want, html)
		}
	}
	if !strings.Contains(html, `href="/ui/needs-match"`) {
		t.Error("pending count should deep-link to /ui/needs-match")
	}
}

// TestHumanizeUptime — sub-second noise stripped, largest-unit-first.
func TestHumanizeUptime(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{5*time.Minute + 5*time.Second + 907*time.Millisecond, "5m 6s"},
		{45 * time.Second, "45s"},
		{2*time.Hour + 5*time.Minute, "2h 5m"},
		{73*time.Hour + 5*time.Minute, "3d 1h 5m"},
		{0, "0s"},
		{-time.Second, "0s"},
	}
	for _, tc := range tests {
		if got := humanizeUptime(tc.in); got != tc.want {
			t.Errorf("humanizeUptime(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestNeedsMatchBadgeFragment — zero renders an invisible span that
// keeps polling; a count renders the pill. The response must NOT carry
// a "load" trigger (an outerHTML-swapped "load" re-fires immediately,
// collapsing the 60s poll into a tight loop) and must keep the
// visibility gate.
func TestNeedsMatchBadgeFragment(t *testing.T) {
	zero := NeedsMatchBadgeFragment(0)
	if strings.Contains(zero, "badge-pending") {
		t.Errorf("zero count should render no visible pill; got %q", zero)
	}
	if !strings.Contains(zero, `id="needs-match-badge"`) ||
		!strings.Contains(zero, "every 60s") {
		t.Errorf("zero count must keep the self-polling span alive; got %q", zero)
	}

	three := NeedsMatchBadgeFragment(3)
	if !strings.Contains(three, ">3</span>") || !strings.Contains(three, "badge-pending") {
		t.Errorf("count should render as a pill; got %q", three)
	}

	for _, frag := range []string{zero, three} {
		if strings.Contains(frag, "load,") || strings.Contains(frag, `"load`) {
			t.Errorf("badge fragment must not re-trigger on load; got %q", frag)
		}
		if !strings.Contains(frag, "[document.visibilityState === 'visible']") {
			t.Errorf("badge fragment must keep the visibility gate; got %q", frag)
		}
		// The span nests inside the nav anchor: without these overrides
		// it inherits hx-target="#content" + hx-push-url="true" and the
		// poll swaps the badge over the page body / rewrites the URL.
		if !strings.Contains(frag, `hx-target="this"`) ||
			!strings.Contains(frag, `hx-push-url="false"`) {
			t.Errorf("badge fragment must override inherited anchor hx attrs; got %q", frag)
		}
	}
}

// TestMembersNewFormLockedFragment — the OOB stub targets the form card
// and offers the start-over escape hatch.
func TestMembersNewFormLockedFragment(t *testing.T) {
	out := MembersNewFormLockedFragment("Happy Path")
	if !strings.Contains(out, `id="members-new-form-card"`) ||
		!strings.Contains(out, `hx-swap-oob="true"`) {
		t.Errorf("stub must OOB-target the form card; got %q", out)
	}
	if !strings.Contains(out, "Happy Path") ||
		!strings.Contains(out, `href="/ui/members/new"`) {
		t.Errorf("stub should name the member and link start-over; got %q", out)
	}
}

// TestPages_DoorsAndMetricsRetired — the templates are gone, so the
// HasPage allowlist 404s the old URLs; health took the metrics slot.
func TestPages_DoorsAndMetricsRetired(t *testing.T) {
	h, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if h.HasPage("doors") {
		t.Error("doors page should be retired")
	}
	if h.HasPage("metrics") {
		t.Error("metrics page should be retired")
	}
	if !h.HasPage("health") {
		t.Error("health page should exist")
	}
}

// TestLayout_NavigationInvariants — PR-B sidebar shape: New Member
// entry, Health replacing Metrics, no Door Policies, and the badge
// poller sitting next to Needs Match with a load trigger (the layout
// copy DOES load-trigger; only the fragment response must not).
func TestLayout_NavigationInvariants(t *testing.T) {
	data, err := fs.ReadFile(templateFS, "templates/layout.html")
	if err != nil {
		t.Fatal(err)
	}
	layout := string(data)

	if strings.Contains(layout, "Door Policies") || strings.Contains(layout, `href="/ui/doors"`) {
		t.Error("Door Policies nav entry should be gone")
	}
	if strings.Contains(layout, `href="/ui/metrics"`) {
		t.Error("Metrics nav entry should be gone")
	}
	if !strings.Contains(layout, `href="/ui/health"`) {
		t.Error("Health nav entry missing")
	}
	if !strings.Contains(layout, `href="/ui/members/new"`) {
		t.Error("New Member nav entry missing")
	}
	if !strings.Contains(layout, `id="needs-match-badge"`) {
		t.Error("Needs Match badge poller missing from the sidebar")
	}
	badgeIdx := strings.Index(layout, `id="needs-match-badge"`)
	loadIdx := strings.Index(layout[badgeIdx:], "load, every 60s")
	if loadIdx < 0 || loadIdx > 200 {
		t.Error("sidebar badge should load-trigger its first fetch")
	}
}
