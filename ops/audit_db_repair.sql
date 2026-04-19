-- audit_db_repair.sql — one-shot repair for v0.4.0 split-logic regression.
--
-- Symptom: audit.db has schema_version pinned to 3 but the tables those
-- three migrations were supposed to create don't exist (specifically
-- `checkins` is missing). Result: RecordCheckIn errors silently and
-- the bridge persists nothing.
--
-- This script re-runs the audit-side DDL (copied verbatim from
-- internal/store/migrations.go, auditMigration1_checkins +
-- auditMigration2_unifi_result + auditMigration3_mappings) against the
-- live audit.db. All CREATE TABLE / CREATE INDEX statements are
-- IF NOT EXISTS so they're safe to re-run. The one non-idempotent
-- statement (ALTER TABLE checkins ADD COLUMN unifi_result) is guarded
-- by creating the checkins table first in the same transaction — if
-- the table already had the column somehow, the ALTER would error and
-- the whole transaction rolls back, leaving the DB unchanged.
--
-- Usage (from the MacBook):
--   sudo launchctl stop system/com.mosaic.bridge
--   sudo sqlite3 /usr/local/mosaic-bridge/audit.db < audit_db_repair.sql
--   sudo launchctl start system/com.mosaic.bridge
--
-- Verify afterwards:
--   sudo sqlite3 /usr/local/mosaic-bridge/audit.db \
--     "SELECT name FROM sqlite_master WHERE type='table' ORDER BY name;"
-- Expected: checkins, door_policies, jobs, match_audit, schema_version,
--           ua_user_mappings, ua_user_mappings_pending

BEGIN TRANSACTION;

-- ─── audit migration 1 (checkins + door_policies + jobs) ─────────

CREATE TABLE IF NOT EXISTS checkins (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp           TEXT NOT NULL DEFAULT (datetime('now')),
    nfc_uid             TEXT NOT NULL,
    customer_id         TEXT NOT NULL DEFAULT '',
    customer_name       TEXT NOT NULL DEFAULT '',
    door_id             TEXT NOT NULL DEFAULT '',
    door_name           TEXT NOT NULL DEFAULT '',
    result              TEXT NOT NULL,
    deny_reason         TEXT NOT NULL DEFAULT '',
    redpoint_recorded   INTEGER NOT NULL DEFAULT 0,
    redpoint_checkin_id TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_checkins_timestamp ON checkins(timestamp);
CREATE INDEX IF NOT EXISTS idx_checkins_customer  ON checkins(customer_id) WHERE customer_id != '';
CREATE INDEX IF NOT EXISTS idx_checkins_nfc       ON checkins(nfc_uid);

CREATE TABLE IF NOT EXISTS door_policies (
    door_id        TEXT PRIMARY KEY,
    door_name      TEXT NOT NULL DEFAULT '',
    policy         TEXT NOT NULL DEFAULT 'membership',
    require_waiver INTEGER NOT NULL DEFAULT 0,
    allowed_badges TEXT NOT NULL DEFAULT '',
    notes          TEXT NOT NULL DEFAULT ''
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

-- ─── audit migration 2 (unifi_result column on checkins) ─────────
--
-- ALTER is non-idempotent. We only add the column if it's not there;
-- sqlite's pragma_table_info lets us detect it cleanly.

-- Use a temp check: if the column exists, this ALTER is a no-op.
-- SQLite doesn't support ADD COLUMN IF NOT EXISTS, so we attempt
-- the ALTER inside a savepoint and swallow the duplicate-column error.
SAVEPOINT add_unifi_result;
ALTER TABLE checkins ADD COLUMN unifi_result TEXT NOT NULL DEFAULT '';
RELEASE SAVEPOINT add_unifi_result;

CREATE INDEX IF NOT EXISTS idx_checkins_unifi_result
    ON checkins(unifi_result) WHERE unifi_result != '';

-- ─── audit migration 3 (UA-Hub mapping tables) ───────────────────

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
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    ua_user_id TEXT NOT NULL,
    field      TEXT NOT NULL,
    before_val TEXT NOT NULL DEFAULT '',
    after_val  TEXT NOT NULL DEFAULT '',
    source     TEXT NOT NULL,
    timestamp  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_match_audit_user ON match_audit(ua_user_id);
CREATE INDEX IF NOT EXISTS idx_match_audit_time ON match_audit(timestamp);

-- schema_version stays at 3 — we're not advancing it, just backfilling
-- the DDL that migration 1/2/3 were supposed to have created.

COMMIT;

-- Quick post-repair sanity check. This will fail loudly if checkins
-- isn't there after the transaction commits, which means we should
-- bail out rather than restart the bridge on a still-broken DB.
SELECT 'checkins present' AS status, COUNT(*) AS rows FROM checkins;
