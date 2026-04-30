package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// UAUser is the local mirror of one UA-Hub user (internal/unifi.UniFiUser).
//
// The row is written by the nightly unifimirror sync and refreshed as a
// side-effect of the UniFi ingest and statusync paths so any code path
// that already talked to UA-Hub contributes to a fresh mirror. Consumers
// that only need the display identity (Needs Match, audit enrichment,
// the recheck pre-filter) should read from here rather than call
// unifi.Client.ListUsers — the live walk costs 17 pages × 10s HTTP
// timeout at LEF's directory size and was the direct cause of the
// Needs Match hang v0.5.2 fixes.
//
// NfcTokensJSON is stored verbatim as a JSON-encoded []string. The
// NfcTokens accessor decodes on read; callers that write through
// UpsertUAUser pass a []string and the store handles marshalling.
type UAUser struct {
	ID             string `db:"id"              json:"id"`
	FirstName      string `db:"first_name"      json:"firstName"`
	LastName       string `db:"last_name"       json:"lastName"`
	Name           string `db:"name"            json:"name"`
	Email          string `db:"email"           json:"email"`
	Status         string `db:"status"          json:"status"`
	NfcTokensJSON  string `db:"nfc_tokens"      json:"-"`
	FirstSeen      string `db:"first_seen"      json:"firstSeen"`
	LastSyncedAt   string `db:"last_synced_at"  json:"lastSyncedAt"`
}

// NfcTokens decodes the stored JSON array. Returns nil on empty or on
// a malformed payload — the mirror is advisory state and a bad row
// should degrade to "no tokens known" rather than fail the caller.
func (u UAUser) NfcTokens() []string {
	if u.NfcTokensJSON == "" {
		return nil
	}
	var toks []string
	if err := json.Unmarshal([]byte(u.NfcTokensJSON), &toks); err != nil {
		return nil
	}
	return toks
}

// FullName mirrors unifi.UniFiUser.FullName so callers can treat the
// mirror as a drop-in read substitute without reaching for the
// upstream type. Prefers the UA-Hub display name when set, falls back
// to "First Last".
func (u UAUser) FullName() string {
	if u.Name != "" {
		return u.Name
	}
	n := u.FirstName
	if u.LastName != "" {
		if n != "" {
			n += " "
		}
		n += u.LastName
	}
	return n
}

// UpsertUAUser writes a UA-Hub user row, refreshing every observed
// column on conflict. first_seen is preserved from the initial insert
// so the mirror doubles as a rough "when did we first see this UA-Hub
// user" audit trail; last_synced_at is advanced on every call so
// staff can tell at a glance whether the row reflects a recent
// observation.
//
// NfcTokens are marshalled to JSON. A nil or empty slice stores as
// "[]" so the not-null default-'[]' constraint is satisfied and
// NfcTokens() reads back as a nil slice.
func (s *Store) UpsertUAUser(ctx context.Context, u *UAUser, tokens []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC().Format(time.RFC3339)
	if u.LastSyncedAt == "" {
		u.LastSyncedAt = now
	}
	payload := "[]"
	if len(tokens) > 0 {
		b, err := json.Marshal(tokens)
		if err != nil {
			return fmt.Errorf("marshal nfc_tokens: %w", err)
		}
		payload = string(b)
	}
	u.NfcTokensJSON = payload
	// On INSERT, first_seen defaults to the caller-supplied value or `now`
	// when blank. On CONFLICT the DO UPDATE SET list deliberately omits
	// first_seen so the column keeps its original-insert value — that's
	// what makes the mirror double as a "when did we first see this UA-Hub
	// user" audit trail. Earlier versions used a subselect inside COALESCE
	// to look up first_seen on conflict; that subselect was always
	// computed-then-discarded since the UPDATE never touched the column.
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO ua_users
            (id, first_name, last_name, name, email, status, nfc_tokens, first_seen, last_synced_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, COALESCE(NULLIF(?, ''), ?), ?)
        ON CONFLICT(id) DO UPDATE SET
            first_name     = excluded.first_name,
            last_name      = excluded.last_name,
            name           = excluded.name,
            email          = excluded.email,
            status         = excluded.status,
            nfc_tokens     = excluded.nfc_tokens,
            last_synced_at = excluded.last_synced_at
    `, u.ID, u.FirstName, u.LastName, u.Name, u.Email, u.Status,
		u.NfcTokensJSON, u.FirstSeen, now, u.LastSyncedAt)
	return err
}

// GetUAUser returns the mirror row for a UA-Hub user ID. Nil, nil on
// miss, consistent with the rest of the store package.
func (s *Store) GetUAUser(ctx context.Context, id string) (*UAUser, error) {
	var u UAUser
	err := s.db.GetContext(ctx, &u, `SELECT * FROM ua_users WHERE id = ?`, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &u, err
}

// AllUAUsers returns every mirror row, most-recently-synced first.
// Used by operator diagnostics; hot paths should prefer GetUAUser.
func (s *Store) AllUAUsers(ctx context.Context) ([]UAUser, error) {
	var us []UAUser
	err := s.db.SelectContext(ctx, &us,
		`SELECT * FROM ua_users ORDER BY last_synced_at DESC, id ASC`)
	return us, err
}

// AllUAUsersWithNFC returns every mirror row that has at least one NFC
// token, ordered the same way AllUAUsers is. The filter is a string
// comparison on the JSON-encoded tokens column — the schema stores
// nfc_tokens as a JSON array (default '[]' for the empty case), so
// "anything other than '[]'" is the cheap, index-friendly way to ask
// "this user has at least one NFC token enrolled" without parsing JSON
// in SQLite.
//
// Drives the ingest pipeline's Step 1: walking the local UA-Hub mirror
// instead of the live ListUsers endpoint avoids a 17-page × 10s-timeout
// HTTP walk per ingest at LEF's scale, and decouples ingest reliability
// from UA-Hub's. The mirror is refreshed on every ua-hub-mirror tick
// (cfg.Sync.Interval) so a scheduled ingest reads at most one sync
// cycle stale.
func (s *Store) AllUAUsersWithNFC(ctx context.Context) ([]UAUser, error) {
	var us []UAUser
	err := s.db.SelectContext(ctx, &us, `
		SELECT * FROM ua_users
		WHERE nfc_tokens IS NOT NULL AND nfc_tokens != '' AND nfc_tokens != '[]'
		ORDER BY last_synced_at DESC, id ASC`)
	return us, err
}

// SearchUAUsers runs a case-insensitive LIKE match across first_name,
// last_name, name, and email for the reassign-target picker (v0.5.9 #10).
// Each whitespace-separated token in q is AND-ed together so "alice s"
// matches "Alice Smith" (as name tokens) or "alice@s.com" (as an email
// substring). Limit caps the hit list; 50 is plenty for a picker.
//
// This is intentionally a LIKE walk rather than an FTS5 virtual table —
// the ua_users mirror tops out in the low thousands at LEF's scale and
// the reassign picker is cold-path; building an FTS index here would be
// premature optimisation and an extra migration to maintain.
func (s *Store) SearchUAUsers(ctx context.Context, q string, limit int) ([]UAUser, error) {
	if limit <= 0 {
		limit = 50
	}
	q = strings.TrimSpace(q)
	if q == "" {
		return nil, nil
	}
	tokens := strings.Fields(q)
	if len(tokens) == 0 {
		return nil, nil
	}
	var (
		clauses []string
		args    []any
	)
	for _, t := range tokens {
		pat := "%" + strings.ToLower(t) + "%"
		clauses = append(clauses,
			`(LOWER(first_name) LIKE ? OR LOWER(last_name) LIKE ? OR LOWER(name) LIKE ? OR LOWER(email) LIKE ?)`)
		args = append(args, pat, pat, pat, pat)
	}
	args = append(args, limit)
	query := `SELECT * FROM ua_users WHERE ` +
		strings.Join(clauses, " AND ") +
		` ORDER BY last_name, first_name, id LIMIT ?`
	var us []UAUser
	err := s.db.SelectContext(ctx, &us, query, args...)
	return us, err
}

// UAUserCount returns the mirror row count. Cheap — drives the
// sync-page "N users mirrored" stat.
func (s *Store) UAUserCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.GetContext(ctx, &n, `SELECT COUNT(*) FROM ua_users`)
	return n, err
}
