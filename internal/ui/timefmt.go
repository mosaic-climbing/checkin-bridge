package ui

// Local-timezone timestamp rendering for the staff UI.
//
// The store keeps two timestamp shapes (documented on parseStoreTimestamp
// in fragments.go): RFC3339 from Go-side writes and SQLite's
// `YYYY-MM-DD HH:MM:SS` from DEFAULT CURRENT_TIMESTAMP columns. Both are
// UTC. Pre-overhaul, most render sites either sliced `t[11:19]` (bare UTC
// HH:MM:SS, hours off local wall-clock) or dumped the raw string. The two
// helpers below are the single render path: the bridge runs on the gym's
// Mac, so time.Local IS the gym's wall clock.
//
//   - FormatLocal:  absolute local time — "3:41 PM" (today),
//     "Jul 24, 3:41 PM" (this year), "Jul 24 2025, 3:41 PM" (older).
//   - FormatRecent: "12m ago · 3:41 PM" for anything in the last day,
//     falling back to FormatLocal for older stamps. This is the default
//     for activity feeds (check-ins, job runs, last-tap cells).
//
// Both return the input unchanged when it doesn't parse — a malformed
// stamp should be visible, not silently blanked — and "" for empty input
// so callers keep owning their empty-state copy ("Never", "—").

import "time"

// FormatLocal renders a store timestamp as an absolute local-time string.
func FormatLocal(ts string) string {
	t, ok := parseStoreTimestamp(ts)
	if !ok {
		return ts
	}
	lt := t.In(time.Local)
	now := time.Now().In(time.Local)
	switch {
	case lt.Year() == now.Year() && lt.YearDay() == now.YearDay():
		return lt.Format("3:04 PM")
	case lt.Year() == now.Year():
		return lt.Format("Jan 2, 3:04 PM")
	default:
		return lt.Format("Jan 2 2006, 3:04 PM")
	}
}

// FormatRecent renders a store timestamp as "12m ago · 3:41 PM" when it
// is less than 24h old, otherwise delegates to FormatLocal. Suited to
// activity feeds where "how long ago" is the first question and the
// wall-clock time the second.
func FormatRecent(ts string) string {
	t, ok := parseStoreTimestamp(ts)
	if !ok {
		return ts
	}
	age := time.Since(t)
	if age >= 0 && age < 24*time.Hour {
		return FormatRelative(ts) + " · " + t.In(time.Local).Format("3:04 PM")
	}
	return FormatLocal(ts)
}
