package ui

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestMain pins time.Local to a fixed non-UTC zone (UTC-4, no DST) so
// the local-rendering assertions are deterministic on any CI host. The
// single write happens before any test runs, so there is no data race
// with the helpers reading time.Local afterwards.
func TestMain(m *testing.M) {
	time.Local = time.FixedZone("TEST-0400", -4*3600)
	os.Exit(m.Run())
}

// TestFormatLocal_BothStoreFormats — the two storage shapes (RFC3339
// from Go-side writes, SQLite's space-separated CURRENT_TIMESTAMP form)
// must parse as UTC and render identically in local time.
func TestFormatLocal_BothStoreFormats(t *testing.T) {
	// 19:41 UTC == 3:41 PM at UTC-4. Use a fixed date safely in the
	// past so the "older than this year" branch is exercised
	// deterministically (unless the suite runs in 2024, which it won't).
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"rfc3339", "2024-07-24T19:41:00Z", "Jul 24 2024, 3:41 PM"},
		{"sqlite", "2024-07-24 19:41:00", "Jul 24 2024, 3:41 PM"},
		{"empty", "", ""},
		{"garbage passes through", "not-a-time", "not-a-time"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := FormatLocal(tc.in); got != tc.want {
				t.Errorf("FormatLocal(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestFormatLocal_TodayOmitsDate — a same-local-day stamp renders as a
// bare wall-clock time.
func TestFormatLocal_TodayOmitsDate(t *testing.T) {
	// One minute ago is always "today" in local terms (modulo a
	// midnight-straddling run, where both sides then agree on the
	// new day — the assertion is on shape, not a fixed time).
	in := time.Now().UTC().Add(-1 * time.Minute).Format(time.RFC3339)
	got := FormatLocal(in)
	if strings.Contains(got, ",") || !strings.HasSuffix(got, "M") {
		t.Errorf("FormatLocal(%q) = %q, want bare clock time like 3:41 PM", in, got)
	}
}

// TestFormatLocal_ThisYearOmitsYear — a stamp from this year (but not
// today) drops the year segment.
func TestFormatLocal_ThisYearOmitsYear(t *testing.T) {
	now := time.Now()
	if now.Month() == time.January && now.Day() < 5 {
		t.Skip("too close to New Year for a stable same-year-not-today stamp")
	}
	in := now.UTC().AddDate(0, 0, -3).Format(time.RFC3339)
	got := FormatLocal(in)
	if strings.Contains(got, fmt.Sprintf("%d", now.Year())) {
		t.Errorf("FormatLocal(%q) = %q, want the year omitted for same-year stamps", in, got)
	}
	if !strings.Contains(got, ",") {
		t.Errorf("FormatLocal(%q) = %q, want a date segment for non-today stamps", in, got)
	}
}

// TestFormatRecent — recent stamps pair a relative segment with the
// local wall-clock time; older stamps fall back to FormatLocal.
func TestFormatRecent(t *testing.T) {
	recent := time.Now().UTC().Add(-12 * time.Minute)
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"recent rfc3339", recent.Format(time.RFC3339),
			"12m ago · " + recent.In(time.Local).Format("3:04 PM")},
		{"recent sqlite", recent.Format("2006-01-02 15:04:05"),
			"12m ago · " + recent.In(time.Local).Format("3:04 PM")},
		{"old falls back to FormatLocal", "2024-07-24T19:41:00Z", "Jul 24 2024, 3:41 PM"},
		{"empty", "", ""},
		{"garbage passes through", "not-a-time", "not-a-time"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := FormatRecent(tc.in); got != tc.want {
				t.Errorf("FormatRecent(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
