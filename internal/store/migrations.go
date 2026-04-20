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
const cacheSchemaVersion = 4

// cacheSchemaVersionAtSplit pins the cache-side schema version that a
// pre-A4 bridge.db *already embodies* at the moment of the legacy split.
// After splitLegacyDB copies bridge.db into cache.db and prunes the
// audit-side tables, the cache.db contains tables at their pre-A4
// shapes: customers (no badge columns), members, customers_fts. That
// state corresponds to cache migrations 1..3 — i.e., 3.
//
// This constant must stay 3 FOREVER. Any new cache-side migration
// (migration 4 onwards) adds columns or tables the legacy file does
// not have; those migrations must run on the post-split cache.db to
// bring it up to current shape. If we bumped the force-set to the
// current head version, migration 4 would be skipped on the split
// path and every subsequent query for badge_status would fail.
const cacheSchemaVersionAtSplit = 3

// cacheMigrations is the ordered DDL script applied to cache.db.
// Migration 1 creates customers + sync_state (was migration 1 in the
// pre-A4 monolithic sequence). Migration 2 creates members (was 2).
// Migration 3 creates customers_fts + triggers (was 6). Migration 4
// extends customers with badge state so the local mirror can answer
// membership questions without a live Redpoint call. Migrations 3,
// 4, and 5 from the old sequence belong to audit.db and are NOT in
// this list.
var cacheMigrations = []string{
	cacheMigration1_customers,
	cacheMigration2_members,
	cacheMigration3_customers_fts,
	cacheMigration4_customers_badge,
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

// Migration 4 (cache): Badge state on customers, for the local mirror.
//
// Why on customers and not members: the mirror answers membership
// questions for anyone who might check in — not just the subset that's
// already NFC-enrolled. A walk-in whose membership we want to verify
// against Redpoint's answer has a customer row but no member row until
// they enrol a wristband. Keeping badge state on customers means one
// table owns "who holds what at Redpoint right now"; members remains
// the NFC-enrolled cache-first lookup.
//
// Columns:
//   - badge_status  — Customer.Badge.Status: ACTIVE | FROZEN | EXPIRED.
//     Empty string on rows that pre-date the mirror or on customers
//     without a badge (e.g., never-purchased guests).
//   - badge_name    — Customer.Badge.CustomerBadge.Name: "Adult Member",
//     "Day Pass", etc. Useful for reports; not used in the deny path.
//   - past_due_balance — Customer.PastDueBalance in dollars. The
//     validation policy refuses entry above a configured threshold.
//     REAL because the API returns cents as fractional dollars.
//   - home_facility_short_name — Customer.HomeFacility.ShortName.
//     Preserved for the multi-facility future; single-facility today.
//   - last_synced_at — when the mirror last refreshed this row.
//     Used to detect staleness and to sort "most-recently-refreshed
//     N customers" for diagnostics. Populated on every upsert from
//     the walker.
//
// Atomicity: wrapped in BEGIN/COMMIT because ALTER TABLE ADD COLUMN
// is NOT idempotent — a partial failure (one added, one not) would
// leave schema_version un-advanced but the table half-modified, and
// on retry the first ALTER would fail with "duplicate column name".
// The operator's only recourse would be to drop cache.db. A
// transaction makes the whole migration all-or-nothing.
const cacheMigration4_customers_badge = `
BEGIN;

ALTER TABLE customers ADD COLUMN badge_status TEXT NOT NULL DEFAULT '';
ALTER TABLE customers ADD COLUMN badge_name TEXT NOT NULL DEFAULT '';
ALTER TABLE customers ADD COLUMN past_due_balance REAL NOT NULL DEFAULT 0;
ALTER TABLE customers ADD COLUMN home_facility_short_name TEXT NOT NULL DEFAULT '';
ALTER TABLE customers ADD COLUMN last_synced_at TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_customers_badge_status
    ON customers(badge_status) WHERE badge_status != '';

COMMIT;
`

// ─── audit.db migrations ────────────────────────────────────────

// auditSchemaVersion is the current migration count for audit.db.
// Bumped to 5 in v0.5.2 for the ua_name + ua_email columns on
// ua_user_mappings_pending (auditMigration5_pending_ua_identity),
// then to 6 in the same release for the ua_users directory mirror
// (auditMigration6_ua_users), then to 7 in v0.5.4 for the one-shot
// backfill that heals pending rows persisted with blank identity
// by v0.5.2/v0.5.3 (auditMigration7_pending_identity_backfill).
const auditSchemaVersion = 7

// auditSchemaVersionAtSplit pins the audit-side schema version that a
// pre-A4 bridge.db already embodies at the moment of the legacy split
// — i.e., the combined-sequence versions 3, 4, and 5 that are
// audit-side. See cacheSchemaVersionAtSplit for the symmetric rationale;
// any new audit migration after 3 must run on the split copy, so the
// force-set must stay pinned. Migration 4 (unifi_log_id) was added in
// v0.5.0 and runs on already-split installs via migrateWith — the
// split pin does NOT need to move forward because the split predates
// v0.5.0 on every real install.
const auditSchemaVersionAtSplit = 3

// auditMigrations is the ordered DDL script applied to audit.db.
// Migration 1 creates checkins + door_policies + jobs (was 3).
// Migration 2 is the unifi_result ALTER on checkins (was 4).
// Migration 3 creates the UA-Hub mapping tables + match_audit
// (was 5). Migration 4 adds unifi_log_id for tap-poller dedup
// (v0.5.0). Migration 5 caches UA-Hub name/email on the pending
// row so the Needs Match page can render without a live UA-Hub
// ListUsers walk (v0.5.2). Migration 6 creates ua_users, the
// nightly UA-Hub directory mirror that parallels the Redpoint
// customers mirror (v0.5.2). Migration 7 backfills the pending
// rows migration 5 left blank when the matcher persisted records
// before the ua_users mirror had observed them (v0.5.4).
// Migrations 1, 2, and 6 from the old combined sequence are
// cache-side and not in this list.
var auditMigrations = []string{
	auditMigration1_checkins,
	auditMigration2_unifi_result,
	auditMigration3_mappings,
	auditMigration4_unifi_log_id,
	auditMigration5_pending_ua_identity,
	auditMigration6_ua_users,
	auditMigration7_pending_identity_backfill,
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

// Migration 4 (audit): unifi_log_id column on checkins for tap-poller
// dedup (v0.5.0). The poller overlaps its time window to tolerate clock
// skew, so the same tap may be fetched across two polls; the unique
// partial index on non-empty values lets RecordCheckIn use INSERT OR
// IGNORE for a crash-safe dedup that survives process restarts too.
//
// Partial (WHERE != '') because historic checkins have empty log_id
// and mustn't collide with each other.
const auditMigration4_unifi_log_id = `
ALTER TABLE checkins ADD COLUMN unifi_log_id TEXT NOT NULL DEFAULT '';

CREATE UNIQUE INDEX IF NOT EXISTS idx_checkins_unifi_log_id
    ON checkins(unifi_log_id) WHERE unifi_log_id != '';
`

// Migration 5 (audit): cache UA-Hub identity on the pending row (v0.5.2).
//
// Before this change, the /ui/frag/unmatched-list handler enriched each
// pending row by calling s.unifi.ListUsers(ctx) live, which walks the
// full UA-Hub user directory sequentially (17 pages × 100/page at LEF;
// 10s per-page HTTP timeout). When UA-Hub was slow or wedged, the
// Needs Match page hung on its HTMX load for minutes and never painted
// the spinner away. We already hold a full unifi.UniFiUser record at
// UpsertPending time (see statusync.Syncer.persistDecision), so the
// cleanest fix is to persist ua_name + ua_email alongside the pending
// row and stop talking to UA-Hub from the render path.
//
// Both columns default to '' so existing rows upgrade cleanly; the next
// statusync pass re-observes the user and refreshes them via the
// UpsertPending ON CONFLICT refresh clause.
//
// Same "ALTER is not idempotent — rely on schema_version" caveat as
// migration 2. Replay safety on a pre-split bridge.db is not a concern
// for this migration: v0.5.2 ships post-A4, so every install reaches
// migration 5 through migrateWith with a proper audit.schema_version
// check.
const auditMigration5_pending_ua_identity = `
ALTER TABLE ua_user_mappings_pending ADD COLUMN ua_name  TEXT NOT NULL DEFAULT '';
ALTER TABLE ua_user_mappings_pending ADD COLUMN ua_email TEXT NOT NULL DEFAULT '';
`

// Migration 6 (audit): UA-Hub user directory mirror (v0.5.2).
//
// The ingest and recheck paths both call unifi.Client.ListUsers /
// ListAllUsersWithStatus synchronously against UA-Hub — 17 pages ×
// 100/page at LEF, 10s per-page HTTP timeout, no caching. When UA-Hub
// is slow or wedged the cost lands on whichever code path happens to
// ask next, most visibly as the Needs Match hang that motivated
// migration 5. Redpoint already has a nightly directory-walker that
// hydrates a local `customers` mirror so downstream queries never pay
// the upstream round-trip; this table gives UA-Hub the same treatment.
//
// Columns track exactly the unifi.UniFiUser fields we consume. Status
// is stored as-is (UA-Hub emits ACTIVE, DEACTIVATED, DELETED, etc.).
// nfc_tokens is a JSON array literal so operators with `sqlite3` handy
// can see the card list without an additional join; the matcher and
// ingester read it back via json.Unmarshal.
//
// last_synced_at is advanced on every upsert so staff can see when a
// row was last observed from UA-Hub — useful for the "has this user
// been seen since the UA-Hub side was fixed?" diagnostic. first_seen
// is anchored on the initial insert and preserved through conflicts
// so the directory mirror doubles as a rough audit of when each
// UA-Hub user first appeared to the bridge.
//
// Audit-side (not cache-side) because this is operator-observable
// state we don't want to lose when cache.db is nuked for a resync.
// A wipe of the UA-Hub directory mirror would just mean one
// slow-path sync to repopulate — tolerable but noisy — whereas losing
// the match_audit trail would be genuine forensic data loss. Keeping
// the full UA-Hub directory next to the mappings it drives also
// means both sides live under the same transactional locus, should
// we ever want a cross-table constraint.
const auditMigration6_ua_users = `
CREATE TABLE IF NOT EXISTS ua_users (
    id              TEXT PRIMARY KEY,
    first_name      TEXT NOT NULL DEFAULT '',
    last_name       TEXT NOT NULL DEFAULT '',
    name            TEXT NOT NULL DEFAULT '',
    email           TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL DEFAULT '',
    nfc_tokens      TEXT NOT NULL DEFAULT '[]',
    first_seen      TEXT NOT NULL DEFAULT (datetime('now')),
    last_synced_at  TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_ua_users_email
    ON ua_users(lower(email)) WHERE email != '';
CREATE INDEX IF NOT EXISTS idx_ua_users_name
    ON ua_users(lower(first_name), lower(last_name));
CREATE INDEX IF NOT EXISTS idx_ua_users_status
    ON ua_users(status) WHERE status != '';
`

// Migration 7 (audit): backfill pending-row identity cache (v0.5.4).
//
// Migration 5 added ua_name/ua_email to ua_user_mappings_pending with a
// default of '' so existing rows could upgrade cleanly, on the assumption
// that the next statusync pass would refresh them via the UpsertPending
// ON CONFLICT clause. That worked for rows already in the table at the
// time migration 5 applied. It did NOT work for rows newly written by
// the v0.5.2 statusync matcher when the UA-Hub ListAllUsersWithStatus
// paginated response returned incomplete records (first_name /
// last_name / email blank). persistDecision passed those blanks through
// to UpsertPending verbatim; the next statusync pass observed the same
// blanks from upstream, so the ON CONFLICT refresh never healed the
// row. Production LEF accumulated 345 of 345 blank rows that rendered
// "(no name) (no email)" on the Needs Match page — the bug this
// migration corrects.
//
// The v0.5.4 UpsertPending gains a self-heal step that reads from
// ua_users when the caller passes blanks, closing the source of new
// bad rows. This migration repairs the rows already on disk.
//
// Safe to run against any audit.db that has both tables (migrations 5
// and 6) applied — which is every install from v0.5.2 forward. The
// WHERE clause ensures we only overwrite blanks, so rows that already
// have good identity data (or that the v0.5.4 UpsertPending self-heal
// has since populated) are left alone.
const auditMigration7_pending_identity_backfill = `
UPDATE ua_user_mappings_pending
   SET ua_name = COALESCE((
           SELECT CASE WHEN u.name       != '' THEN u.name
                       WHEN u.first_name != '' AND u.last_name != '' THEN u.first_name || ' ' || u.last_name
                       WHEN u.first_name != '' THEN u.first_name
                       WHEN u.last_name  != '' THEN u.last_name
                       ELSE ''
                  END
             FROM ua_users u WHERE u.id = ua_user_mappings_pending.ua_user_id
       ), ua_name)
 WHERE ua_name = '';

UPDATE ua_user_mappings_pending
   SET ua_email = COALESCE((
           SELECT u.email FROM ua_users u
            WHERE u.id = ua_user_mappings_pending.ua_user_id
       ), ua_email)
 WHERE ua_email = '';
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
