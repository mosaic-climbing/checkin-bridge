package store

import (
	"context"
	"time"
)

// CheckInEvent represents a durable record of an NFC tap event.
//
// Two independent decisions are recorded per event:
//   - Result      — the bridge's verdict ("allowed", "denied", "recheck_allowed")
//   - UnifiResult — the UA-Hub's own verdict ("ACCESS" or "BLOCKED"), read off
//                    the WebSocket AccessEvent.Result field. Used by the shadow
//                    decisions panel to surface disagreements before flipping
//                    from shadow to live.
type CheckInEvent struct {
	ID                int    `db:"id"                  json:"id"`
	Timestamp         string `db:"timestamp"           json:"timestamp"`
	NfcUID            string `db:"nfc_uid"             json:"nfcUid"`
	CustomerID        string `db:"customer_id"         json:"customerId"`
	CustomerName      string `db:"customer_name"       json:"customerName"`
	DoorID            string `db:"door_id"             json:"doorId"`
	DoorName          string `db:"door_name"           json:"doorName"`
	Result            string `db:"result"              json:"result"`       // "allowed", "denied", "recheck_allowed"
	DenyReason        string `db:"deny_reason"         json:"denyReason"`
	RedpointRecorded  bool   `db:"redpoint_recorded"   json:"redpointRecorded"`
	RedpointCheckInID string `db:"redpoint_checkin_id" json:"redpointCheckInId"`
	UnifiResult       string `db:"unifi_result"        json:"unifiResult"`  // "ACCESS", "BLOCKED", "" if unknown
	// UnifiLogID is the stable UA-Hub system-log `_id` that produced
	// this check-in (v0.5.0+, tap-poller path). Empty on historical
	// rows and on rows that never had a UniFi side (e.g. devhooks
	// test taps). A unique partial index prevents the poller from
	// inserting the same log entry twice when its time window
	// overlaps a prior poll.
	UnifiLogID string `db:"unifi_log_id"        json:"unifiLogId,omitempty"`
}

// RecordCheckIn stores a check-in event. Returns the row ID.
//
// When UnifiLogID is non-empty, the insert is dedup'd via INSERT OR
// IGNORE against idx_checkins_unifi_log_id. On conflict (same log id
// already recorded), the return is (0, nil) — callers that need to
// distinguish "inserted" from "deduped" should check whether the
// returned ID is > 0.
func (s *Store) RecordCheckIn(ctx context.Context, evt *CheckInEvent) (int64, error) {
	if evt.Timestamp == "" {
		evt.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}
	result, err := s.db.ExecContext(ctx, `
        INSERT OR IGNORE INTO checkins (timestamp, nfc_uid, customer_id, customer_name, door_id, door_name,
                             result, deny_reason, redpoint_recorded, redpoint_checkin_id, unifi_result, unifi_log_id)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
    `, evt.Timestamp, evt.NfcUID, evt.CustomerID, evt.CustomerName, evt.DoorID, evt.DoorName,
		evt.Result, evt.DenyReason, evt.RedpointRecorded, evt.RedpointCheckInID, evt.UnifiResult, evt.UnifiLogID)
	if err != nil {
		return 0, err
	}

	// IMPORTANT: modernc.org/sqlite's LastInsertId() returns the last
	// successful insert's rowid for the connection, NOT zero on an
	// IGNORE'd row. We check RowsAffected() to distinguish a real
	// insert from a dedup — if 0 rows changed, the UnifiLogID
	// already existed and we return id=0 so the caller can treat
	// the event as "already recorded, skip".
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if affected == 0 {
		return 0, nil
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, nil
}

// MarkRedpointRecorded updates a check-in event after async Redpoint recording succeeds.
func (s *Store) MarkRedpointRecorded(ctx context.Context, id int64, redpointID string) error {
	_, err := s.db.ExecContext(ctx, `
        UPDATE checkins SET redpoint_recorded = 1, redpoint_checkin_id = ? WHERE id = ?
    `, redpointID, id)
	return err
}

// RecentCheckIns returns the N most recent check-in events.
func (s *Store) RecentCheckIns(ctx context.Context, limit int) ([]CheckInEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	var events []CheckInEvent
	err := s.db.SelectContext(ctx, &events,
		`SELECT * FROM checkins ORDER BY id DESC LIMIT ?`, limit)
	return events, err
}

// CheckInsForCustomer returns check-in history for a specific customer.
func (s *Store) CheckInsForCustomer(ctx context.Context, customerID string, limit int) ([]CheckInEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	var events []CheckInEvent
	err := s.db.SelectContext(ctx, &events,
		`SELECT * FROM checkins WHERE customer_id = ? ORDER BY id DESC LIMIT ?`, customerID, limit)
	return events, err
}

// CheckInsBetween returns check-in events whose timestamp falls within an
// inclusive date range. start/end are ISO-8601 dates ("2026-04-16") or full
// RFC3339 timestamps. If start is empty, the range is unbounded on the low
// side; same for end. Ordered oldest-first, which is the useful order for
// exporting audit trails.
//
// String comparison is valid here because timestamps are stored in RFC3339,
// which sorts lexicographically in the same order it sorts chronologically.
// Bare-date end bounds are expanded to end-of-day so "to=2026-04-16" covers
// every event on that calendar day.
func (s *Store) CheckInsBetween(ctx context.Context, start, end string) ([]CheckInEvent, error) {
	query := `SELECT * FROM checkins WHERE 1=1`
	var args []any
	if start != "" {
		query += ` AND timestamp >= ?`
		args = append(args, start)
	}
	if end != "" {
		bound := end
		if len(end) == 10 { // "YYYY-MM-DD"
			bound = end + "T23:59:59Z"
		}
		query += ` AND timestamp <= ?`
		args = append(args, bound)
	}
	query += ` ORDER BY id ASC`

	var events []CheckInEvent
	err := s.db.SelectContext(ctx, &events, query, args...)
	return events, err
}

// CheckInStats returns aggregate check-in statistics.
type CheckInStats struct {
	TotalAllTime int `db:"total"     json:"totalAllTime"`
	TotalToday   int `db:"today"     json:"totalToday"`
	AllowedToday int `db:"allowed"   json:"allowedToday"`
	DeniedToday  int `db:"denied"    json:"deniedToday"`
	UniqueToday  int `db:"uniq"      json:"uniqueMembersToday"`
}

// todayBoundsUTC returns today's date and the next day's date in
// lexicographically-sortable YYYY-MM-DD form, computed against UTC. These
// values are used as half-open range bounds (`[today, tomorrow)`) against the
// `timestamp` column so the query planner can use `idx_checkins_timestamp`.
//
// Why date-only strings rather than full RFC3339 boundaries:
//
//	Timestamps are normally stored in RFC3339 ("2026-04-17T15:30:00Z") by
//	RecordCheckIn, but the column DEFAULT uses SQLite's datetime('now')
//	("2026-04-17 15:30:00") and legacy rows may use either form. In ASCII the
//	space (0x20) sorts before "T" (0x54), so a boundary of
//	"2026-04-17T00:00:00Z" would wrongly exclude rows stored in the
//	space-separated form. Comparing against the bare date "2026-04-17" works
//	for both formats because they share the YYYY-MM-DD prefix.
//
// Using UTC here is deliberate: RecordCheckIn writes in UTC, so aligning the
// day boundary with UTC matches what's on disk.
func todayBoundsUTC() (string, string) {
	now := time.Now().UTC()
	today := now.Format("2006-01-02")
	tomorrow := now.AddDate(0, 0, 1).Format("2006-01-02")
	return today, tomorrow
}

// CheckInStats returns totals plus today's breakdown (allowed / denied /
// unique members). Today's filters use a `timestamp >= ? AND timestamp < ?`
// range so the planner can use `idx_checkins_timestamp`; wrapping timestamp
// in `date()` — as an earlier version did — turned each subquery into a full
// scan, which is five full scans per request.
func (s *Store) CheckInStats(ctx context.Context) (*CheckInStats, error) {
	today, tomorrow := todayBoundsUTC()
	var stats CheckInStats
	err := s.db.GetContext(ctx, &stats, `
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
	return &stats, err
}

// CheckInsByHour returns check-in counts grouped by hour for a given date.
// `date` is a YYYY-MM-DD string; an empty value defaults to today (UTC).
//
// The WHERE clause uses a range against the raw `timestamp` column so the
// `idx_checkins_timestamp` index can be used. Earlier versions matched
// `date(timestamp) = ?`, which forced a full scan.
func (s *Store) CheckInsByHour(ctx context.Context, date string) ([]HourlyCount, error) {
	if date == "" {
		date = time.Now().UTC().Format("2006-01-02")
	}
	// Compute the exclusive upper bound (start of next day). If the caller
	// passed an unparseable string, fall back to the old same-day-prefix
	// behaviour by using `date` itself as the upper bound, which returns
	// zero rows rather than erroring.
	var next string
	if t, err := time.Parse("2006-01-02", date); err == nil {
		next = t.AddDate(0, 0, 1).Format("2006-01-02")
	} else {
		next = date
	}
	var counts []HourlyCount
	err := s.db.SelectContext(ctx, &counts, `
        SELECT
            CAST(strftime('%H', timestamp) AS INTEGER) AS hour,
            COUNT(*) AS count,
            SUM(CASE WHEN result = 'allowed' THEN 1 ELSE 0 END) AS allowed,
            SUM(CASE WHEN result = 'denied' THEN 1 ELSE 0 END) AS denied
        FROM checkins
        WHERE timestamp >= ? AND timestamp < ?
        GROUP BY hour
        ORDER BY hour
    `, date, next)
	return counts, err
}

type HourlyCount struct {
	Hour    int `db:"hour"    json:"hour"`
	Count   int `db:"count"   json:"count"`
	Allowed int `db:"allowed" json:"allowed"`
	Denied  int `db:"denied"  json:"denied"`
}

// DisagreementEvents returns recent check-ins where the UA-Hub's native
// decision (unifi_result = ACCESS|BLOCKED) contradicts the bridge's decision
// (result = allowed|denied). These are the taps an operator must review
// before switching from shadow to live — every row is either a would-be
// missed entry or a would-be false admit.
//
// Events where unifi_result is empty (pre-migration rows, or events the
// WebSocket didn't carry a Result on) are excluded; there is nothing to
// compare against.
func (s *Store) DisagreementEvents(ctx context.Context, limit int) ([]CheckInEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	var events []CheckInEvent
	err := s.db.SelectContext(ctx, &events, `
        SELECT * FROM checkins
        WHERE unifi_result != ''
          AND (
              (unifi_result = 'ACCESS'  AND result != 'allowed' AND result != 'recheck_allowed')
           OR (unifi_result = 'BLOCKED' AND (result = 'allowed' OR result = 'recheck_allowed'))
          )
        ORDER BY id DESC
        LIMIT ?
    `, limit)
	return events, err
}

// ShadowDecisionStats summarizes agreement between UniFi and the bridge
// for today's traffic. Emitted by the shadow-decisions panel so operators
// can see at a glance whether they're safe to flip to live.
type ShadowDecisionStats struct {
	Total        int `db:"total"         json:"total"`
	Agree        int `db:"agree"         json:"agree"`
	Disagree     int `db:"disagree"      json:"disagree"`
	Unknown      int `db:"unknown"       json:"unknown"`  // no unifi_result recorded
	WouldMiss    int `db:"would_miss"    json:"wouldMiss"`    // UniFi ACCESS, bridge denied — paying members we'd lock out
	WouldAdmit   int `db:"would_admit"   json:"wouldAdmit"`   // UniFi BLOCKED, bridge allowed — UniFi policy we'd override
}

// ShadowDecisionStatsToday returns agreement counters for today.
//
// Same range-query rationale as CheckInStats: `WHERE date(timestamp) = date('now')`
// skips `idx_checkins_timestamp`, so we pass today/tomorrow as parameters and
// use a half-open range instead.
func (s *Store) ShadowDecisionStatsToday(ctx context.Context) (*ShadowDecisionStats, error) {
	today, tomorrow := todayBoundsUTC()
	var stats ShadowDecisionStats
	err := s.db.GetContext(ctx, &stats, `
        SELECT
            COALESCE(COUNT(*), 0) AS total,
            COALESCE(SUM(CASE
                WHEN unifi_result = 'ACCESS'  AND (result = 'allowed' OR result = 'recheck_allowed') THEN 1
                WHEN unifi_result = 'BLOCKED' AND result = 'denied' THEN 1
                ELSE 0
            END), 0) AS agree,
            COALESCE(SUM(CASE
                WHEN unifi_result = 'ACCESS'  AND result = 'denied' THEN 1
                WHEN unifi_result = 'BLOCKED' AND (result = 'allowed' OR result = 'recheck_allowed') THEN 1
                ELSE 0
            END), 0) AS disagree,
            COALESCE(SUM(CASE WHEN unifi_result = '' THEN 1 ELSE 0 END), 0) AS unknown,
            COALESCE(SUM(CASE WHEN unifi_result = 'ACCESS'  AND result = 'denied' THEN 1 ELSE 0 END), 0) AS would_miss,
            COALESCE(SUM(CASE WHEN unifi_result = 'BLOCKED' AND (result = 'allowed' OR result = 'recheck_allowed') THEN 1 ELSE 0 END), 0) AS would_admit
        FROM checkins
        WHERE timestamp >= ? AND timestamp < ?
    `, today, tomorrow)
	return &stats, err
}
