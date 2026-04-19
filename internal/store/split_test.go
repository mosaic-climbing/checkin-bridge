package store

// Tests for the A4 schema split: cache-side tables (customers,
// customers_fts, sync_state, members) moved to cache.db; audit-side
// tables (checkins, door_policies, jobs, ua_user_mappings,
// ua_user_mappings_pending, match_audit) stayed in audit.db. The
// primary `*sqlx.DB` is audit.db; cache.db is ATTACHed at Open() time.
//
// The test file covers four scenarios that matter for operators:
//
//  1. Fresh install — no pre-A4 bridge.db; boot creates both DBs
//     cleanly.
//  2. Legacy split — an existing bridge.db from pre-A4 is detected
//     at boot, duplicated into audit.db + cache.db, wrong-side
//     tables are pruned from each, and the legacy file is renamed.
//     All rows are preserved.
//  3. Cross-database unqualified name resolution — code written
//     against the pre-A4 monolithic layout (plain `SELECT * FROM
//     customers`) keeps working because SQLite searches main then
//     attached for unqualified names.
//  4. FK + FTS still work post-split — the foreign key from
//     members → customers is enforced inside cache.db; the FTS
//     triggers fire on writes to customers regardless of which
//     connection did the write.

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

// ─── Fixtures ───────────────────────────────────────────────────

// quietLogger drops all log output so -v tests don't drown in boot
// chatter. Scoped helper so edits don't accidentally make split_test's
// expectations depend on other tests' loggers.
func splitQuietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// buildLegacyBridgeDB writes a bridge.db file at path that looks like a
// fully-migrated pre-A4 install: all tables from both sides present at
// their final v6 shapes, schema_version=6, and some seed rows in
// representative tables so the test can verify the split preserved
// every row.
func buildLegacyBridgeDB(t *testing.T, path string) {
	t.Helper()
	db, err := sqlx.Open("sqlite", dsnFor(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// The A4 cache/audit migrations together reconstruct every table
	// that the pre-A4 monolithic sequence used to create — minus the
	// unifi_result ALTER, which was originally migration 4 over the
	// already-created checkins table. Order matters for the FK:
	// customers must exist before members. We run them in the same
	// order the pre-A4 code did.
	legacyOrder := []string{
		cacheMigration1_customers, // 1: customers + sync_state
		cacheMigration2_members,   // 2: members (FK → customers)
		auditMigration1_checkins,  // 3: checkins + door_policies + jobs
		auditMigration2_unifi_result, // 4: ALTER checkins ADD unifi_result
		auditMigration3_mappings,  // 5: ua_user_mappings + pending + match_audit
		cacheMigration3_customers_fts, // 6: customers_fts + triggers
	}
	for i, s := range legacyOrder {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("legacy migration %d: %v", i+1, err)
		}
	}
	// Pre-A4 schema_version tracking wrote one row per migration. The
	// split logic reads MAX(version), so a single row with version=6
	// is equivalent for our purposes.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_version (version) VALUES (6)`); err != nil {
		t.Fatal(err)
	}

	// Seed rows. We cover one row per table so the split can be
	// verified end-to-end rather than just "both files exist".
	_, err = db.Exec(`INSERT INTO customers (redpoint_id, first_name, last_name, email, active)
		VALUES ('rp-1', 'Alex', 'Honnold', 'alex@example.com', 1)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO members (nfc_uid, customer_id, first_name, last_name, badge_status, active)
		VALUES ('NFC-ALEX', 'rp-1', 'Alex', 'Honnold', 'ACTIVE', 1)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO checkins (nfc_uid, customer_id, customer_name, result)
		VALUES ('NFC-ALEX', 'rp-1', 'Alex Honnold', 'allowed')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO door_policies (door_id, door_name, policy) VALUES ('door-1', 'Front', 'membership')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO jobs (id, type, status) VALUES ('j1', 'ingest', 'completed')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO ua_user_mappings (ua_user_id, redpoint_customer_id, matched_by)
		VALUES ('ua-1', 'rp-1', 'auto:email')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO match_audit (ua_user_id, field, source) VALUES ('ua-1', 'mapping', 'auto:email')`)
	if err != nil {
		t.Fatal(err)
	}
}

// ─── Fresh-install path ─────────────────────────────────────────

// TestSplit_FreshInstall_CreatesBothDBs asserts that Open() on an
// empty data dir produces audit.db and cache.db side-by-side with no
// legacy artefacts. The ATTACH must succeed and unqualified queries
// must hit the right side.
func TestSplit_FreshInstall_CreatesBothDBs(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, splitQuietLogger())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	for _, want := range []string{"audit.db", "cache.db"} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("expected %s to exist: %v", want, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "bridge.db")); !os.IsNotExist(err) {
		t.Errorf("expected no legacy bridge.db in a fresh install")
	}

	// Unqualified query on an attached table must resolve.
	ctx := context.Background()
	if err := s.UpsertCustomerBatch(ctx, []Customer{
		{RedpointID: "rp-test", FirstName: "T", LastName: "U", Active: true},
	}); err != nil {
		t.Fatalf("write to attached cache.db failed: %v", err)
	}
	n, err := s.CustomerCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("CustomerCount = %d, want 1", n)
	}
}

// ─── Legacy split path ──────────────────────────────────────────

// TestSplit_LegacyBridgeDB_Migrates asserts the full A4 migration:
// a pre-A4 bridge.db with seeded rows on both sides becomes an
// audit.db + cache.db pair containing exactly the right rows each,
// plus a bridge.db.pre-a4.bak backup.
func TestSplit_LegacyBridgeDB_Migrates(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "bridge.db")
	buildLegacyBridgeDB(t, legacy)

	s, err := Open(dir, splitQuietLogger())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	// Legacy file is moved out of the way.
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Error("bridge.db should have been renamed")
	}
	if _, err := os.Stat(legacy + ".pre-a4.bak"); err != nil {
		t.Errorf("expected bridge.db.pre-a4.bak: %v", err)
	}
	// New files are present.
	for _, want := range []string{"audit.db", "cache.db"} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("expected %s: %v", want, err)
		}
	}

	ctx := context.Background()

	// Rows that should have landed in cache.db (unqualified via ATTACH).
	if n, _ := s.CustomerCount(ctx); n != 1 {
		t.Errorf("customers: got %d, want 1", n)
	}
	m, err := s.GetMemberByNFC(ctx, "NFC-ALEX")
	if err != nil {
		t.Fatalf("GetMemberByNFC: %v", err)
	}
	if m == nil || m.CustomerID != "rp-1" {
		t.Errorf("member not preserved: %+v", m)
	}

	// Rows that should have landed in audit.db.
	checks, err := s.RecentCheckIns(ctx, 10)
	if err != nil {
		t.Fatalf("RecentCheckIns: %v", err)
	}
	if len(checks) != 1 || checks[0].CustomerID != "rp-1" {
		t.Errorf("checkins: %+v", checks)
	}
	p, err := s.GetDoorPolicy(ctx, "door-1")
	if err != nil || p == nil || p.DoorName != "Front" {
		t.Errorf("door policy: %+v (err %v)", p, err)
	}

	// Per-side schema_version should equal the new counts, so
	// migrate() on subsequent boots is a no-op and doesn't re-run
	// the FTS backfill INSERT (which would duplicate rows).
	assertSchemaVersion(t, filepath.Join(dir, "audit.db"), auditSchemaVersion)
	assertSchemaVersion(t, filepath.Join(dir, "cache.db"), cacheSchemaVersion)

	// Verify the prune actually removed the wrong-side tables. The
	// easiest way to assert this is to open each file raw and query
	// sqlite_master.
	// ua_users is created by audit migration 6 (v0.5.2 UA-Hub mirror).
	// It must land on the audit side and never appear on the cache
	// side; put it in both lists so a future table rename or accidental
	// cache-side creation fails loudly here.
	assertTablePresence(t, filepath.Join(dir, "audit.db"),
		present{"checkins", "door_policies", "jobs", "ua_user_mappings", "ua_user_mappings_pending", "match_audit", "ua_users"},
		absent{"customers", "members", "sync_state", "customers_fts"},
	)
	assertTablePresence(t, filepath.Join(dir, "cache.db"),
		present{"customers", "members", "sync_state", "customers_fts"},
		absent{"checkins", "door_policies", "jobs", "ua_user_mappings", "ua_user_mappings_pending", "match_audit", "ua_users"},
	)
}

// TestSplit_LegacyBridgeDB_Idempotent asserts a second Open() on an
// already-split data dir (bridge.db renamed, audit.db + cache.db
// present) is a no-op for the legacy path and doesn't touch rows.
func TestSplit_LegacyBridgeDB_Idempotent(t *testing.T) {
	dir := t.TempDir()
	buildLegacyBridgeDB(t, filepath.Join(dir, "bridge.db"))

	s1, err := Open(dir, splitQuietLogger())
	if err != nil {
		t.Fatal(err)
	}
	s1.Close()

	// Second boot: legacy file already renamed, audit.db present.
	s2, err := Open(dir, splitQuietLogger())
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	defer s2.Close()

	ctx := context.Background()
	// Row count must be exactly 1 — not 2, which would happen if
	// the legacy split ran a second time and copied bridge.db
	// forward again.
	if n, _ := s2.CustomerCount(ctx); n != 1 {
		t.Errorf("customers after re-open: got %d, want 1 (idempotency broken)", n)
	}
	checks, _ := s2.RecentCheckIns(ctx, 10)
	if len(checks) != 1 {
		t.Errorf("checkins after re-open: got %d, want 1", len(checks))
	}
}

// TestSplit_LegacyAndAuditPresent_SkipsSplit asserts that if an
// operator has manually placed both bridge.db AND audit.db in the
// data dir (weird but possible during a recovery), we don't try to
// re-split and silently nuke their audit.db. We just log and proceed
// with the normal open path.
func TestSplit_LegacyAndAuditPresent_SkipsSplit(t *testing.T) {
	dir := t.TempDir()
	// Create an empty audit.db (as though a previous boot partially
	// succeeded) and also a bridge.db.
	audit := filepath.Join(dir, "audit.db")
	if err := os.WriteFile(audit, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Create an empty cache.db too — otherwise runCacheMigrations
	// opens the path, creates a fresh cache.db there, and the test
	// passes trivially. We want to simulate a fully set-up split.
	if err := os.WriteFile(filepath.Join(dir, "cache.db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	buildLegacyBridgeDB(t, filepath.Join(dir, "bridge.db"))

	// Our empty audit.db file isn't a valid SQLite DB, so the
	// subsequent Open() will fail when it tries to run migrations
	// against it — but crucially, the legacy split must NOT have
	// fired. We detect "legacy untouched" by checking bridge.db is
	// still in place.
	_, _ = Open(dir, splitQuietLogger()) // expected to fail

	if _, err := os.Stat(filepath.Join(dir, "bridge.db")); err != nil {
		t.Errorf("bridge.db should remain untouched when audit.db already exists")
	}
}

// ─── Post-split semantics ───────────────────────────────────────

// TestSplit_ForeignKeyEnforcedAcrossATTACH — proves that the FK
// infrastructure inside cache.db (members → customers) is intact after
// ATTACH and would fire if anyone enabled foreign_keys.
//
// Why this is necessary even though production runs with foreign_keys=0:
// SQLite's ATTACH+PRAGMA interaction is subtle. PRAGMA foreign_keys is
// a *connection-level* setting — if it were silently scoped to "main
// only" or got disabled when a database is attached, an operator who
// later wanted to enable enforcement (or a future DSN-pragma fix) would
// find the FK quietly inert. We assert here that toggling the PRAGMA
// on the same connection that ATTACHed cache.db results in real
// enforcement of the cross-table FK.
func TestSplit_ForeignKeyEnforcedAcrossATTACH(t *testing.T) {
	s := testStore(t)
	if _, err := s.db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		t.Fatalf("enable FK: %v", err)
	}
	// Confirm the PRAGMA actually flipped on this conn — guards against
	// a future regression where modernc silently drops PRAGMA on the
	// pinned conn.
	var fk int
	if err := s.db.Get(&fk, `PRAGMA foreign_keys`); err != nil {
		t.Fatalf("query FK: %v", err)
	}
	if fk != 1 {
		t.Fatalf("foreign_keys did not enable: got %d", fk)
	}

	_, err := s.db.Exec(`INSERT INTO members (nfc_uid, customer_id, active) VALUES ('NFC-X', 'rp-nonexistent', 1)`)
	if err == nil {
		t.Fatal("expected FK violation, got nil")
	}
}

// TestSplit_FTSWorksOnAttachedCache — writing a customer through the
// normal store API fires the FTS trigger and the row becomes
// searchable. This fails if the ATTACH broke trigger firing (SQLite
// triggers on attached tables do work, but only if the trigger was
// defined in the same database as the table — which our migration
// ensures since we run cacheMigration3 against cache.db's own conn).
func TestSplit_FTSWorksOnAttachedCache(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if err := s.UpsertCustomerBatch(ctx, []Customer{
		{RedpointID: "rp-2", FirstName: "Lynn", LastName: "Hill", Email: "lynn@example.com", Active: true},
	}); err != nil {
		t.Fatal(err)
	}
	results, err := s.SearchCustomersFTS(ctx, "lynn", 10)
	if err != nil {
		t.Fatalf("SearchCustomersFTS: %v", err)
	}
	if len(results) != 1 || results[0].RedpointID != "rp-2" {
		t.Errorf("FTS search after split: got %+v", results)
	}
}

// ─── Helpers for table/schema presence assertions ───────────────

type present []string
type absent []string

// assertSchemaVersion opens a DB file raw and checks MAX(version) in
// its schema_version table.
func assertSchemaVersion(t *testing.T, path string, want int) {
	t.Helper()
	db, err := sqlx.Open("sqlite", dsnFor(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var got int
	if err := db.Get(&got, `SELECT COALESCE(MAX(version), 0) FROM schema_version`); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("%s schema_version = %d, want %d", path, got, want)
	}
}

// assertTablePresence opens a DB file raw and verifies that every name
// in `have` exists in sqlite_master and every name in `want` does not.
// We check both tables and virtual-table entries (customers_fts lives
// in sqlite_master with type='table' for FTS5 in modernc).
func assertTablePresence(t *testing.T, path string, have present, want absent) {
	t.Helper()
	db, err := sqlx.Open("sqlite", dsnFor(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, name := range have {
		var got string
		err := db.Get(&got, `SELECT name FROM sqlite_master WHERE type IN ('table') AND name = ?`, name)
		if err == sql.ErrNoRows {
			t.Errorf("%s: expected table %q present", path, name)
			continue
		}
		if err != nil {
			t.Errorf("%s: lookup %q: %v", path, name, err)
		}
	}
	for _, name := range want {
		var got string
		err := db.Get(&got, `SELECT name FROM sqlite_master WHERE type IN ('table') AND name = ?`, name)
		if err == nil {
			t.Errorf("%s: expected table %q ABSENT, but it exists", path, name)
		}
	}
}
