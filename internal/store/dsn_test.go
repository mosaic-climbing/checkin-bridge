package store

// Tests for dsnFor — specifically that the pragmas in the DSN actually
// reach the connection. modernc.org/sqlite silently ignores the
// mattn/go-sqlite3 param shorthand (`_busy_timeout=5000` et al.), so a
// string-level test can pass while the driver applies nothing. These
// tests assert against PRAGMA readback on a live connection instead: a
// regression back to an ignored param form fails here.

import (
	"path/filepath"
	"testing"

	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

func openDSNTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlite", dsnFor(filepath.Join(t.TempDir(), "dsn.db")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	return db
}

func TestDSNBusyTimeoutHonoured(t *testing.T) {
	db := openDSNTestDB(t)

	var timeout int
	if err := db.Get(&timeout, `PRAGMA busy_timeout`); err != nil {
		t.Fatalf("read busy_timeout: %v", err)
	}
	if timeout != 5000 {
		t.Errorf("busy_timeout = %d, want 5000 — DSN pragma not applied by driver", timeout)
	}
}

// TestDSNDefaultsUnchanged pins the pragmas dsnFor deliberately does
// NOT set. Production has run with rollback journal and foreign_keys
// off since day one; enabling either is its own planned change (see
// the dsnFor comment), so a DSN edit that flips one as a side effect
// should fail loudly here.
func TestDSNDefaultsUnchanged(t *testing.T) {
	db := openDSNTestDB(t)

	var mode string
	if err := db.Get(&mode, `PRAGMA journal_mode`); err != nil {
		t.Fatalf("read journal_mode: %v", err)
	}
	if mode != "delete" {
		t.Errorf("journal_mode = %q, want %q", mode, "delete")
	}

	var fk int
	if err := db.Get(&fk, `PRAGMA foreign_keys`); err != nil {
		t.Fatalf("read foreign_keys: %v", err)
	}
	if fk != 0 {
		t.Errorf("foreign_keys = %d, want 0", fk)
	}
}
