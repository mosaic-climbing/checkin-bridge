package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Member represents an NFC-enrolled member (replaces cache.CachedMember).
type Member struct {
	NfcUID      string `db:"nfc_uid"      json:"nfcUid"`
	CustomerID  string `db:"customer_id"  json:"customerId"`
	Barcode     string `db:"barcode"      json:"barcode"`
	FirstName   string `db:"first_name"   json:"firstName"`
	LastName    string `db:"last_name"    json:"lastName"`
	BadgeStatus string `db:"badge_status" json:"badgeStatus"`
	BadgeName   string `db:"badge_name"   json:"badgeName"`
	Active      bool   `db:"active"       json:"active"`
	CachedAt    string `db:"cached_at"    json:"cachedAt"`
	LastCheckIn string `db:"last_checkin" json:"lastCheckIn"`
}

func (m *Member) FullName() string {
	name := strings.TrimSpace(m.FirstName + " " + m.LastName)
	if name == "" {
		return "Unknown"
	}
	return name
}

func (m *Member) IsAllowed() bool {
	return m.Active && m.BadgeStatus == "ACTIVE"
}

func (m *Member) DenyReason() string {
	if !m.Active {
		return "account inactive"
	}
	switch m.BadgeStatus {
	case "FROZEN":
		return "membership frozen"
	case "EXPIRED":
		return "membership expired"
	case "PENDING_SYNC":
		return "pending initial sync"
	case "DELETED":
		return "membership deleted"
	default:
		return "badge status: " + m.BadgeStatus
	}
}

// GetMemberByNFC looks up a member by NFC card UID.
// This is the hot path for check-in — must be fast.
func (s *Store) GetMemberByNFC(ctx context.Context, nfcUID string) (*Member, error) {
	var m Member
	err := s.db.GetContext(ctx, &m, `SELECT * FROM members WHERE nfc_uid = ?`, strings.ToUpper(nfcUID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &m, err
}

// GetMemberByCustomerID looks up a member by Redpoint customer ID.
func (s *Store) GetMemberByCustomerID(ctx context.Context, customerID string) (*Member, error) {
	var m Member
	err := s.db.GetContext(ctx, &m, `SELECT * FROM members WHERE customer_id = ?`, customerID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &m, err
}

// GetMemberByBarcode looks up a member by Redpoint barcode.
func (s *Store) GetMemberByBarcode(ctx context.Context, barcode string) (*Member, error) {
	var m Member
	err := s.db.GetContext(ctx, &m, `SELECT * FROM members WHERE barcode = ?`, barcode)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &m, err
}

// UpsertMember inserts or updates a member by NFC UID.
func (s *Store) UpsertMember(ctx context.Context, m *Member) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO members (nfc_uid, customer_id, barcode, first_name, last_name,
                            badge_status, badge_name, active, cached_at, last_checkin)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(nfc_uid) DO UPDATE SET
            customer_id  = excluded.customer_id,
            barcode      = excluded.barcode,
            first_name   = excluded.first_name,
            last_name    = excluded.last_name,
            badge_status = excluded.badge_status,
            badge_name   = excluded.badge_name,
            active       = excluded.active,
            cached_at    = excluded.cached_at,
            last_checkin = CASE WHEN excluded.last_checkin != '' THEN excluded.last_checkin ELSE members.last_checkin END
    `, m.NfcUID, m.CustomerID, m.Barcode, m.FirstName, m.LastName,
		m.BadgeStatus, m.BadgeName, m.Active, m.CachedAt, m.LastCheckIn)
	return err
}

// RemoveMember deletes a member by NFC UID.
func (s *Store) RemoveMember(ctx context.Context, nfcUID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `DELETE FROM members WHERE nfc_uid = ?`, strings.ToUpper(nfcUID))
	return err
}

// RecordCheckIn updates the last check-in timestamp for a member.
func (s *Store) RecordMemberCheckIn(ctx context.Context, nfcUID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `UPDATE members SET last_checkin = ? WHERE nfc_uid = ?`, now, strings.ToUpper(nfcUID))
	return err
}

// AllMembers returns all members (for admin API dump).
func (s *Store) AllMembers(ctx context.Context) ([]Member, error) {
	var members []Member
	err := s.db.SelectContext(ctx, &members, `SELECT * FROM members ORDER BY last_name, first_name`)
	return members, err
}

// AllMembersPaged returns a page of members ordered by most-recently-bound first
// (mapping.matched_at DESC, NULLS LAST so orphans sort to the bottom), with
// (last_name, first_name) as the tiebreak so identical timestamps fall back to
// alphabetical order. total is the count across all pages (not affected by
// limit/offset).
//
// v0.5.9 sort rationale: after a bulk ingest the newly-bound members are the
// ones staff is most likely to open the detail panel on — misassignments
// surface at the top of the list rather than needing a scroll through the
// alphabetical back-catalogue. Orphaned members (no mapping row) sort to the
// bottom because they're almost always historical debris from an aborted sync
// run and the recovery action is "Remove", which doesn't care about position.
func (s *Store) AllMembersPaged(ctx context.Context, limit, offset int) ([]Member, int, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	var total int
	if err := s.db.GetContext(ctx, &total, `SELECT COUNT(*) FROM members`); err != nil {
		return nil, 0, err
	}
	// LEFT JOIN rather than INNER: members whose customer_id has no
	// mapping row still appear in the result set — their matched_at
	// comes back NULL and the trailing `matched_at IS NULL` sort clause
	// shoves them to the bottom of the list. SQLite doesn't support
	// `NULLS LAST`; ordering by `matched_at IS NULL` (which is 0 for
	// mapped rows and 1 for orphans) first achieves the same effect.
	var members []Member
	err := s.db.SelectContext(ctx, &members, `
        SELECT mem.*
          FROM members mem
          LEFT JOIN ua_user_mappings map ON map.redpoint_customer_id = mem.customer_id
         ORDER BY map.matched_at IS NULL, map.matched_at DESC, mem.last_name, mem.first_name
         LIMIT ? OFFSET ?
    `, limit, offset)
	return members, total, err
}

// AllMemberCustomerIDs returns all customer IDs for targeted Redpoint refresh.
func (s *Store) AllMemberCustomerIDs(ctx context.Context) ([]string, error) {
	var ids []string
	err := s.db.SelectContext(ctx, &ids, `SELECT DISTINCT customer_id FROM members`)
	return ids, err
}

// MemberStats returns aggregate stats.
type MemberStats struct {
	Total          int `db:"total"          json:"totalMembers"`
	Active         int `db:"active"         json:"activeMembers"`
	Frozen         int `db:"frozen"         json:"frozenMembers"`
	Expired        int `db:"expired"        json:"expiredMembers"`
	PendingSync    int `db:"pending_sync"   json:"pendingSyncMembers"`
	Inactive       int `db:"inactive"       json:"inactiveAccounts"`
	CheckedInToday int `db:"checked_in_today" json:"checkedInToday"`
}

func (s *Store) MemberStats(ctx context.Context) (*MemberStats, error) {
	var stats MemberStats
	err := s.db.GetContext(ctx, &stats, `
        SELECT
            COUNT(*) AS total,
            SUM(CASE WHEN badge_status = 'ACTIVE' AND active = 1 THEN 1 ELSE 0 END) AS active,
            SUM(CASE WHEN badge_status = 'FROZEN' THEN 1 ELSE 0 END) AS frozen,
            SUM(CASE WHEN badge_status = 'EXPIRED' THEN 1 ELSE 0 END) AS expired,
            SUM(CASE WHEN badge_status = 'PENDING_SYNC' THEN 1 ELSE 0 END) AS pending_sync,
            SUM(CASE WHEN active = 0 THEN 1 ELSE 0 END) AS inactive,
            SUM(CASE WHEN date(last_checkin) = date('now') THEN 1 ELSE 0 END) AS checked_in_today
        FROM members
    `)
	return &stats, err
}

// PruneInactive removes members that are no longer active. Returns count removed.
func (s *Store) PruneInactive(ctx context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.ExecContext(ctx, `DELETE FROM members WHERE active = 0 AND badge_status IN ('DELETED', 'EXPIRED')`)
	if err != nil {
		return 0, err
	}
	n, _ := result.RowsAffected()
	return int(n), nil
}

// BulkReplace replaces all members atomically (for full re-ingest).
func (s *Store) BulkReplace(ctx context.Context, members []Member) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM members`); err != nil {
		return err
	}

	stmt, err := tx.PrepareContext(ctx, `
        INSERT INTO members (nfc_uid, customer_id, barcode, first_name, last_name,
                            badge_status, badge_name, active, cached_at, last_checkin)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
    `)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, m := range members {
		if _, err := stmt.ExecContext(ctx, m.NfcUID, m.CustomerID, m.Barcode,
			m.FirstName, m.LastName, m.BadgeStatus, m.BadgeName, m.Active,
			m.CachedAt, m.LastCheckIn); err != nil {
			return err
		}
	}
	return tx.Commit()
}
