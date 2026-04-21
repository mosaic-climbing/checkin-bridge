package ui

// v0.5.7.1 coverage for the sync-page "Last run" pill rendering.
// Three moving parts this file pins:
//
//   1. FormatRelative must accept both the RFC3339 form Go-side
//      writes use AND the space-separated SQLite CURRENT_TIMESTAMP
//      default — pre-v0.5.7.1 only RFC3339 parsed, and every "Last
//      run: ⟳ Running · started Xm ago" rendered "never" because
//      created_at was SQLite-formatted.
//
//   2. The running-case renderer must interpolate the current
//      progress phase ("hydrating 450/1500") when provided so staff
//      see the pill tick through phases instead of sitting on a
//      static "⟳ Running".
//
//   3. When a running row is older than unstickAgeThreshold, the
//      renderer must drop the progress segment (stale phase strings
//      mislead more than help) and append a "Clear stuck" HTMX link
//      wired to POST /ui/sync/unstick/{type}.

import (
	"strings"
	"testing"
	"time"
)

func TestFormatRelative_AcceptsRFC3339(t *testing.T) {
	ts := time.Now().Add(-3 * time.Minute).UTC().Format(time.RFC3339)
	got := FormatRelative(ts)
	if got == "never" {
		t.Errorf("FormatRelative(%q) = %q, want a relative time string", ts, got)
	}
	if !strings.HasSuffix(got, "ago") && got != "just now" {
		t.Errorf("FormatRelative(%q) = %q, want it to look relative", ts, got)
	}
}

func TestFormatRelative_AcceptsSQLiteTimestamp(t *testing.T) {
	// Matches what SQLite's `DEFAULT CURRENT_TIMESTAMP` writes into
	// jobs.created_at — the exact shape that silently broke the
	// pre-v0.5.7.1 renderer.
	ts := time.Now().Add(-3 * time.Minute).UTC().Format("2006-01-02 15:04:05")
	got := FormatRelative(ts)
	if got == "never" {
		t.Errorf("FormatRelative(SQLite %q) = %q, want a relative time string", ts, got)
	}
}

func TestFormatRelative_EmptyIsNever(t *testing.T) {
	if got := FormatRelative(""); got != "never" {
		t.Errorf("FormatRelative(\"\") = %q, want never", got)
	}
}

func TestFormatRelative_UnparseableIsNever(t *testing.T) {
	if got := FormatRelative("not-a-timestamp"); got != "never" {
		t.Errorf("FormatRelative(\"not-a-timestamp\") = %q, want never", got)
	}
}

func TestSyncLastRunPillFull_RunningWithProgress(t *testing.T) {
	// Fresh running row (started just now) with a phase string
	// already captured — the typical mid-flight pill.
	ts := time.Now().UTC().Format(time.RFC3339)
	got := SyncLastRunPillFull("ua_hub_sync", "running", ts, "",
		`"hydrating 450/1500"`)

	if !strings.Contains(got, "Running") {
		t.Errorf("pill = %q, want it to say Running", got)
	}
	if !strings.Contains(got, "hydrating 450/1500") {
		t.Errorf("pill = %q, want it to contain the phase 'hydrating 450/1500'", got)
	}
	// JSON quotes around the stored phase must NOT leak into the
	// user-visible HTML.
	if strings.Contains(got, `"hydrating 450/1500"`) {
		t.Errorf("pill = %q, want JSON quotes stripped", got)
	}
	if strings.Contains(got, "Clear stuck") {
		t.Errorf("pill = %q, fresh job should not show the unstick link", got)
	}
}

func TestSyncLastRunPillFull_RunningStuckShowsUnstickLink(t *testing.T) {
	// Row older than unstickAgeThreshold (10m) — stale progress
	// must be dropped and "Clear stuck" link rendered.
	ts := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	got := SyncLastRunPillFull("ua_hub_sync", "running", ts, "",
		`"hydrating 450/1500"`)

	if !strings.Contains(got, "Clear stuck") {
		t.Errorf("pill = %q, want it to show Clear stuck link for a >10min running job", got)
	}
	if !strings.Contains(got, "/ui/sync/unstick/ua_hub_sync") {
		t.Errorf("pill = %q, want unstick POST target in hx-post", got)
	}
	if strings.Contains(got, "hydrating 450/1500") {
		t.Errorf("pill = %q, want stale progress phase dropped on stuck render", got)
	}
}

func TestSyncLastRunPillFull_RunningWithoutProgress(t *testing.T) {
	// A brand-new running row, before any phase has been written.
	// Should still render the running badge but without a phase
	// segment.
	ts := time.Now().UTC().Format(time.RFC3339)
	got := SyncLastRunPillFull("ua_hub_sync", "running", ts, "", "")

	if !strings.Contains(got, "Running") {
		t.Errorf("pill = %q, want it to say Running", got)
	}
	// No phase segment — just ⟳ Running · started Xm ago.
	// Assert the separator count so a regression that emits an
	// empty "· ·" sequence is caught.
	body := strings.SplitN(got, ">", 2)[1] // strip leading span tag
	sep := strings.Count(body, "·")
	if sep < 1 || sep > 1 {
		t.Errorf("pill body = %q, want exactly one '·' separator (got %d)", body, sep)
	}
}

func TestSyncLastRunPillFull_CompletedUnchanged(t *testing.T) {
	// Non-running statuses should behave identically to the old
	// four-arg SyncLastRunPill — no phase rendering, no unstick.
	ts := time.Now().UTC().Format(time.RFC3339)
	got := SyncLastRunPillFull("ua_hub_sync", "completed", ts, "",
		`"listEmails=5"`)

	if !strings.Contains(got, "badge-completed") {
		t.Errorf("pill = %q, want badge-completed class", got)
	}
	if strings.Contains(got, "listEmails=5") {
		t.Errorf("pill = %q, completed pill must not render progress phase", got)
	}
}

func TestSyncLastRunPill_LegacySignatureStillWorks(t *testing.T) {
	// Ensure the four-arg shim compiles and renders: this is the
	// path legacy call sites in the api package still use.
	ts := time.Now().UTC().Format(time.RFC3339)
	got := SyncLastRunPill("ua_hub_sync", "completed", ts, "")
	if !strings.Contains(got, "badge-completed") {
		t.Errorf("pill = %q, want badge-completed", got)
	}
}

func TestParseStoreTimestamp_Roundtrips(t *testing.T) {
	// RFC3339 round-trip.
	now := time.Now().UTC().Truncate(time.Second)
	if got, ok := parseStoreTimestamp(now.Format(time.RFC3339)); !ok || !got.Equal(now) {
		t.Errorf("RFC3339 round-trip: got=%v ok=%v, want %v/true", got, ok, now)
	}
	// SQLite form round-trip.
	if got, ok := parseStoreTimestamp(now.Format("2006-01-02 15:04:05")); !ok || !got.Equal(now) {
		t.Errorf("SQLite round-trip: got=%v ok=%v, want %v/true", got, ok, now)
	}
	// Garbage.
	if _, ok := parseStoreTimestamp("tomorrow"); ok {
		t.Errorf("parseStoreTimestamp(garbage) = ok, want !ok")
	}
	if _, ok := parseStoreTimestamp(""); ok {
		t.Errorf("parseStoreTimestamp(\"\") = ok, want !ok")
	}
}
