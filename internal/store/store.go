package store

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

// Store is the unified SQLite persistence layer, replacing both the JSON
// member cache and the customer directory.
//
// As of A4, the backing storage is split across two SQLite files under
// dataDir:
//
//   - audit.db  — the primary connection (s.db). Holds the canonical
//     audit trail (checkins), operator-configured door policies,
//     operator-curated UA→Redpoint mappings, the match audit log, and
//     background-job records. Must never be lost.
//
//   - cache.db  — ATTACHed to the primary connection as `cache` at
//     Open() time. Holds rebuildable caches (customers, customers_fts,
//     members, sync_state). Safe to wipe; the next full Redpoint sync
//     reconstructs it.
//
// SQLite resolves unqualified table names by searching main (audit.db)
// first, then any attached databases in order. Since no table name
// collides between the two files, every existing query that reads or
// writes `customers`, `members`, `checkins`, etc. continues to work
// unchanged after the split — the attached cache is transparent.
//
// Legacy bridge.db from pre-A4 installs is handled at Open() time by
// splitLegacyDB — see its comment for the migration path.
type Store struct {
	db     *sqlx.DB
	logger *slog.Logger
	mu     sync.RWMutex // protects write transactions
}

// Open creates or opens the split-schema database under dataDir. It
// enforces 0700 on dataDir, runs the one-time legacy split if a
// pre-A4 bridge.db is present, runs per-side migrations, and ATTACHes
// cache.db to the audit.db primary connection.
func Open(dataDir string, logger *slog.Logger) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	_ = os.Chmod(dataDir, 0o700)

	auditPath := filepath.Join(dataDir, "audit.db")
	cachePath := filepath.Join(dataDir, "cache.db")
	legacyPath := filepath.Join(dataDir, "bridge.db")

	// Step 1: if a pre-A4 bridge.db exists and we haven't already split
	// it on a previous boot, perform the one-time split before any other
	// Open/migrate work. This preserves every byte of audit history; the
	// original file is renamed to bridge.db.pre-a4.bak so an operator
	// can verify and delete it when confident.
	if err := splitLegacyDBIfNeeded(legacyPath, auditPath, cachePath, logger); err != nil {
		return nil, fmt.Errorf("split legacy bridge.db: %w", err)
	}

	// Step 2: run cache.db migrations on a standalone connection. We
	// don't leave cache.db attached during migration because some DDL
	// (in particular FTS5 virtual-table creation and the trigger
	// definitions in migration 3) behaves more predictably when the
	// DB is the main schema of its connection rather than an
	// attached one. After this function returns, cache.db is closed
	// here and re-opened via ATTACH on the primary connection below.
	if err := runCacheMigrations(cachePath, logger); err != nil {
		return nil, fmt.Errorf("cache migrations: %w", err)
	}

	// Step 3: open audit.db as the primary long-lived connection.
	dsn := dsnFor(auditPath)
	db, err := sqlx.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open audit.db: %w", err)
	}
	// SetMaxOpenConns(1) is deliberate — DO NOT raise this without
	// understanding what follows.
	//
	// modernc.org/sqlite is a pure-Go translation of the SQLite C source.
	// Each `sql.DB` connection backs a distinct sqlite3* handle with its
	// own transaction state, statement cache, AND list of ATTACHed
	// databases. WAL mode lets multiple *processes* read concurrently
	// while one writes, but inside a single Go process the per-connection
	// isolation has surprised every Go+SQLite codebase that's tried to
	// "speed things up" with a larger pool:
	//
	//   - A read on connection A started before a write on connection B
	//     commits will see the pre-write snapshot until A's transaction
	//     ends. Stale reads are trivially observable in "write here,
	//     immediately read there" code.
	//   - Writes from N goroutines on N connections all try to grab the
	//     SQLite reserved-lock; only one wins. The rest block on
	//     `_busy_timeout` or fail with SQLITE_BUSY — more contention,
	//     not more throughput.
	//   - Prepared statements and the FTS5 virtual-table session are
	//     bound to the conn that prepared them. With a pool, the cache
	//     splits N ways and most plans get re-prepared per call.
	//   - CRITICAL for A4: ATTACH DATABASE is per-connection. If we had
	//     N connections in the pool, we'd need to ATTACH cache.db on
	//     each one. A single-connection pool plus SetConnMaxLifetime(0)
	//     means we attach once at Open() and the ATTACH stays for the
	//     life of the process.
	//
	// Pinning to 1 makes the application-level mutex (`s.mu` for
	// batched writes, plus the implicit serialisation through this
	// single conn) the *only* writer-coordination mechanism. Reads
	// serialise too, but at our load (peak ~20 reads/sec from the
	// staff UI + a trickle of NFC taps) that's not a bottleneck — and
	// even if it became one, the right answer would be a separate
	// read-only `*sql.DB` with its own Open() pointing at the same
	// files with `?mode=ro`, not bumping this number.
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)

	// Step 4: ATTACH cache.db to the primary connection. Quoting the
	// path lets us tolerate spaces in dataDir (e.g. macOS "Application
	// Support"). We don't attach via DSN query string because
	// modernc.org/sqlite doesn't expose an init-hook for per-connection
	// setup; direct SQL on the one conn we keep is the simplest
	// durable approach.
	if _, err := db.Exec(fmt.Sprintf(`ATTACH DATABASE '%s' AS cache`, escapeSQLString(cachePath))); err != nil {
		db.Close()
		return nil, fmt.Errorf("attach cache.db: %w", err)
	}

	// Step 5: run audit.db migrations on the primary connection. We
	// do this AFTER ATTACH so that cross-DB references (none today, but
	// possibly in future migrations) resolve; the idempotent DDL on the
	// audit side doesn't inadvertently touch cache tables because the
	// statements are scoped to tables that only exist in main.
	if err := migrateWith(db, auditMigrations, "audit"); err != nil {
		db.Close()
		return nil, fmt.Errorf("audit migrations: %w", err)
	}

	s := &Store{db: db, logger: logger}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// dsnFor returns the SQLite DSN for a given database file. Kept as a
// helper so cache-migration Open() and primary Open() use identical
// pragmas.
//
// Historical note (surfaced while writing A4 split tests): the DSN-param
// shorthand `?_journal_mode=WAL&_foreign_keys=ON&_busy_timeout=5000`
// that mattn/go-sqlite3 recognises is SILENTLY IGNORED by modernc.org/
// sqlite, which is the pure-Go driver this codebase has used since
// day one. A probe shows pre-A4 installs ran with journal_mode=delete,
// foreign_keys=0, and busy_timeout=0 — not what the string implies.
// The correct modernc syntax is `?_pragma=NAME(VAL)` (one query param
// per pragma), which the driver runs on every new connection.
//
// A4's mandate is the audit/cache split; changing pragma semantics
// across the codebase is NOT in A4's scope because every test and
// production call site has implicitly depended on the current (no-FK,
// no-WAL, no-busy-timeout) behaviour. We preserve that exact behaviour
// here and log the discrepancy in architecture-review.md as a separate
// follow-up. Any A5+ pragma work can flip to `_pragma=` form once the
// downstream churn is planned for.
func dsnFor(path string) string {
	return fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=ON", path)
}

// escapeSQLString escapes single-quotes for use inside a SQL string
// literal. We can't bind dataDir through a parameter because ATTACH
// DATABASE doesn't accept one — the path must be inline. In practice
// dataDir is operator-configured at boot and can't contain attacker-
// controlled input, but doubling quotes is still the right default.
func escapeSQLString(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			out = append(out, '\'', '\'')
			continue
		}
		out = append(out, s[i])
	}
	return string(out)
}

// runCacheMigrations opens cache.db as a standalone connection, runs
// its migration sequence, and closes the connection. Kept separate so
// cache-side DDL is not entangled with audit-side DDL during boot.
func runCacheMigrations(cachePath string, logger *slog.Logger) error {
	db, err := sqlx.Open("sqlite", dsnFor(cachePath))
	if err != nil {
		return fmt.Errorf("open cache.db: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)
	if err := migrateWith(db, cacheMigrations, "cache"); err != nil {
		return err
	}
	return nil
}

// splitLegacyDBIfNeeded performs the one-time pre-A4 → A4 migration:
// an existing bridge.db is detected, duplicated into audit.db and
// cache.db, the "wrong-side" tables are dropped from each copy, and
// the legacy file is renamed to bridge.db.pre-a4.bak.
//
// Preconditions:
//   - legacyPath exists
//   - auditPath does NOT exist (we have not already migrated)
//
// If those aren't both true, this is a no-op. That way:
//   - Fresh installs (no bridge.db) skip the legacy path entirely.
//   - Post-migration boots (bridge.db.pre-a4.bak + audit.db + cache.db)
//     see no legacyPath file and proceed normally.
//   - A weird half-migrated state (bridge.db AND audit.db present) is
//     left alone rather than re-split, which would either duplicate
//     data or fail mid-way. The operator must manually reconcile.
//
// Safety approach: duplicate the file *before* modifying either copy,
// so at every moment there is at least one intact snapshot on disk.
// If anything fails mid-way the bridge.db file is still unchanged
// and the operator can roll back by deleting audit.db/cache.db.
func splitLegacyDBIfNeeded(legacyPath, auditPath, cachePath string, logger *slog.Logger) error {
	legacyInfo, err := os.Stat(legacyPath)
	if os.IsNotExist(err) {
		return nil // fresh install, nothing to migrate
	}
	if err != nil {
		return fmt.Errorf("stat legacy bridge.db: %w", err)
	}
	if _, err := os.Stat(auditPath); err == nil {
		// Already split — nothing to do. We don't refuse boot on
		// this state because a reinstalling operator may have
		// copied both files forward intentionally.
		logger.Info("split-db: audit.db already present, skipping legacy split",
			"legacy", legacyPath)
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat audit.db: %w", err)
	}

	logger.Info("split-db: migrating pre-A4 bridge.db to split audit.db/cache.db",
		"legacy", legacyPath,
		"legacySize", legacyInfo.Size())

	// Step 1: duplicate the legacy file to both new paths. We do a
	// content copy rather than a rename so the legacy file stays
	// intact for rollback.
	if err := copyFile(legacyPath, auditPath); err != nil {
		return fmt.Errorf("copy legacy → audit.db: %w", err)
	}
	if err := copyFile(legacyPath, cachePath); err != nil {
		// Try to clean up the audit.db we just created so the next
		// boot can retry. We ignore the removal error — logging is
		// enough; the legacy bridge.db is still intact.
		_ = os.Remove(auditPath)
		return fmt.Errorf("copy legacy → cache.db: %w", err)
	}

	// Step 2: prune the wrong-side tables from each copy.
	if err := pruneAuditCopy(auditPath); err != nil {
		_ = os.Remove(auditPath)
		_ = os.Remove(cachePath)
		return fmt.Errorf("prune audit.db: %w", err)
	}
	if err := pruneCacheCopy(cachePath); err != nil {
		_ = os.Remove(auditPath)
		_ = os.Remove(cachePath)
		return fmt.Errorf("prune cache.db: %w", err)
	}

	// Step 3: rename the legacy file out of the way. Post-rename the
	// bridge still boots cleanly; operators who want to be extra-safe
	// can check audit.db/cache.db, run a few queries, and then
	// delete bridge.db.pre-a4.bak at their leisure.
	backupPath := legacyPath + ".pre-a4.bak"
	if err := os.Rename(legacyPath, backupPath); err != nil {
		// audit.db and cache.db are good; the rename is a
		// housekeeping step. Log and continue — the next boot will
		// see audit.db present and skip legacy migration via the
		// early-return above, so we won't accidentally re-split.
		logger.Warn("split-db: could not rename legacy file (manual cleanup recommended)",
			"legacy", legacyPath,
			"backup", backupPath,
			"error", err)
		return nil
	}
	logger.Info("split-db: migration complete",
		"audit", auditPath,
		"cache", cachePath,
		"backup", backupPath)
	return nil
}

// copyFile does a straight file-to-file byte copy, creating the
// destination with 0600 permissions. We don't use os.Link because
// hardlinking would let a write on one file corrupt the other.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	return out.Close()
}

// pruneAuditCopy removes cache-side objects from the copy destined to
// become audit.db, then force-sets schema_version to the audit-side
// count. After this function returns, audit.db contains only the
// tables/indexes/triggers it's supposed to and normal migrate() on
// boot will be a no-op.
func pruneAuditCopy(path string) error {
	db, err := sqlx.Open("sqlite", dsnFor(path))
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)

	// Order matters: drop FTS triggers before dropping the virtual
	// table they reference, or the DROP TRIGGER resolution misses.
	stmts := []string{
		`DROP TRIGGER IF EXISTS customers_fts_ai`,
		`DROP TRIGGER IF EXISTS customers_fts_ad`,
		`DROP TRIGGER IF EXISTS customers_fts_au`,
		`DROP TABLE IF EXISTS customers_fts`,
		`DROP TABLE IF EXISTS members`,
		`DROP TABLE IF EXISTS customers`,
		`DROP TABLE IF EXISTS sync_state`,
		`DELETE FROM schema_version`,
		`INSERT INTO schema_version (version) VALUES (?)`,
	}
	for i, q := range stmts {
		if i == len(stmts)-1 {
			if _, err := db.Exec(q, auditSchemaVersionAtSplit); err != nil {
				return fmt.Errorf("%s: %w", q, err)
			}
			continue
		}
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("%s: %w", q, err)
		}
	}
	return nil
}

// pruneCacheCopy removes audit-side objects from the copy destined to
// become cache.db, then force-sets schema_version to the cache-side
// count at the moment of split. Mirrors pruneAuditCopy.
//
// Post-condition: cache.db contains pre-A4-shape customers + members +
// customers_fts, with schema_version = cacheSchemaVersionAtSplit (3).
// migrateWith on the next boot then applies migrations 4..N to bring
// the file to current head — e.g., migration 4 adds the badge columns.
func pruneCacheCopy(path string) error {
	db, err := sqlx.Open("sqlite", dsnFor(path))
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)

	stmts := []string{
		`DROP TABLE IF EXISTS match_audit`,
		`DROP TABLE IF EXISTS ua_user_mappings_pending`,
		`DROP TABLE IF EXISTS ua_user_mappings`,
		`DROP TABLE IF EXISTS jobs`,
		`DROP TABLE IF EXISTS door_policies`,
		`DROP TABLE IF EXISTS checkins`,
		`DELETE FROM schema_version`,
		`INSERT INTO schema_version (version) VALUES (?)`,
	}
	for i, q := range stmts {
		if i == len(stmts)-1 {
			if _, err := db.Exec(q, cacheSchemaVersionAtSplit); err != nil {
				return fmt.Errorf("%s: %w", q, err)
			}
			continue
		}
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("%s: %w", q, err)
		}
	}
	return nil
}
