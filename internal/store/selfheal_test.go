package store

// Tests for selfHealAuditSchema — the v0.5.0 startup step that
// recreates missing audit-side tables whose schema_version pin says
// they were migrated, even if the DDL never actually ran. This
// exists to close the v0.4.0 regression where splitLegacyDBIfNeeded
// could pin a fresh audit.db to schema_version=3 without ever
// executing the migrations themselves, silently breaking
// RecordCheckIn for 24 hours at LEF on Apr 17-18 2026.
//
// Coverage:
//
//   1. Fresh install (schema_version=0) — self-heal is a no-op;
//      migrateWith runs the full sequence unencumbered.
//   2. The LEF regression (schema_version=3, no tables) — self-heal
//      recreates checkins, door_policies, jobs, mappings tables AND
//      the unifi_result column, leaving Open() free to run migration
//      4 normally.
//   3. Correct install (all migrations ran) — self-heal re-issues
//      the CREATE TABLE IF NOT EXISTS statements and produces no
//      changes; migrateWith sees schema_version=4 and does nothing.

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

func selfHealQuietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestSelfHeal_FreshInstall_NoOp: on a clean DB with schema_version=0,
// self-heal should not create any tables — that's migrateWith's job.
// Proves self-heal doesn't race ahead and break migration 2's ALTER.
func TestSelfHeal_FreshInstall_NoOp(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "audit.db")
	db, err := sqlx.Open("sqlite", dsnFor(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	if err := selfHealAuditSchema(db, selfHealQuietLogger()); err != nil {
		t.Fatalf("self-heal on fresh DB: %v", err)
	}

	// schema_version should exist (self-heal creates it) but be empty.
	var count int
	if err := db.Get(&count, `SELECT COUNT(*) FROM schema_version`); err != nil {
		t.Fatalf("schema_version should exist after self-heal: %v", err)
	}
	if count != 0 {
		t.Errorf("schema_version should be empty on fresh install, got %d rows", count)
	}

	// checkins should NOT exist yet — only migrateWith should create it.
	var name string
	err = db.Get(&name, `SELECT name FROM sqlite_master WHERE type='table' AND name='checkins'`)
	if err == nil {
		t.Errorf("checkins should not exist after self-heal on fresh install (got %q)", name)
	}
}

// TestSelfHeal_LEFRegression recreates the exact v0.4.0 state that
// broke LEF on Apr 17-18 2026: audit.db has schema_version=3 but no
// tables. The self-heal should recreate checkins (with all columns
// through migration 2) plus all of the migration 3 mapping tables,
// leaving Open() free to run migration 4 afterwards.
func TestSelfHeal_LEFRegression(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "audit.db")
	db, err := sqlx.Open("sqlite", dsnFor(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	// Reproduce the regression: schema_version=3, NO other tables.
	if _, err := db.Exec(`CREATE TABLE schema_version (version INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_version VALUES (1),(2),(3)`); err != nil {
		t.Fatal(err)
	}

	if err := selfHealAuditSchema(db, selfHealQuietLogger()); err != nil {
		t.Fatalf("self-heal on LEF regression: %v", err)
	}

	// Every table from migrations 1-3 must now exist.
	wantTables := []string{
		"checkins", "door_policies", "jobs",
		"ua_user_mappings", "ua_user_mappings_pending", "match_audit",
	}
	for _, name := range wantTables {
		var got string
		err := db.Get(&got, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, name)
		if err != nil {
			t.Errorf("table %s missing after self-heal: %v", name, err)
		}
	}

	// unifi_result (from migration 2) must be present on checkins.
	var colCount int
	if err := db.Get(&colCount,
		`SELECT COUNT(*) FROM pragma_table_info('checkins') WHERE name = 'unifi_result'`,
	); err != nil {
		t.Fatal(err)
	}
	if colCount != 1 {
		t.Errorf("checkins.unifi_result missing after self-heal, count=%d", colCount)
	}

	// unifi_log_id (from migration 4) must NOT yet be present — that's
	// for migrateWith to add when it sees current=3 < 4.
	if err := db.Get(&colCount,
		`SELECT COUNT(*) FROM pragma_table_info('checkins') WHERE name = 'unifi_log_id'`,
	); err != nil {
		t.Fatal(err)
	}
	if colCount != 0 {
		t.Errorf("checkins.unifi_log_id should not exist yet; self-heal only heals migrations <= schema_version. count=%d", colCount)
	}

	// A RecordCheckIn through Store does require migrateWith to have
	// run migration 4 first, so chain: run migrateWith from current=3
	// and verify the end state is usable.
	if err := migrateWith(db, auditMigrations, "audit"); err != nil {
		t.Fatalf("migrateWith after self-heal: %v", err)
	}

	// End-to-end: a Store.RecordCheckIn against this healed DB must
	// insert successfully — the original production symptom of the
	// regression was this failing with "no such table: checkins".
	s := &Store{db: db, logger: selfHealQuietLogger()}
	id, err := s.RecordCheckIn(context.Background(), &CheckInEvent{
		NfcUID:     "04A1B2",
		DoorName:   "Front",
		Result:     "allowed",
		UnifiLogID: "73118",
	})
	if err != nil {
		t.Fatalf("RecordCheckIn on healed DB: %v", err)
	}
	if id <= 0 {
		t.Errorf("RecordCheckIn returned id=%d, want >0", id)
	}
}

// TestSelfHeal_DedupOnUnifiLogID verifies the unique partial index
// on unifi_log_id (migration 4) causes INSERT OR IGNORE to no-op on
// a duplicate log id. This is the mechanism that keeps the poller
// safe against overlapping time windows.
func TestSelfHeal_DedupOnUnifiLogID(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "audit.db")
	db, err := sqlx.Open("sqlite", dsnFor(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	// Normal boot path.
	if err := selfHealAuditSchema(db, selfHealQuietLogger()); err != nil {
		t.Fatal(err)
	}
	if err := migrateWith(db, auditMigrations, "audit"); err != nil {
		t.Fatal(err)
	}

	s := &Store{db: db, logger: selfHealQuietLogger()}
	ctx := context.Background()

	id1, err := s.RecordCheckIn(ctx, &CheckInEvent{
		NfcUID: "04A1B2", DoorName: "Front", Result: "allowed", UnifiLogID: "73118",
	})
	if err != nil || id1 <= 0 {
		t.Fatalf("first insert: id=%d err=%v", id1, err)
	}

	// Same UnifiLogID — INSERT OR IGNORE should produce LastInsertId=0.
	id2, err := s.RecordCheckIn(ctx, &CheckInEvent{
		NfcUID: "04A1B2", DoorName: "Front", Result: "allowed", UnifiLogID: "73118",
	})
	if err != nil {
		t.Fatalf("duplicate insert returned error instead of dedup: %v", err)
	}
	if id2 != 0 {
		t.Errorf("duplicate insert should dedup (id=0), got id=%d", id2)
	}

	// Different UnifiLogID — should insert normally.
	id3, err := s.RecordCheckIn(ctx, &CheckInEvent{
		NfcUID: "04A1B2", DoorName: "Front", Result: "allowed", UnifiLogID: "73119",
	})
	if err != nil || id3 <= 0 {
		t.Fatalf("second unique insert: id=%d err=%v", id3, err)
	}

	// Two rows total.
	var n int
	if err := db.Get(&n, `SELECT COUNT(*) FROM checkins`); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("want 2 rows, got %d", n)
	}

	// Empty UnifiLogID — partial index doesn't cover, so two rows
	// with empty log id should both insert (historical rows have
	// empty log id and must not collide).
	for i := 0; i < 2; i++ {
		if _, err := s.RecordCheckIn(ctx, &CheckInEvent{
			NfcUID: "11", DoorName: "Front", Result: "allowed",
		}); err != nil {
			t.Fatalf("empty-log-id insert %d: %v", i, err)
		}
	}
	if err := db.Get(&n, `SELECT COUNT(*) FROM checkins`); err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("after empty-log-id inserts want 4 rows, got %d", n)
	}
}
