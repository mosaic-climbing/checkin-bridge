package store

// The schema is split across two SQLite files as of A4:
//
//   - cache.db  — tables rebuildable from Redpoint / UA-Hub truth
//                 (customers, customers_fts, sync_state, members).
//                 Wiping this file and re-running the next sync
//                 reconstructs it; operator impact is a few seconds of
//                 "pending initial sync" on cold taps.
//   - audit.db  — tables that must never be lost
//                 (checkins, door_policies, jobs, ua_user_mappings,
//                 ua_user_mappings_pending, match_audit). Every
//                 business-relevant write lands here; every
//                 operator-curated binding lives here.
//
// Each file carries its own `schema_version` table and its own
// monotonic migration sequence. The audit-side migration numbers are
// distinct from the cache-side numbers — they're independent streams.
// Before A4 there was a single combined sequence (1..6); the mapping
// from old-version to new-version for existing installs is handled in
// splitLegacyDB (see store.go).
//
// Why both cache and audit are CREATE TABLE IF NOT EXISTS: so that a
// split-in-place on an existing bridge.db (which already contains every
// table at its final v6 shape) is a no-op for the DDL and the
// schema_version adjustment is the only state that actually changes.

import (
	"fmt"

	"github.com/jmoiron/sqlx"
)

// ─── cache.db migrations ────────────────────────────────────────

// cacheSchemaVersion is the current migration count for cache.db.
// Increment when appending to cacheMigrations.
const cacheSchemaVersion = 3

// cacheMigrations is the ordered DDL script applied to cache.db.
// Migration 1 creates customers + sync_state (was migration 1 in the
// pre-A4 monolithic sequence). Migration 2 creates members (was 2).
// Migration 3 creates customers_fts + triggers (was 6). Migrations 3,
// 4, and 5 from the old sequence belong to audit.db and are NOT in
// this list.
var cacheMigrations = []string{
	cacheMigration1_customers,
	cacheMigration2_members,
	cacheMigration3_customers_fts,
}

// Migration 1 (cache): Customer directory + sync state.
const cacheMigration1_customers = `
CREATE TABLE IF NOT EXISTS customers (
    redpoint_id  TEXT PRIMARY KEY,
    first_name   TEXT NOT NULL DEFAULT '',
    last_name    TEXT NOT NULL DEFAULT '',
    email        TEXT NOT NULL DEFAULT '',
    barcode      TEXT NOT NULL DEFAULT '',
    external_id  TEXT NOT NULL DEFAULT '',
    active       INTEGER NOT NULL DEFAULT 1,
    created_at   TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at   TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_customers_name
    ON customers(lower(first_name), lower(last_name));
CREATE INDEX IF NOT EXISTS idx_customers_email
    ON customers(lower(email)) WHERE email != '';
CREATE INDEX IF NOT EXISTS idx_customers_external_id
    ON customers(external_id) WHERE external_id != '';
CREATE INDEX IF NOT EXISTS idx_customers_barcode
    ON customers(barcode) WHERE barcode != '';

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
`

// Migration 2 (cache): Members (NFC-enrolled, cache-first lookup).
// The foreign key to customers stays intra-cache.db so PRAGMA
// foreign_keys=ON continues to enforce it after the split.
const cacheMigration2_members = `
CREATE TABLE IF NOT EXISTS members (
    nfc_uid       TEXT PRIMARY KEY,
    customer_id   TEXT NOT NULL,
    barcode       TEXT NOT NULL DEFAULT '',
    first_name    TEXT NOT NULL DEFAULT '',
    last_name     TEXT NOT NULL DEFAULT '',
    badge_status  TEXT NOT NULL DEFAULT 'PENDING_SYNC',
    badge_name    TEXT NOT NULL DEFAULT '',
    active        INTEGER NOT NULL DEFAULT 1,
    cached_at     TEXT NOT NULL DEFAULT (datetime('now')),
    last_checkin  TEXT NOT NULL DEFAULT '',
    FOREIGN KEY (customer_id) REFERENCES customers(redpoint_id)
);

CREATE INDEX IF NOT EXISTS idx_members_customer_id ON members(customer_id);
CREATE INDEX IF NOT EXISTS idx_members_barcode ON members(barcode) WHERE barcode != '';
`

// Migration 3 (cache): Customer-directory full-text search.
//
// FTS5 design rationale (unchanged from the pre-A4 migration 6):
// contentless FTS with UNINDEXED redpoint_id so we can join back to
// customers; unicode61 + remove_diacritics for "Ramírez" ≡ "ramirez";
// trigger-driven sync because partial-column updates aren't supported
// in contentless mode; initial backfill for existing rows on first run.
//
// Important: on a post-split bridge.db (via splitLegacyDB) this
// migration is effectively a replay — customers_fts already exists
// at its v6 shape, the triggers are there, and the backfill SELECT
// into an already-populated FTS would duplicate rows. That's why
// splitLegacyDB force-sets cache.schema_version to 3 after the copy;
// this migration then does not re-run.
const cacheMigration3_customers_fts = `
CREATE VIRTUAL TABLE IF NOT EXISTS customers_fts USING fts5(
    redpoint_id UNINDEXED,
    name,
    email,
    external_id,
    barcode,
    tokenize='unicode61 remove_diacritics 2'
);

INSERT INTO customers_fts (redpoint_id, name, email, external_id, barcode)
SELECT redpoint_id,
       trim(first_name || ' ' || last_name),
       email,
       external_id,
       barcode
FROM customers;

CREATE TRIGGER IF NOT EXISTS customers_fts_ai AFTER INSERT ON customers BEGIN
    INSERT INTO customers_fts (redpoint_id, name, email, external_id, barcode)
    VALUES (NEW.redpoint_id,
            trim(NEW.first_name || ' ' || NEW.last_name),
            NEW.email,
            NEW.external_id,
            NEW.barcode);
END;

CREATE TRIGGER IF NOT EXISTS customers_fts_ad AFTER DELETE ON customers BEGIN
    DELETE FROM customers_fts WHERE redpoint_id = OLD.redpoint_id;
END;

CREATE TRIGGER IF NOT EXISTS customers_fts_au AFTER UPDATE ON customers BEGIN
    DELETE FROM customers_fts WHERE redpoint_id = OLD.redpoint_id;
    INSERT INTO customers_fts (redpoint_id, name, email, external_id, barcode)
    VALUES (NEW.redpoint_id,
            trim(NEW.first_name || ' ' || NEW.last_name),
            NEW.email,
            NEW.external_id,
            NEW.barcode);
END;
`

// ─── audit.db migrations ────────────────────────────────────────

// auditSchemaVersion is the current migration count for audit.db.
const auditSchemaVersion = 3

// auditMigrations is the ordered DDL script applied to audit.db.
// Migration 1 creates checkins + door_policies + jobs (was 3).
// Migration 2 is the unifi_result ALTER on checkins (was 4).
// Migration 3 creates the UA-Hub mapping tables + match_audit
// (was 5). Migrations 1, 2, and 6 from the old sequence are
// cache-side and not in this list.
var auditMigrations = []string{
	auditMigration1_checkins,
	auditMigration2_unifi_result,
	auditMigration3_mappings,
}

// Migration 1 (audit): Check-in event log, door policies, background jobs.
const auditMigration1_checkins = `
CREATE TABLE IF NOT EXISTS checkins (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp    TEXT NOT NULL DEFAULT (datetime('now')),
    nfc_uid      TEXT NOT NULL,
    customer_id  TEXT NOT NULL DEFAULT '',
    customer_name TEXT NOT NULL DEFAULT '',
    door_id      TEXT NOT NULL DEFAULT '',
    door_name    TEXT NOT NULL DEFAULT '',
    result       TEXT NOT NULL,
    deny_reason  TEXT NOT NULL DEFAULT '',
    redpoint_recorded INTEGER NOT NULL DEFAULT 0,
    redpoint_checkin_id TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_checkins_timestamp ON checkins(timestamp);
CREATE INDEX IF NOT EXISTS idx_checkins_customer ON checkins(customer_id) WHERE customer_id != '';
CREATE INDEX IF NOT EXISTS idx_checkins_nfc ON checkins(nfc_uid);

CREATE TABLE IF NOT EXISTS door_policies (
    door_id      TEXT PRIMARY KEY,
    door_name    TEXT NOT NULL DEFAULT '',
    policy       TEXT NOT NULL DEFAULT 'membership',
    require_waiver INTEGER NOT NULL DEFAULT 0,
    allowed_badges TEXT NOT NULL DEFAULT '',
    notes        TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS jobs (
    id         TEXT PRIMARY KEY,
    type       TEXT NOT NULL,
    status     TEXT NOT NULL DEFAULT 'pending',
    progress   TEXT NOT NULL DEFAULT '',
    result     TEXT NOT NULL DEFAULT '',
    error      TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status);
`

// Migration 2 (audit): Record UniFi's native decision alongside the
// bridge's decision, used by shadow-mode agreement reporting.
//
// This is an ALTER — not idempotent on its own. SQLite doesn't support
// "ADD COLUMN IF NOT EXISTS" directly, so a replay on a post-split
// bridge.db (where the column already exists) would error. That's
// why splitLegacyDB force-sets audit.schema_version to 3 — the ALTER
// never re-runs on installs that already had this applied.
const auditMigration2_unifi_result = `
ALTER TABLE checkins ADD COLUMN unifi_result TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_checkins_unifi_result
    ON checkins(unifi_result) WHERE unifi_result != '';
`

// Migration 3 (audit): UA-Hub user → Redpoint customer mapping (C2).
//
// The matching layer needs three pieces of state (full rationale lives
// on the pre-A4 migration 5 text — unchanged here):
//
//   - ua_user_mappings: resolved one-to-one bindings.
//   - ua_user_mappings_pending: unmatched queue with grace window.
//   - match_audit: append-only forensic log.
//
// All three are audit-side because they're operator-touched state —
// a staff member resolving an ambiguous match must not lose that
// decision when cache.db is nuked for a re-sync.
const auditMigration3_mappings = `
CREATE TABLE IF NOT EXISTS ua_user_mappings (
    ua_user_id           TEXT PRIMARY KEY,
    redpoint_customer_id TEXT NOT NULL,
    matched_at           TEXT NOT NULL DEFAULT (datetime('now')),
    matched_by           TEXT NOT NULL,
    last_email_synced_at TEXT NOT NULL DEFAULT '',
    UNIQUE (redpoint_customer_id)
);

CREATE INDEX IF NOT EXISTS idx_ua_mappings_customer
    ON ua_user_mappings(redpoint_customer_id);

CREATE TABLE IF NOT EXISTS ua_user_mappings_pending (
    ua_user_id   TEXT PRIMARY KEY,
    reason       TEXT NOT NULL,
    first_seen   TEXT NOT NULL DEFAULT (datetime('now')),
    last_seen    TEXT NOT NULL DEFAULT (datetime('now')),
    grace_until  TEXT NOT NULL,
    candidates   TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_ua_pending_grace
    ON ua_user_mappings_pending(grace_until);

CREATE TABLE IF NOT EXISTS match_audit (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    ua_user_id   TEXT NOT NULL,
    field        TEXT NOT NULL,
    before_val   TEXT NOT NULL DEFAULT '',
    after_val    TEXT NOT NULL DEFAULT '',
    source       TEXT NOT NULL,
    timestamp    TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_match_audit_user
    ON match_audit(ua_user_id);
CREATE INDEX IF NOT EXISTS idx_match_audit_time
    ON match_audit(timestamp);
`

// ─── Migration runners ──────────────────────────────────────────

// migrateWith applies a migration sequence against a target handle.
// The handle must already be configured (WAL, foreign_keys, etc.) —
// migrateWith only advances schema_version and runs the pending DDL.
//
// Both cache.db and audit.db own a table named "schema_version", but
// because they're physically separate SQLite files the names don't
// collide — each tracks its own sequence independently.
//
// Pre-A4 schema_version lookups started off `*sqlx.DB.Get`; the signature
// stays the same so the cutover is a local change to Open() rather than
// every call site.
func migrateWith(db *sqlx.DB, migrations []string, label string) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("%s: create schema_version: %w", label, err)
	}
	var current int
	if err := db.Get(&current, `SELECT COALESCE(MAX(version), 0) FROM schema_version`); err != nil {
		return fmt.Errorf("%s: read schema_version: %w", label, err)
	}
	for i := current; i < len(migrations); i++ {
		if _, err := db.Exec(migrations[i]); err != nil {
			return fmt.Errorf("%s: migration %d: %w", label, i+1, err)
		}
		if _, err := db.Exec(`INSERT INTO schema_version (version) VALUES (?)`, i+1); err != nil {
			return fmt.Errorf("%s: record schema_version %d: %w", label, i+1, err)
		}
	}
	return nil
}
