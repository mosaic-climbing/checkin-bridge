package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Mapping is the resolved binding between a UA-Hub user and a Redpoint
// customer. Written once by the matching algorithm (auto or staff), then
// read on every subsequent sync as the fast path.
//
// MatchedBy values:
//
//	auto:email         — email match, exactly one Redpoint customer with
//	                     that email, no ambiguity.
//	auto:email+name    — email match produced 2+ Redpoint customers
//	                     (household collision), disambiguated locally by
//	                     normalized (firstName, lastName) match.
//	auto:name          — no email match, fell back to full-directory name
//	                     scan, exactly one normalized-name match.
//	staff:<username>   — manually assigned via /ui/unmatched.
type Mapping struct {
	UAUserID          string `db:"ua_user_id"           json:"uaUserId"`
	RedpointCustomer  string `db:"redpoint_customer_id" json:"redpointCustomerId"`
	MatchedAt         string `db:"matched_at"           json:"matchedAt"`
	MatchedBy         string `db:"matched_by"           json:"matchedBy"`
	LastEmailSyncedAt string `db:"last_email_synced_at" json:"lastEmailSyncedAt"`
}

// PendingReason enumerates the situations in which a UA-Hub user falls into
// the unmatched bucket. The UI renders different copy per reason so staff
// can prioritise.
const (
	PendingReasonNoEmail        = "no_email"         // UA-Hub user has NFC tokens but no email field set
	PendingReasonNoMatch        = "no_match"         // email search returned zero Redpoint customers
	PendingReasonAmbiguousEmail = "ambiguous_email"  // 2+ Redpoint customers share the email AND the name check couldn't disambiguate
	PendingReasonAmbiguousName  = "ambiguous_name"   // email missing or no match; name-scan fallback returned 2+ candidates
)

// Pending represents a UA-Hub user that couldn't be auto-matched and is
// accruing grace-window time before default-deactivation.
//
// Candidates is a pipe-separated list of Redpoint customer IDs (e.g. the
// household-email collision set, or the top-N name-fuzzy matches). Rendered
// in the staff UI so an operator can see the shortlist without re-running
// the search.
type Pending struct {
	UAUserID   string `db:"ua_user_id"  json:"uaUserId"`
	Reason     string `db:"reason"      json:"reason"`
	FirstSeen  string `db:"first_seen"  json:"firstSeen"`
	LastSeen   string `db:"last_seen"   json:"lastSeen"`
	GraceUntil string `db:"grace_until" json:"graceUntil"`
	Candidates string `db:"candidates"  json:"candidates"`
	// UAName and UAEmail cache the UA-Hub user's display identity at
	// the time of the last observation. They exist so the Needs Match
	// page can render without a live UA-Hub ListUsers walk — see
	// auditMigration5_pending_ua_identity for the rationale. Both are
	// refreshed on every UpsertPending so a UA-Hub-side rename
	// propagates on the next statusync pass.
	UAName  string `db:"ua_name"  json:"uaName"`
	UAEmail string `db:"ua_email" json:"uaEmail"`
}

// MatchAudit is an append-only forensic log of every mapping decision and
// every UA-Hub UpdateUser call the bridge performs. Field is a symbolic
// name like "mapping" | "user_email" | "user_status" | "access_policy" |
// "nfc_card". Source is the decision source (same strings as
// Mapping.MatchedBy, plus "bridge:sync", "bridge:deactivate", etc.).
type MatchAudit struct {
	ID        int64  `db:"id"         json:"id"`
	UAUserID  string `db:"ua_user_id" json:"uaUserId"`
	Field     string `db:"field"      json:"field"`
	BeforeVal string `db:"before_val" json:"beforeVal"`
	AfterVal  string `db:"after_val"  json:"afterVal"`
	Source    string `db:"source"     json:"source"`
	Timestamp string `db:"timestamp"  json:"timestamp"`
}

// GetMapping looks up a mapping by UA-Hub user ID. Returns nil, nil if
// no mapping exists (per the store package's convention — callers should
// not see sql.ErrNoRows).
func (s *Store) GetMapping(ctx context.Context, uaUserID string) (*Mapping, error) {
	var m Mapping
	err := s.db.GetContext(ctx, &m, `SELECT * FROM ua_user_mappings WHERE ua_user_id = ?`, uaUserID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &m, err
}

// GetMappingByCustomerID looks up a mapping by Redpoint customer ID. Used
// to detect the "this Redpoint customer is already bound to a different
// UA-Hub user" race when staff tries to manually match, and to refuse.
func (s *Store) GetMappingByCustomerID(ctx context.Context, customerID string) (*Mapping, error) {
	var m Mapping
	err := s.db.GetContext(ctx, &m, `SELECT * FROM ua_user_mappings WHERE redpoint_customer_id = ?`, customerID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &m, err
}

// UpsertMapping creates or updates the mapping row for a UA-Hub user. The
// UNIQUE constraint on redpoint_customer_id can surface as a sql error if
// the caller tries to bind the same customer to two different UA-Hub users
// concurrently; the error is returned unwrapped so the caller (usually the
// staff UI) can render it verbatim to the operator.
func (s *Store) UpsertMapping(ctx context.Context, m *Mapping) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.MatchedAt == "" {
		m.MatchedAt = time.Now().UTC().Format(time.RFC3339)
	}
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO ua_user_mappings (ua_user_id, redpoint_customer_id, matched_at, matched_by, last_email_synced_at)
        VALUES (?, ?, ?, ?, ?)
        ON CONFLICT(ua_user_id) DO UPDATE SET
            redpoint_customer_id = excluded.redpoint_customer_id,
            matched_at           = excluded.matched_at,
            matched_by           = excluded.matched_by,
            last_email_synced_at = CASE
                WHEN excluded.last_email_synced_at != '' THEN excluded.last_email_synced_at
                ELSE ua_user_mappings.last_email_synced_at
            END
    `, m.UAUserID, m.RedpointCustomer, m.MatchedAt, m.MatchedBy, m.LastEmailSyncedAt)
	return err
}

// TouchMappingEmailSynced records the time the bridge last mirrored the
// Redpoint email into UA-Hub for this mapping. Written after a successful
// UA-Hub UpdateUser(email=...) call so the next sync can detect drift
// quickly.
func (s *Store) TouchMappingEmailSynced(ctx context.Context, uaUserID string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx,
		`UPDATE ua_user_mappings SET last_email_synced_at = ? WHERE ua_user_id = ?`,
		at.UTC().Format(time.RFC3339), uaUserID)
	return err
}

// DeleteMapping removes the mapping for a UA-Hub user. Rare — mainly used
// when staff explicitly un-matches a user via the UI, or when the UA-Hub
// user itself is deleted.
func (s *Store) DeleteMapping(ctx context.Context, uaUserID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `DELETE FROM ua_user_mappings WHERE ua_user_id = ?`, uaUserID)
	return err
}

// AllMappings returns every mapping row. Used by the dashboard summary and
// by the status syncer's fast-path lookup when it walks all UA-Hub users.
func (s *Store) AllMappings(ctx context.Context) ([]Mapping, error) {
	var ms []Mapping
	err := s.db.SelectContext(ctx, &ms, `SELECT * FROM ua_user_mappings ORDER BY matched_at DESC`)
	return ms, err
}

// UpsertPending creates or updates a pending row. The first_seen column is
// preserved on conflict so the grace-window ticker has an accurate "when
// did we start waiting" anchor; last_seen, reason, grace_until, candidates,
// ua_name, and ua_email are refreshed on every call so the cached display
// identity stays in sync with UA-Hub if the operator renames the user or
// adds an email.
func (s *Store) UpsertPending(ctx context.Context, p *Pending) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC().Format(time.RFC3339)
	if p.FirstSeen == "" {
		p.FirstSeen = now
	}
	if p.LastSeen == "" {
		p.LastSeen = now
	}
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO ua_user_mappings_pending
            (ua_user_id, reason, first_seen, last_seen, grace_until, candidates, ua_name, ua_email)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(ua_user_id) DO UPDATE SET
            reason      = excluded.reason,
            last_seen   = excluded.last_seen,
            grace_until = excluded.grace_until,
            candidates  = excluded.candidates,
            ua_name     = excluded.ua_name,
            ua_email    = excluded.ua_email
    `, p.UAUserID, p.Reason, p.FirstSeen, p.LastSeen, p.GraceUntil, p.Candidates, p.UAName, p.UAEmail)
	return err
}

// GetPending looks up a pending row by UA-Hub user ID.
func (s *Store) GetPending(ctx context.Context, uaUserID string) (*Pending, error) {
	var p Pending
	err := s.db.GetContext(ctx, &p, `SELECT * FROM ua_user_mappings_pending WHERE ua_user_id = ?`, uaUserID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &p, err
}

// AllPending returns every pending row, oldest first. Drives the
// /ui/unmatched panel and the per-sync expiry walk.
func (s *Store) AllPending(ctx context.Context) ([]Pending, error) {
	var ps []Pending
	err := s.db.SelectContext(ctx, &ps, `SELECT * FROM ua_user_mappings_pending ORDER BY first_seen ASC`)
	return ps, err
}

// PendingCount returns the count of pending rows. Cheap; used by dashboard
// badges that just want to show "N unmatched users".
func (s *Store) PendingCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.GetContext(ctx, &n, `SELECT COUNT(*) FROM ua_user_mappings_pending`)
	return n, err
}

// DeletePending removes a pending row. Called after a successful auto or
// staff match (and after /ui/unmatched/:id/skip deactivates the user).
func (s *Store) DeletePending(ctx context.Context, uaUserID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `DELETE FROM ua_user_mappings_pending WHERE ua_user_id = ?`, uaUserID)
	return err
}

// ExpiredPending returns pending rows whose grace_until has passed as of
// `now`. These are the candidates for default-deactivation in the current
// sync run.
func (s *Store) ExpiredPending(ctx context.Context, now time.Time) ([]Pending, error) {
	var ps []Pending
	err := s.db.SelectContext(ctx, &ps,
		`SELECT * FROM ua_user_mappings_pending WHERE grace_until <= ? ORDER BY grace_until ASC`,
		now.UTC().Format(time.RFC3339))
	return ps, err
}

// AppendMatchAudit appends a forensic log row. ID is assigned by SQLite on
// insert; callers pass zero. Never errors on duplicate (there's no unique
// constraint on the audit table; we deliberately want every write).
func (s *Store) AppendMatchAudit(ctx context.Context, a *MatchAudit) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a.Timestamp == "" {
		a.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO match_audit (ua_user_id, field, before_val, after_val, source, timestamp)
        VALUES (?, ?, ?, ?, ?, ?)
    `, a.UAUserID, a.Field, a.BeforeVal, a.AfterVal, a.Source, a.Timestamp)
	return err
}

// ListMatchAudit returns audit rows for a UA-Hub user, newest first.
// Drives the per-user forensics view in the staff UI.
func (s *Store) ListMatchAudit(ctx context.Context, uaUserID string, limit int) ([]MatchAudit, error) {
	if limit <= 0 {
		limit = 100
	}
	var rows []MatchAudit
	err := s.db.SelectContext(ctx, &rows,
		`SELECT * FROM match_audit WHERE ua_user_id = ? ORDER BY id DESC LIMIT ?`,
		uaUserID, limit)
	return rows, err
}
