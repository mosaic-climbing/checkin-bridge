package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Customer represents a Redpoint customer in the local directory.
type Customer struct {
	RedpointID string `db:"redpoint_id" json:"redpointId"`
	FirstName  string `db:"first_name"  json:"firstName"`
	LastName   string `db:"last_name"   json:"lastName"`
	Email      string `db:"email"       json:"email"`
	Barcode    string `db:"barcode"     json:"barcode"`
	ExternalID string `db:"external_id" json:"externalId"`
	Active     bool   `db:"active"      json:"active"`
	CreatedAt  string `db:"created_at"  json:"createdAt"`
	UpdatedAt  string `db:"updated_at"  json:"updatedAt"`
}

func (c *Customer) FullName() string {
	return strings.TrimSpace(c.FirstName + " " + c.LastName)
}

// SyncState tracks bulk directory load progress.
type SyncState struct {
	Status       string `db:"status"        json:"status"`
	TotalFetched int    `db:"total_fetched" json:"totalFetched"`
	LastCursor   string `db:"last_cursor"   json:"lastCursor"`
	LastError    string `db:"last_error"    json:"lastError"`
	StartedAt    string `db:"started_at"    json:"startedAt"`
	CompletedAt  string `db:"completed_at"  json:"completedAt"`
}

// CustomerCount returns the total number of customers in the directory.
func (s *Store) CustomerCount(ctx context.Context) (int, error) {
	var count int
	err := s.db.GetContext(ctx, &count, `SELECT COUNT(*) FROM customers`)
	return count, err
}

// GetCustomerByID looks up a single customer by Redpoint ID.
func (s *Store) GetCustomerByID(ctx context.Context, redpointID string) (*Customer, error) {
	var c Customer
	err := s.db.GetContext(ctx, &c, `SELECT * FROM customers WHERE redpoint_id = ?`, redpointID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &c, err
}

// SearchCustomersByName searches by first and/or last name (case-insensitive).
func (s *Store) SearchCustomersByName(ctx context.Context, firstName, lastName string) ([]Customer, error) {
	var customers []Customer
	var args []any
	query := `SELECT * FROM customers WHERE 1=1`

	if firstName != "" {
		query += ` AND lower(first_name) LIKE ?`
		args = append(args, strings.ToLower(firstName)+"%")
	}
	if lastName != "" {
		query += ` AND lower(last_name) LIKE ?`
		args = append(args, strings.ToLower(lastName)+"%")
	}
	query += ` ORDER BY last_name, first_name LIMIT 50`

	err := s.db.SelectContext(ctx, &customers, query, args...)
	return customers, err
}

// SearchCustomersByLastName searches by last name only.
func (s *Store) SearchCustomersByLastName(ctx context.Context, lastName string) ([]Customer, error) {
	var customers []Customer
	err := s.db.SelectContext(ctx, &customers,
		`SELECT * FROM customers WHERE lower(last_name) LIKE ? ORDER BY last_name, first_name LIMIT 50`,
		strings.ToLower(lastName)+"%")
	return customers, err
}

// SearchCustomersByEmail searches by exact email (case-insensitive).
func (s *Store) SearchCustomersByEmail(ctx context.Context, email string) (*Customer, error) {
	var c Customer
	err := s.db.GetContext(ctx, &c, `SELECT * FROM customers WHERE lower(email) = ?`, strings.ToLower(email))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &c, err
}

// buildFTSQuery turns a free-text search box into an FTS5 MATCH expression.
//
// The job is small but easy to get wrong: FTS5's query syntax has a handful
// of operators (`AND OR NOT NEAR ()`) and meta-characters (`* " : -`) that
// will either error out or change semantics if a user types them. We:
//
//  1. Trim whitespace and split into tokens.
//  2. Strip every character that is not a letter, digit, `_`, `@`, `.` or
//     `-`. This keeps things like email addresses (alice@example.com) and
//     hyphenated names (smith-jones) intact while neutralising `"`, `(`,
//     `*`, etc. The `@`/`.` survive intact; the unicode61 tokenizer treats
//     them as separators when indexing, so an email is indexed as multiple
//     tokens (alice, example, com) — matching it as a literal phrase needs
//     us to keep the `@`/`.` in the query so FTS5 splits it the same way.
//  3. Wrap each token in double-quotes (FTS5 treats double-quoted text as a
//     literal phrase, even if the contents contain reserved punctuation)
//     and append `*` for prefix matching.
//  4. Join with a space, which is implicit AND in FTS5. Multi-word queries
//     like "alice smith" become `"alice"* "smith"*` — a row matches only if
//     both prefixes are present somewhere across the indexed columns.
//
// Returns the empty string when no usable token survives sanitization, so
// callers can short-circuit to "no results" rather than running a query
// that would return everything.
func buildFTSQuery(q string) string {
	// Allow only characters that are safe inside a quoted FTS5 phrase and
	// useful for our domain (names, emails, ids, barcodes).
	keep := func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_', r == '@', r == '.', r == '-':
			return r
		default:
			return -1 // strip
		}
	}
	var parts []string
	for _, raw := range strings.Fields(q) {
		clean := strings.Map(keep, raw)
		if clean == "" {
			continue
		}
		parts = append(parts, `"`+clean+`"*`)
	}
	return strings.Join(parts, " ")
}

// SearchCustomersFTS runs a single FTS5 prefix-AND search across name,
// email, external_id, and barcode and returns the matching customer rows.
// Replaces the three sequential SearchCustomersBy* fan-out used by the
// /directory/search handler.
//
// Ordering: BM25 with column weights skewed toward name, then email, then
// id columns. BM25 returns lower scores for stronger matches in SQLite, so
// the ORDER BY is plain ASC (no `DESC`).
//
// Limit: callers pass an explicit cap; values <= 0 default to 50 and the
// hard ceiling is 200 — search results are an interactive UI affordance,
// not an export, so unbounded result sets are never useful here.
func (s *Store) SearchCustomersFTS(ctx context.Context, q string, limit int) ([]Customer, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	match := buildFTSQuery(q)
	if match == "" {
		return []Customer{}, nil
	}
	var customers []Customer
	err := s.db.SelectContext(ctx, &customers, `
        SELECT c.*
        FROM customers_fts f
        JOIN customers c ON c.redpoint_id = f.redpoint_id
        WHERE customers_fts MATCH ?
        ORDER BY bm25(customers_fts, 10.0, 5.0, 2.0, 2.0)
        LIMIT ?
    `, match, limit)
	return customers, err
}

// UpsertCustomer inserts or updates a customer record.
func (s *Store) UpsertCustomer(ctx context.Context, c *Customer) error {
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO customers (redpoint_id, first_name, last_name, email, barcode, external_id, active, updated_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(redpoint_id) DO UPDATE SET
            first_name  = excluded.first_name,
            last_name   = excluded.last_name,
            email       = excluded.email,
            barcode     = excluded.barcode,
            external_id = excluded.external_id,
            active      = excluded.active,
            updated_at  = excluded.updated_at
    `, c.RedpointID, c.FirstName, c.LastName, c.Email, c.Barcode, c.ExternalID, c.Active, c.UpdatedAt)
	return err
}

// UpsertCustomerBatch inserts or updates multiple customer records in a single
// transaction.
//
// Performance shape — DO NOT "simplify" this to a loop calling UpsertCustomer.
// The bulk Redpoint→SQLite directory sync feeds 100-row pages here and
// auto-commit per row would run 100 fsyncs per page (≈10× slower at 2k rows
// today, plus blocking every other writer for the duration of the sync).
// The current shape is the textbook fast-bulk-insert pattern for SQLite:
//
//  1. One BEGIN per page → single fsync at COMMIT instead of per-row.
//  2. One PREPARE for the INSERT…ON CONFLICT statement, then Exec reused
//     per row → SQLite parses + plans once, the FTS5 trigger chain compiles
//     once, the bind path is a memcpy after that.
//  3. defer tx.Rollback() is the safety net — Commit() makes Rollback a
//     no-op, so this is correct in both the success and error paths.
//  4. s.mu.Lock() enforces single-writer at the application layer (the DB
//     pool is also pinned to one conn — see store.go) so concurrent batches
//     queue rather than racing for SQLite's reserved-lock with backoff.
//
// The architecture review (P7) called this out as a future bottleneck if the
// directory grows past 20k rows; the optimisation already lands here.
func (s *Store) UpsertCustomerBatch(ctx context.Context, customers []Customer) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
        INSERT INTO customers (redpoint_id, first_name, last_name, email, barcode, external_id, active, updated_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(redpoint_id) DO UPDATE SET
            first_name  = excluded.first_name,
            last_name   = excluded.last_name,
            email       = excluded.email,
            barcode     = excluded.barcode,
            external_id = excluded.external_id,
            active      = excluded.active,
            updated_at  = excluded.updated_at
    `)
	if err != nil {
		return err
	}
	defer stmt.Close()

	now := time.Now().UTC().Format(time.RFC3339)
	for _, c := range customers {
		if c.UpdatedAt == "" {
			c.UpdatedAt = now
		}
		if _, err := stmt.ExecContext(ctx, c.RedpointID, c.FirstName, c.LastName,
			c.Email, c.Barcode, c.ExternalID, c.Active, c.UpdatedAt); err != nil {
			return fmt.Errorf("upsert customer %s: %w", c.RedpointID, err)
		}
	}
	return tx.Commit()
}

// GetSyncState returns the current directory sync state.
func (s *Store) GetSyncState(ctx context.Context) (*SyncState, error) {
	var state SyncState
	err := s.db.GetContext(ctx, &state, `SELECT status, total_fetched, last_cursor, last_error, started_at, completed_at FROM sync_state WHERE id = 1`)
	if err != nil {
		return nil, err
	}
	return &state, nil
}

// UpdateSyncState updates the directory sync state.
func (s *Store) UpdateSyncState(ctx context.Context, state *SyncState) error {
	_, err := s.db.ExecContext(ctx, `
        UPDATE sync_state SET
            status = ?, total_fetched = ?, last_cursor = ?,
            last_error = ?, started_at = ?, completed_at = ?
        WHERE id = 1
    `, state.Status, state.TotalFetched, state.LastCursor, state.LastError, state.StartedAt, state.CompletedAt)
	return err
}
