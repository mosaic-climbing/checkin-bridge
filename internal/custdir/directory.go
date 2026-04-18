// Package custdir provides a local SQLite directory of Redpoint customers.
//
// This is NOT the membership cache — it's a phone book. It maps customer
// names/emails to Redpoint customer IDs so the bridge can match UniFi NFC
// users to Redpoint accounts without hitting the rate-limited API.
//
// Population:
//   - Initial bulk load: pages through all Redpoint customers (~30 min, one-time)
//   - Incremental: polls recent check-ins to discover new customers
//   - Email lookup: instant single-customer adds for new NFC registrations
//
// The JSON membership cache (internal/cache) handles the fast NFC-tap path
// and tracks live membership status. This directory just helps with matching.
package custdir

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

// Directory is a local SQLite store of Redpoint customer records.
type Directory struct {
	db     *sqlx.DB
	logger *slog.Logger
}

// CustomerRecord is a row in the local customer directory.
type CustomerRecord struct {
	RedpointID string  `db:"redpoint_id" json:"redpointId"`
	FirstName  string  `db:"first_name"  json:"firstName"`
	LastName   string  `db:"last_name"   json:"lastName"`
	Email      string  `db:"email"       json:"email"`
	Barcode    string  `db:"barcode"     json:"barcode"`
	ExternalID string  `db:"external_id" json:"externalId"`
	Active     bool    `db:"active"      json:"active"`
	CreatedAt  string  `db:"created_at"  json:"createdAt"`
	UpdatedAt  string  `db:"updated_at"  json:"updatedAt"`
}

// SyncState tracks progress of the bulk Redpoint → SQLite sync.
type SyncState struct {
	Status       string `db:"status"        json:"status"` // idle, running, complete, error
	TotalFetched int    `db:"total_fetched" json:"totalFetched"`
	LastCursor   string `db:"last_cursor"   json:"lastCursor"`
	LastError    string `db:"last_error"    json:"lastError"`
	StartedAt    string `db:"started_at"    json:"startedAt"`
	CompletedAt  string `db:"completed_at"  json:"completedAt"`
}

// Open creates or opens the SQLite directory at the given path.
func Open(dataDir string, logger *slog.Logger) (*Directory, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	dbPath := filepath.Join(dataDir, "customers.db")
	db, err := sqlx.Open("sqlite", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	// SetMaxOpenConns(1) is deliberate. See the long-form note in
	// internal/store/store.go.Open — same reasoning applies: modernc/sqlite
	// + sqlx with a pool > 1 introduces per-connection transaction
	// isolation surprises (write here, read stale row there), forces
	// writers to fight for the SQLite reserved-lock under WAL even though
	// one would have sufficed, and splits the prepared-statement cache.
	// Pinning to 1 lets application-level locking be the single writer-
	// coordination mechanism. Do not raise this without removing every
	// place in this package that assumes a single-writer model.
	db.SetMaxOpenConns(1)

	dir := &Directory{db: db, logger: logger}
	if err := dir.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	// Reset stale "running" status from a previously killed process
	dir.db.Exec(`UPDATE sync_state SET status = 'error', last_error = 'process restarted' WHERE id = 1 AND status = 'running'`)

	count, _ := dir.Count()
	logger.Info("customer directory opened", "path", dbPath, "customers", count)
	return dir, nil
}

func (d *Directory) migrate() error {
	_, err := d.db.Exec(`
		CREATE TABLE IF NOT EXISTS customers (
			redpoint_id TEXT PRIMARY KEY,
			first_name  TEXT NOT NULL DEFAULT '',
			last_name   TEXT NOT NULL DEFAULT '',
			email       TEXT NOT NULL DEFAULT '',
			barcode     TEXT NOT NULL DEFAULT '',
			external_id TEXT NOT NULL DEFAULT '',
			active      INTEGER NOT NULL DEFAULT 1,
			created_at  TEXT NOT NULL DEFAULT '',
			updated_at  TEXT NOT NULL DEFAULT ''
		);

		CREATE INDEX IF NOT EXISTS idx_customers_name
			ON customers(lower(first_name), lower(last_name));

		CREATE INDEX IF NOT EXISTS idx_customers_email
			ON customers(lower(email)) WHERE email != '';

		CREATE INDEX IF NOT EXISTS idx_customers_external_id
			ON customers(external_id) WHERE external_id != '';

		CREATE TABLE IF NOT EXISTS sync_state (
			id             INTEGER PRIMARY KEY CHECK (id = 1),
			status         TEXT NOT NULL DEFAULT 'idle',
			total_fetched  INTEGER NOT NULL DEFAULT 0,
			last_cursor    TEXT NOT NULL DEFAULT '',
			last_error     TEXT NOT NULL DEFAULT '',
			started_at     TEXT NOT NULL DEFAULT '',
			completed_at   TEXT NOT NULL DEFAULT ''
		);

		INSERT OR IGNORE INTO sync_state (id) VALUES (1);
	`)
	return err
}

// Close closes the database.
func (d *Directory) Close() error {
	return d.db.Close()
}

// ─── Lookups ────────────────────────────────────────────────

// SearchByName finds customers matching a first + last name (case-insensitive).
func (d *Directory) SearchByName(firstName, lastName string) ([]CustomerRecord, error) {
	var records []CustomerRecord
	err := d.db.Select(&records, `
		SELECT redpoint_id, first_name, last_name, email, barcode, external_id, active, created_at, updated_at FROM customers
		WHERE lower(first_name) = lower(?) AND lower(last_name) = lower(?)
		LIMIT 10
	`, strings.TrimSpace(firstName), strings.TrimSpace(lastName))
	return records, err
}

// SearchByEmail finds a customer by exact email (case-insensitive).
func (d *Directory) SearchByEmail(email string) (*CustomerRecord, error) {
	var record CustomerRecord
	err := d.db.Get(&record, `
		SELECT redpoint_id, first_name, last_name, email, barcode, external_id, active, created_at, updated_at FROM customers WHERE lower(email) = lower(?) LIMIT 1
	`, strings.TrimSpace(email))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &record, err
}

// SearchByLastName finds customers by last name only (case-insensitive).
func (d *Directory) SearchByLastName(lastName string) ([]CustomerRecord, error) {
	var records []CustomerRecord
	err := d.db.Select(&records, `
		SELECT redpoint_id, first_name, last_name, email, barcode, external_id, active, created_at, updated_at
		FROM customers
		WHERE lower(last_name) = lower(?)
		LIMIT 20
	`, strings.TrimSpace(lastName))
	return records, err
}

// GetByID fetches a customer by Redpoint ID.
func (d *Directory) GetByID(redpointID string) (*CustomerRecord, error) {
	var record CustomerRecord
	err := d.db.Get(&record, `SELECT redpoint_id, first_name, last_name, email, barcode, external_id, active, created_at, updated_at FROM customers WHERE redpoint_id = ?`, redpointID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &record, err
}

// Count returns the total number of customers in the directory.
func (d *Directory) Count() (int, error) {
	var count int
	err := d.db.Get(&count, `SELECT COUNT(*) FROM customers`)
	return count, err
}

// ─── Writes ─────────────────────────────────────────────────

// Upsert inserts or updates a customer record.
func (d *Directory) Upsert(r *CustomerRecord) error {
	_, err := d.db.Exec(`
		INSERT INTO customers (redpoint_id, first_name, last_name, email, barcode, external_id, active, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(redpoint_id) DO UPDATE SET
			first_name  = excluded.first_name,
			last_name   = excluded.last_name,
			email       = excluded.email,
			barcode     = excluded.barcode,
			external_id = excluded.external_id,
			active      = excluded.active,
			updated_at  = excluded.updated_at
	`, r.RedpointID, r.FirstName, r.LastName, r.Email, r.Barcode, r.ExternalID, r.Active, r.CreatedAt, r.UpdatedAt)
	return err
}

// UpsertBatch inserts or updates a batch of customers in a single transaction.
//
// Same load-bearing pattern as store.UpsertCustomerBatch — see the long
// note there. One BEGIN, one PREPARE, Exec-per-row, one COMMIT. Do not
// regress this to a per-row Upsert loop on the assumption that "the DB
// will batch internally"; SQLite will not, and a 5k-customer bulk sync
// would jump from a few seconds to most of a minute, blocking every
// other writer (single-conn pool) for the duration.
func (d *Directory) UpsertBatch(records []CustomerRecord) error {
	tx, err := d.db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Preparex(`
		INSERT INTO customers (redpoint_id, first_name, last_name, email, barcode, external_id, active, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
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

	for _, r := range records {
		_, err := stmt.Exec(r.RedpointID, r.FirstName, r.LastName, r.Email, r.Barcode, r.ExternalID, r.Active, r.CreatedAt, r.UpdatedAt)
		if err != nil {
			return fmt.Errorf("upsert customer %s: %w", r.RedpointID, err)
		}
	}

	return tx.Commit()
}

// ─── Sync State ─────────────────────────────────────────────

// GetSyncState returns the current bulk sync progress.
func (d *Directory) GetSyncState() (*SyncState, error) {
	var state SyncState
	err := d.db.Get(&state, `SELECT status, total_fetched, last_cursor, last_error, started_at, completed_at FROM sync_state WHERE id = 1`)
	return &state, err
}

// UpdateSyncState saves sync progress (used by the bulk loader to resume).
func (d *Directory) UpdateSyncState(state *SyncState) error {
	_, err := d.db.Exec(`
		UPDATE sync_state SET
			status = ?, total_fetched = ?, last_cursor = ?,
			last_error = ?, started_at = ?, completed_at = ?
		WHERE id = 1
	`, state.Status, state.TotalFetched, state.LastCursor, state.LastError, state.StartedAt, state.CompletedAt)
	return err
}

// ─── Bulk Loader ────────────────────────────────────────────

// BulkLoadFromRedpoint pages through all Redpoint customers and stores them
// in SQLite. It's resumable — if interrupted, it picks up from the last cursor.
// This runs as a background job; use GetSyncState() to monitor progress.
func (d *Directory) BulkLoadFromRedpoint(ctx context.Context, fetchPage func(ctx context.Context, pageSize int, cursor *string) (customers []CustomerRecord, nextCursor *string, err error)) error {
	state, err := d.GetSyncState()
	if err != nil {
		return err
	}

	// Resume from last position if interrupted
	var cursor *string
	if (state.Status == "running" || state.Status == "error") && state.LastCursor != "" {
		cursor = &state.LastCursor
		d.logger.Info("resuming bulk load from last cursor",
			"fetched", state.TotalFetched,
			"cursor", state.LastCursor,
		)
	} else {
		state.TotalFetched = 0
		state.StartedAt = time.Now().UTC().Format(time.RFC3339)
	}

	state.Status = "running"
	state.LastError = ""
	state.CompletedAt = ""
	d.UpdateSyncState(state)

	const pageSize = 50
	page := 0

	for {
		page++

		// Rate limit: 3s between pages to avoid overloading Redpoint
		if page > 1 {
			select {
			case <-ctx.Done():
				state.LastError = "interrupted"
				d.UpdateSyncState(state)
				return ctx.Err()
			case <-time.After(3 * time.Second):
			}
		}

		d.logger.Info("bulk loading Redpoint customers",
			"page", page,
			"fetched", state.TotalFetched,
		)

		customers, nextCursor, err := fetchPage(ctx, pageSize, cursor)
		if err != nil {
			state.Status = "error"
			state.LastError = err.Error()
			d.UpdateSyncState(state)
			return fmt.Errorf("fetch page %d: %w", page, err)
		}

		if len(customers) > 0 {
			if err := d.UpsertBatch(customers); err != nil {
				state.Status = "error"
				state.LastError = err.Error()
				d.UpdateSyncState(state)
				return fmt.Errorf("upsert page %d: %w", page, err)
			}
		}

		state.TotalFetched += len(customers)
		if nextCursor != nil {
			state.LastCursor = *nextCursor
		}
		d.UpdateSyncState(state)

		if nextCursor == nil || len(customers) < pageSize {
			break // last page
		}
		cursor = nextCursor
	}

	state.Status = "complete"
	state.CompletedAt = time.Now().UTC().Format(time.RFC3339)
	d.UpdateSyncState(state)

	count, _ := d.Count()
	d.logger.Info("bulk load complete",
		"totalFetched", state.TotalFetched,
		"totalInDB", count,
		"pages", page,
	)

	return nil
}
