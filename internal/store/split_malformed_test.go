package store

// v0.5.8 coverage for the splitLegacyDB completeness guard (#80).
//
// The bug: pruneAuditCopy pinned schema_version=auditSchemaVersionAtSplit
// unconditionally on the duplicated audit.db. If the legacy bridge.db
// was missing any table that audit migrations 1-3 should have created,
// the pin masked that from migrate() and the bridge booted with a
// half-schema (check-ins failed with "no such table: ua_user_mappings").
// Observed on a v0.4.0 deploy; worked around at the symptom level by
// v0.5.0's schema self-heal.
//
// Fix: legacyBridgeHasAuditTables screens the file before splitLegacyDB
// prunes it. On a miss, splitLegacyDBIfNeeded renames the malformed
// bridge.db aside with a .malformed.<timestamp>.bak suffix and returns
// nil, so the outer boot path creates fresh audit.db/cache.db through
// the normal migration sequence.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

// buildPartialLegacyBridgeDB writes a bridge.db that has SOME audit-side
// tables but is missing one — the exact shape that triggered #80 on the
// v0.4.0 deploy (the MacBook's bridge.db was missing migration 3's
// mapping tables). We intentionally run migrations 1+2 but not 3.
func buildPartialLegacyBridgeDB(t *testing.T, path string) {
	t.Helper()
	db, err := sqlx.Open("sqlite", dsnFor(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Migration 1 (checkins/door_policies/jobs) + migration 2 (ALTER)
	// only. Skip migration 3, so ua_user_mappings et al. never exist.
	for _, s := range []string{
		auditMigration1_checkins,
		auditMigration2_unifi_result,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("partial legacy migration: %v", err)
		}
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	// Pre-A4 would have pinned at version=2 because migration 3 never
	// ran. This is the exact state the bug walked into.
	if _, err := db.Exec(`INSERT INTO schema_version (version) VALUES (2)`); err != nil {
		t.Fatal(err)
	}
}

// TestLegacyBridgeHasAuditTables_CompleteReturnsTrue is the positive
// contract: a fully-migrated legacy DB must pass the guard so the
// regular split can proceed.
func TestLegacyBridgeHasAuditTables_CompleteReturnsTrue(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "bridge.db")
	buildLegacyBridgeDB(t, legacy)

	ok, missing, err := legacyBridgeHasAuditTables(legacy)
	if err != nil {
		t.Fatalf("legacyBridgeHasAuditTables: %v", err)
	}
	if !ok {
		t.Errorf("complete legacy DB flagged malformed (missing=%q)", missing)
	}
	if missing != "" {
		t.Errorf("complete legacy DB reported missing=%q, want empty", missing)
	}
}

// TestLegacyBridgeHasAuditTables_MissingTableReturnsFalse names the
// specific missing table so operators get an actionable log line.
func TestLegacyBridgeHasAuditTables_MissingTableReturnsFalse(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "bridge.db")
	buildPartialLegacyBridgeDB(t, legacy)

	ok, missing, err := legacyBridgeHasAuditTables(legacy)
	if err != nil {
		t.Fatalf("legacyBridgeHasAuditTables: %v", err)
	}
	if ok {
		t.Errorf("partial legacy DB flagged complete; guard would have allowed the pin")
	}
	// buildPartialLegacyBridgeDB skips migration 3; the first check in
	// expectedLegacyAuditTables that's from migration 3 is
	// "ua_user_mappings".
	if missing != "ua_user_mappings" {
		t.Errorf("reported missing=%q, want ua_user_mappings", missing)
	}
}

// TestSplit_MalformedLegacyDB_RenamesAsideAndBootsFresh is the
// end-to-end regression test: a malformed bridge.db must be moved out
// of the way, audit.db/cache.db created from scratch, and the bridge
// must come up usable (check-ins insertable — i.e., the table exists).
// Pre-fix the Open() either returned an error, or — more dangerously —
// succeeded with a half-schema.
func TestSplit_MalformedLegacyDB_RenamesAsideAndBootsFresh(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "bridge.db")
	buildPartialLegacyBridgeDB(t, legacy)

	s, err := Open(dir, splitQuietLogger())
	if err != nil {
		t.Fatalf("Open on malformed legacy DB should have succeeded via rename-aside path, got %v", err)
	}
	defer s.Close()

	// Original bridge.db must have been renamed out of the way.
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Error("malformed bridge.db should have been renamed aside")
	}
	// A .malformed.<timestamp>.bak sibling must exist.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	foundBak := false
	for _, e := range entries {
		if strings.Contains(e.Name(), "bridge.db.malformed.") && strings.HasSuffix(e.Name(), ".bak") {
			foundBak = true
			break
		}
	}
	if !foundBak {
		t.Errorf("expected a bridge.db.malformed.<ts>.bak file in %s; found: %v", dir, entries)
	}

	// Fresh audit.db + cache.db must be present and fully-migrated.
	for _, want := range []string{"audit.db", "cache.db"} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("expected %s: %v", want, err)
		}
	}

	// The tables migration 3 should have created must now exist —
	// this is the assertion that would have failed pre-fix, since
	// pruneAuditCopy would have pinned schema_version=3 on a file
	// that never ran migration 3.
	ctx := context.Background()
	_, err = s.RecordCheckIn(ctx, &CheckInEvent{
		NfcUID:       "NFC-TEST",
		CustomerID:   "rp-none",
		CustomerName: "Unknown",
		Result:       "denied",
	})
	if err != nil {
		t.Errorf("RecordCheckIn on fresh DB after malformed legacy rename: %v (missing audit-side table suggests migrations didn't re-run)", err)
	}
}
