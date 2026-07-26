package ui

// Tests for the UI-presentation overhaul: status vocabulary, grace-cell
// alert-only rendering, per-type unstick thresholds, inline failure
// errors, the pending-summary chip, and the poller visibility gates.

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestStatusBadge_Vocabulary — internal enum values render as staff-
// readable labels (render-time only; stored values untouched), with the
// raw value preserved in the title attribute and the class keyed so
// DELETED/unknown can never render as a green pill.
func TestStatusBadge_Vocabulary(t *testing.T) {
	tests := []struct {
		status    string
		wantLabel string
		wantClass string
	}{
		{"ACTIVE", "ACTIVE", "badge-active"},
		{"FROZEN", "FROZEN", "badge-frozen"},
		{"EXPIRED", "EXPIRED", "badge-expired"},
		{"PENDING_SYNC", "Awaiting first sync", "badge-pending"},
		{"DELETED", "Not in Redpoint", "badge-denied"},
		{"SOME_FUTURE_STATE", "SOME_FUTURE_STATE", "badge-muted"},
	}
	for _, tc := range tests {
		t.Run(tc.status, func(t *testing.T) {
			got := statusBadge(tc.status)
			if !strings.Contains(got, ">"+tc.wantLabel+"<") {
				t.Errorf("statusBadge(%q) = %q, want label %q", tc.status, got, tc.wantLabel)
			}
			if !strings.Contains(got, tc.wantClass) {
				t.Errorf("statusBadge(%q) = %q, want class %q", tc.status, got, tc.wantClass)
			}
			if !strings.Contains(got, `title="`+tc.status+`"`) {
				t.Errorf("statusBadge(%q) = %q, want raw value in title", tc.status, got)
			}
		})
	}
}

// TestMemberRow_Presentation — the members table row de-emphasizes the
// NFC UID, renders the vocabulary-mapped status, uses the quiet outline
// Remove button (with confirm), and formats the last check-in.
func TestMemberRow_Presentation(t *testing.T) {
	recent := time.Now().UTC().Add(-30 * time.Minute).Format(time.RFC3339)
	out := MemberTableFragment([]MemberRow{{
		NfcUID:      "04AABBCC",
		Name:        "Alice Smith",
		BadgeStatus: "DELETED",
		BadgeName:   "",
		LastCheckIn: recent,
	}})

	if !strings.Contains(out, "Not in Redpoint") {
		t.Errorf("DELETED should render as 'Not in Redpoint'; output:\n%s", out)
	}
	if strings.Contains(out, `class="badge badge-active" title="DELETED"`) {
		t.Errorf("DELETED must not render green; output:\n%s", out)
	}
	if !strings.Contains(out, `btn-outline-danger`) {
		t.Errorf("row Remove button should use the quiet outline style; output:\n%s", out)
	}
	if !strings.Contains(out, `hx-confirm="Remove Alice Smith`) {
		t.Errorf("row Remove button should keep its confirm; output:\n%s", out)
	}
	// NFC UID is de-emphasized: small muted <code>.
	if !strings.Contains(out, `<code style="font-size:11px; color: var(--text-muted)">04AABBCC</code>`) {
		t.Errorf("NFC UID cell should be small + muted; output:\n%s", out)
	}
	// Last check-in renders relative + local, not the raw UTC stamp.
	if !strings.Contains(out, "30m ago") {
		t.Errorf("last check-in should render relative; output:\n%s", out)
	}
	// Empty membership renders an em-dash placeholder, not a blank hole.
	if !strings.Contains(out, "Populates on next status refresh") {
		t.Errorf("empty membership should render the placeholder; output:\n%s", out)
	}
}

// TestCheckInTable_LocalTimes — raw store timestamps render via
// FormatRecent with the raw stamp in the title attribute.
func TestCheckInTable_LocalTimes(t *testing.T) {
	ts := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)
	out := CheckInTableFragment([]CheckInRow{{
		Time: ts, Name: "Alice", NfcUID: "04AA", Door: "Front", Result: "allowed",
	}})
	if !strings.Contains(out, "5m ago · ") {
		t.Errorf("check-in time should render relative + local; output:\n%s", out)
	}
	if !strings.Contains(out, `title="`+ts+`"`) {
		t.Errorf("raw timestamp should survive in title; output:\n%s", out)
	}
	if strings.Contains(out, ">"+ts+"<") {
		t.Errorf("raw UTC timestamp must not render as cell text; output:\n%s", out)
	}
}

// TestGraceCell_AlertOnly — overdue rows warn and say "resolve
// manually"; nothing may promise an automatic deactivation.
func TestGraceCell_AlertOnly(t *testing.T) {
	overdue := time.Now().UTC().Add(-3 * 24 * time.Hour).Format(time.RFC3339)
	got := graceCell(overdue)
	if !strings.Contains(got, "badge-failed") {
		t.Errorf("overdue grace should render the warning style; got %q", got)
	}
	if !strings.Contains(got, "Past grace (3d) — resolve manually") {
		t.Errorf("overdue grace should read 'Past grace (3d) — resolve manually'; got %q", got)
	}
	if strings.Contains(strings.ToLower(got), "deactivate") &&
		!strings.Contains(got, "Nothing is deactivated automatically") {
		t.Errorf("grace cell must not promise deactivation; got %q", got)
	}

	future := time.Now().UTC().Add(2 * 24 * time.Hour).Format(time.RFC3339)
	got = graceCell(future)
	if strings.Contains(got, "badge-failed") {
		t.Errorf("future grace should not warn; got %q", got)
	}
}

// TestNeedsMatchList_NoDeactivationPromise — the queue's column header
// says when grace ends, not that anything deactivates.
func TestNeedsMatchList_NoDeactivationPromise(t *testing.T) {
	out := NeedsMatchListFragment([]NeedsMatchRow{{
		UAUserID:   "ua-1",
		UAName:     "Petrov Kid",
		Reason:     "ambiguous_email",
		FirstSeen:  "2026-07-01T12:00:00Z",
		GraceUntil: "2026-07-08T12:00:00Z",
	}})
	if strings.Contains(out, "Deactivates") {
		t.Errorf("column header must not promise deactivation; output:\n%s", out)
	}
	if !strings.Contains(out, "Grace Ends") {
		t.Errorf("want a 'Grace Ends' column header; output:\n%s", out)
	}
}

// TestNeedsMatchDetail_SnoozeVocabulary — the defer button reads as an
// alert snooze, matching the alert-only expiry semantics.
func TestNeedsMatchDetail_SnoozeVocabulary(t *testing.T) {
	out := NeedsMatchDetailFragment("ua-1", "Kid", "kid@example.com",
		"2026-07-01T12:00:00Z", "2026-07-08T12:00:00Z", "ambiguous_email", nil, "")
	if !strings.Contains(out, "Snooze alerts 7 days") {
		t.Errorf("defer button should read 'Snooze alerts 7 days'; output:\n%s", out)
	}
	if strings.Contains(out, "Defer 7 days") {
		t.Errorf("old defer label still present; output:\n%s", out)
	}
}

// TestUnstickThreshold_PerJobType — fast jobs surface Clear stuck at
// ~2min; slow walks keep their generous window.
func TestUnstickThreshold_PerJobType(t *testing.T) {
	tests := []struct {
		jobType   string
		age       time.Duration
		wantStuck bool
	}{
		{"cache_sync", 3 * time.Minute, true},   // fast job, 2min threshold
		{"unifi_ingest", 3 * time.Minute, true}, // fast job, 2min threshold
		{"cache_sync", 1 * time.Minute, false},  // still fresh
		{"status_sync", 3 * time.Minute, false}, // 10min threshold
		{"status_sync", 11 * time.Minute, true}, //
		{"ua_hub_sync", 5 * time.Minute, false}, // legit 4-5min walk
		{"ua_hub_sync", 16 * time.Minute, true}, // 15min threshold
		{"directory_sync", 12 * time.Minute, false},
		{"unknown_type", 11 * time.Minute, true}, // falls back to 10min default
	}
	for _, tc := range tests {
		t.Run(tc.jobType+"/"+tc.age.String(), func(t *testing.T) {
			createdAt := time.Now().UTC().Add(-tc.age).Format(time.RFC3339)
			pill := SyncLastRunPillFull(tc.jobType, "running", createdAt, "", "")
			gotStuck := strings.Contains(pill, "Clear stuck")
			if gotStuck != tc.wantStuck {
				t.Errorf("%s at %s: stuck=%v, want %v; pill: %s",
					tc.jobType, tc.age, gotStuck, tc.wantStuck, pill)
			}
		})
	}
}

// TestFailedPill_InlineError — the first ~60 chars of the error render
// inline (tooltips are invisible on touch), full message in the title.
func TestFailedPill_InlineError(t *testing.T) {
	longErr := strings.Repeat("redpoint live query failed: connection refused ", 3)
	pill := SyncLastRunPillFull("cache_sync", "failed",
		time.Now().UTC().Add(-2*time.Minute).Format(time.RFC3339), longErr, "")

	if !strings.Contains(pill, "✗ Failed · 2m ago · ") {
		t.Errorf("failed pill should carry an inline error segment; pill: %s", pill)
	}
	if !strings.Contains(pill, "…") {
		t.Errorf("long error should be truncated with an ellipsis; pill: %s", pill)
	}
	if !strings.Contains(pill, "redpoint live query failed") {
		t.Errorf("error text missing from pill; pill: %s", pill)
	}

	short := SyncLastRunPillFull("cache_sync", "failed",
		time.Now().UTC().Add(-2*time.Minute).Format(time.RFC3339), "boom", "")
	if !strings.Contains(short, "· boom<") {
		t.Errorf("short error should render inline untruncated; pill: %s", short)
	}
}

// TestPendingSummaryFragment — the sync-page chip links to Needs Match
// and pluralizes; zero renders the calm empty state.
func TestPendingSummaryFragment(t *testing.T) {
	if got := PendingSummaryFragment(0); !strings.Contains(got, "No users pending") {
		t.Errorf("zero pending should render the empty state; got %q", got)
	}
	one := PendingSummaryFragment(1)
	if !strings.Contains(one, ">1</span> user pending") {
		t.Errorf("singular form wrong; got %q", one)
	}
	many := PendingSummaryFragment(3)
	if !strings.Contains(many, ">3</span> users pending") ||
		!strings.Contains(many, `href="/ui/needs-match"`) {
		t.Errorf("plural chip should count and link to /ui/needs-match; got %q", many)
	}
}

// TestTemplates_PollersAreVisibilityGated scans every embedded template
// for periodic hx-trigger pollers and requires the visibilityState
// guard, so a future page can't silently reintroduce background-tab
// polling. (The 500ms enrollment poll lives in fragments.go and is
// deliberately exempt: it is short-lived, bounded at 120 polls, and
// pausing it would leave the reader captured in enrollment mode.)
func TestTemplates_PollersAreVisibilityGated(t *testing.T) {
	attr := regexp.MustCompile(`hx-trigger="([^"]*)"`)
	every := regexp.MustCompile(`every \d+m?s`)
	err := fs.WalkDir(templateFS, "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".html") {
			return err
		}
		data, rerr := fs.ReadFile(templateFS, path)
		if rerr != nil {
			return rerr
		}
		for _, m := range attr.FindAllStringSubmatch(string(data), -1) {
			trigger := m[1]
			if every.MatchString(trigger) &&
				!strings.Contains(trigger, "[document.visibilityState === 'visible']") {
				t.Errorf("%s: poller missing visibility gate: %s", path, trigger)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestLayout_PresentationInvariants — the sub-768px font-size:0 rule is
// gone, wide tables scroll in-card, the favicon is inline, and the
// logout control is a quiet link rather than a red button.
func TestLayout_PresentationInvariants(t *testing.T) {
	data, err := fs.ReadFile(templateFS, "templates/layout.html")
	if err != nil {
		t.Fatal(err)
	}
	layout := string(data)

	if strings.Contains(layout, "font-size: 0") || strings.Contains(layout, "font-size:0") {
		t.Error("layout still contains the invisible-nav font-size:0 rule")
	}
	if strings.Contains(layout, "@media (max-width: 768px)") {
		t.Error("layout still contains the broken sub-768px block")
	}
	if !strings.Contains(layout, "overflow-x: auto") {
		t.Error("cards should scroll wide tables via overflow-x: auto")
	}
	if !strings.Contains(layout, `rel="icon"`) {
		t.Error("layout is missing the inline favicon")
	}
	if !strings.Contains(layout, `class="logout-link"`) {
		t.Error("logout should be the quiet .logout-link, not a button pill")
	}
	if strings.Contains(layout, `btn-danger btn-sm" style="width:100%"`) {
		t.Error("old red logout button still present")
	}
	// Active-nav + title sync script present.
	if !strings.Contains(layout, "htmx:pushedIntoHistory") {
		t.Error("layout is missing the active-nav/title sync script")
	}
}

// TestLoginTemplate_HasFavicon — the login page is served outside the
// layout and needs its own icon.
func TestLoginTemplate_HasFavicon(t *testing.T) {
	data, err := fs.ReadFile(templateFS, "templates/login.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `rel="icon"`) {
		t.Error("login page is missing the inline favicon")
	}
}
