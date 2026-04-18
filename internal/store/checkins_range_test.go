package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// P3 — CheckInStats / CheckInsByHour / ShadowDecisionStatsToday switched from
// `WHERE date(timestamp) = date('now')` to a `timestamp >= ? AND timestamp < ?`
// range so the planner can use `idx_checkins_timestamp`. These tests pin:
//
//   1. Today's boundaries are computed in UTC (matches how RecordCheckIn writes).
//   2. Rows stored in either RFC3339 (`"...T..Z"`) or SQLite-default
//      (`"... ..."` with a space) form are both included — the bare date
//      prefix guarantees this.
//   3. Yesterday/tomorrow rows are excluded from the "today" counters.
//   4. CheckInsByHour's optional `date` argument still scopes to one day.
//   5. EXPLAIN QUERY PLAN actually reports index usage, not a full scan.

// checkinStatsPlan returns the concatenated `EXPLAIN QUERY PLAN` detail
// rows for the CheckInStats query. Used to assert the planner chose the
// timestamp index instead of a table scan.
func checkinStatsPlan(t *testing.T, s *Store) string {
	t.Helper()
	today, tomorrow := todayBoundsUTC()
	rows, err := s.db.QueryContext(context.Background(), `
        EXPLAIN QUERY PLAN
        SELECT
            (SELECT COUNT(*) FROM checkins) AS total,
            (SELECT COUNT(*) FROM checkins
                WHERE timestamp >= ? AND timestamp < ?) AS today,
            (SELECT COUNT(*) FROM checkins
                WHERE timestamp >= ? AND timestamp < ?
                  AND result = 'allowed') AS allowed,
            (SELECT COUNT(*) FROM checkins
                WHERE timestamp >= ? AND timestamp < ?
                  AND result = 'denied') AS denied,
            (SELECT COUNT(DISTINCT customer_id) FROM checkins
                WHERE timestamp >= ? AND timestamp < ?
                  AND customer_id != '') AS uniq
    `,
		today, tomorrow,
		today, tomorrow,
		today, tomorrow,
		today, tomorrow,
		today, tomorrow,
	)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer rows.Close()
	var all strings.Builder
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan: %v", err)
		}
		all.WriteString(detail)
		all.WriteString("\n")
	}
	return all.String()
}

func TestCheckInStats_UsesTimestampIndex(t *testing.T) {
	s := testStore(t)
	plan := checkinStatsPlan(t, s)
	// Each of the four today-scoped subqueries must hit
	// idx_checkins_timestamp. The unconditional COUNT(*) in subquery 1 has
	// no filter and will necessarily scan the table via whatever covering
	// index SQLite picks — that's not a regression, so we don't try to pin
	// subquery-1's plan, only the filtered ones.
	hits := strings.Count(plan, "idx_checkins_timestamp")
	if hits < 4 {
		t.Errorf("expected idx_checkins_timestamp used at least 4 times, got %d:\n%s", hits, plan)
	}
	// Guard against the specific regression we care about: a filtered
	// subquery downgrading from "SEARCH ... USING INDEX idx_checkins_timestamp"
	// to a bare "SEARCH checkins" without-index. SQLite writes SCANs for the
	// unconditional COUNT(*); it writes SEARCHes for filtered queries. A
	// filtered subquery that doesn't use the index would say "SEARCH checkins"
	// with no USING INDEX clause.
	lines := strings.Split(plan, "\n")
	for _, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		if strings.HasPrefix(trimmed, "SEARCH checkins") &&
			!strings.Contains(trimmed, "idx_checkins_timestamp") {
			t.Errorf("filtered subquery not using timestamp index: %q", trimmed)
		}
	}
}

func TestCheckInStats_CountsOnlyTodayRows(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	yesterday := now.AddDate(0, 0, -1).Format(time.RFC3339)
	todayRFC := now.Format(time.RFC3339)
	tomorrow := now.AddDate(0, 0, 1).Format(time.RFC3339)

	// One row per day. Same customer for UniqueToday = 1.
	seed := []CheckInEvent{
		{Timestamp: yesterday, NfcUID: "N1", CustomerID: "c1", CustomerName: "A", Result: "allowed"},
		{Timestamp: todayRFC, NfcUID: "N1", CustomerID: "c1", CustomerName: "A", Result: "allowed"},
		{Timestamp: todayRFC, NfcUID: "N2", CustomerID: "c2", CustomerName: "B", Result: "denied"},
		{Timestamp: tomorrow, NfcUID: "N3", CustomerID: "c3", CustomerName: "C", Result: "allowed"},
	}
	for i := range seed {
		if _, err := s.RecordCheckIn(ctx, &seed[i]); err != nil {
			t.Fatalf("seed[%d]: %v", i, err)
		}
	}

	stats, err := s.CheckInStats(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if stats.TotalAllTime != 4 {
		t.Errorf("TotalAllTime = %d, want 4", stats.TotalAllTime)
	}
	if stats.TotalToday != 2 {
		t.Errorf("TotalToday = %d, want 2 (yesterday & tomorrow should be excluded)", stats.TotalToday)
	}
	if stats.AllowedToday != 1 {
		t.Errorf("AllowedToday = %d, want 1", stats.AllowedToday)
	}
	if stats.DeniedToday != 1 {
		t.Errorf("DeniedToday = %d, want 1", stats.DeniedToday)
	}
	if stats.UniqueToday != 2 {
		t.Errorf("UniqueToday = %d, want 2 (c1 & c2)", stats.UniqueToday)
	}
}

func TestCheckInStats_IncludesRowsWithSpaceSeparatedTimestamps(t *testing.T) {
	// SQLite's column DEFAULT is datetime('now'), which emits
	// "YYYY-MM-DD HH:MM:SS" (space separator). Any row that bypasses the Go
	// helper and relies on the default will be in this form. The range
	// query uses date-only bounds so both forms sort into range.
	s := testStore(t)
	ctx := context.Background()

	today, _ := todayBoundsUTC()
	// Insert directly with space-separated today timestamp.
	if _, err := s.db.ExecContext(ctx, `
        INSERT INTO checkins (timestamp, nfc_uid, customer_id, customer_name,
                              result, deny_reason)
        VALUES (?, 'N-SP', 'c-sp', 'Space', 'allowed', '')
    `, today+" 12:00:00"); err != nil {
		t.Fatal(err)
	}
	// Insert with RFC3339 today timestamp.
	if _, err := s.RecordCheckIn(ctx, &CheckInEvent{
		Timestamp:    today + "T12:00:00Z",
		NfcUID:       "N-T",
		CustomerID:   "c-t",
		CustomerName: "Tee",
		Result:       "allowed",
	}); err != nil {
		t.Fatal(err)
	}

	stats, err := s.CheckInStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalToday != 2 {
		t.Errorf("TotalToday = %d, want 2 (space- and T-separated forms both match)", stats.TotalToday)
	}
}

func TestCheckInsByHour_DefaultsToTodayUTC(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	today, _ := todayBoundsUTC()
	// Seed at three distinct hours today.
	hours := []string{"09:00:00Z", "09:30:00Z", "14:05:00Z"}
	for i, h := range hours {
		if _, err := s.RecordCheckIn(ctx, &CheckInEvent{
			Timestamp:  today + "T" + h,
			NfcUID:     "N" + string(rune('0'+i)),
			CustomerID: "c",
			Result:     "allowed",
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Seed yesterday at the same hour; it must not bleed into today's buckets.
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	if _, err := s.RecordCheckIn(ctx, &CheckInEvent{
		Timestamp:  yesterday + "T09:00:00Z",
		NfcUID:     "N-Y",
		CustomerID: "c",
		Result:     "allowed",
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.CheckInsByHour(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	// 9 should have two rows, 14 one row, no other buckets, no yesterday bleed.
	if len(got) != 2 {
		t.Fatalf("buckets = %d (%+v), want 2", len(got), got)
	}
	byHour := map[int]HourlyCount{}
	for _, h := range got {
		byHour[h.Hour] = h
	}
	if byHour[9].Count != 2 {
		t.Errorf("hour 9 count = %d, want 2", byHour[9].Count)
	}
	if byHour[14].Count != 1 {
		t.Errorf("hour 14 count = %d, want 1", byHour[14].Count)
	}
}

func TestCheckInsByHour_ExplicitDateScopesToOneDay(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	target := "2025-06-15"
	// Three events on target date, one on the day after.
	if _, err := s.RecordCheckIn(ctx, &CheckInEvent{
		Timestamp: target + "T10:00:00Z", NfcUID: "a", Result: "allowed",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordCheckIn(ctx, &CheckInEvent{
		Timestamp: target + "T11:00:00Z", NfcUID: "b", Result: "denied",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordCheckIn(ctx, &CheckInEvent{
		Timestamp: target + "T11:30:00Z", NfcUID: "c", Result: "denied",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordCheckIn(ctx, &CheckInEvent{
		Timestamp: "2025-06-16T11:00:00Z", NfcUID: "d", Result: "allowed",
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.CheckInsByHour(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d buckets, want 2 (10 and 11 only): %+v", len(got), got)
	}
	byHour := map[int]HourlyCount{}
	for _, h := range got {
		byHour[h.Hour] = h
	}
	if byHour[10].Allowed != 1 || byHour[10].Denied != 0 {
		t.Errorf("hour 10 = %+v, want Allowed=1 Denied=0", byHour[10])
	}
	if byHour[11].Allowed != 0 || byHour[11].Denied != 2 {
		t.Errorf("hour 11 = %+v, want Allowed=0 Denied=2", byHour[11])
	}
}

func TestCheckInsByHour_InvalidDateReturnsEmpty(t *testing.T) {
	// An unparseable date string falls back to using the input as the upper
	// bound too, which returns zero rows rather than erroring — the endpoint
	// stays a pure GET that can't be made to crash with a bad query param.
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.RecordCheckIn(ctx, &CheckInEvent{
		Timestamp: "2026-04-17T10:00:00Z", NfcUID: "x", Result: "allowed",
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.CheckInsByHour(ctx, "not-a-date")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 buckets, got %d (%+v)", len(got), got)
	}
}

func TestShadowDecisionStatsToday_UsesRange(t *testing.T) {
	// Seed yesterday + today + tomorrow; assert only today's row contributes
	// to the Total. The yesterday/tomorrow rows would have been swept into
	// Total under `date(timestamp) = date('now')` only if the test machine's
	// local day boundary disagreed with UTC; with the range fix both forms
	// are pinned to UTC unambiguously.
	s := testStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	for _, rec := range []struct {
		ts          string
		unifiResult string
		result      string
	}{
		{now.AddDate(0, 0, -1).Format(time.RFC3339), "ACCESS", "allowed"},
		{now.Format(time.RFC3339), "ACCESS", "allowed"},
		{now.Format(time.RFC3339), "BLOCKED", "denied"},
		{now.AddDate(0, 0, 1).Format(time.RFC3339), "ACCESS", "allowed"},
	} {
		if _, err := s.RecordCheckIn(ctx, &CheckInEvent{
			Timestamp:   rec.ts,
			NfcUID:      "n",
			CustomerID:  "c",
			Result:      rec.result,
			UnifiResult: rec.unifiResult,
		}); err != nil {
			t.Fatal(err)
		}
	}

	stats, err := s.ShadowDecisionStatsToday(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Total != 2 {
		t.Errorf("Total = %d, want 2 (yesterday & tomorrow excluded)", stats.Total)
	}
	if stats.Agree != 2 {
		t.Errorf("Agree = %d, want 2 (both today rows agree)", stats.Agree)
	}
	if stats.Disagree != 0 {
		t.Errorf("Disagree = %d, want 0", stats.Disagree)
	}
}

func TestTodayBoundsUTC_IsHalfOpen(t *testing.T) {
	// Sanity: the tuple must be (today, tomorrow) in YYYY-MM-DD form, ordered
	// lexicographically, one day apart. If this drifts (e.g. someone
	// accidentally switches to local time), the storage-layer tests above
	// may go green while production silently rolls over at the wrong hour.
	today, tomorrow := todayBoundsUTC()
	if len(today) != 10 || len(tomorrow) != 10 {
		t.Fatalf("bounds not YYYY-MM-DD: today=%q tomorrow=%q", today, tomorrow)
	}
	if today >= tomorrow {
		t.Errorf("today=%q should sort < tomorrow=%q", today, tomorrow)
	}
	tToday, err := time.Parse("2006-01-02", today)
	if err != nil {
		t.Fatal(err)
	}
	tTomorrow, err := time.Parse("2006-01-02", tomorrow)
	if err != nil {
		t.Fatal(err)
	}
	if diff := tTomorrow.Sub(tToday); diff != 24*time.Hour {
		t.Errorf("tomorrow - today = %v, want 24h", diff)
	}
}
