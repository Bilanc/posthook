package store

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"sync"

	"github.com/bilanc/posthook/internal/paths"

	_ "modernc.org/sqlite"
)

// DB is the posthook SQLite handle. We wrap *sql.DB so callers don't import
// database/sql directly and so we can swap in a Postgres impl later behind
// the same surface.
type DB struct {
	*sql.DB
}

var (
	cached     *DB
	cachedOnce sync.Mutex
)

// Open returns a process-cached DB handle. Creates ~/.posthook/ if missing,
// applies the schema + migrations + backfill passes on first open.
func Open() (*DB, error) {
	cachedOnce.Lock()
	defer cachedOnce.Unlock()
	if cached != nil {
		return cached, nil
	}
	if err := os.MkdirAll(paths.PosthookDir(), 0o755); err != nil {
		return nil, err
	}

	// DSN-level pragmas for modernc.org/sqlite. WAL allows concurrent
	// readers + writers. busy_timeout=5000 lets racing writers wait for the
	// lock instead of failing instantly with SQLITE_BUSY — without this,
	// codex's parallel hook fires surface as "PostToolUse hook (failed)".
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(on)")
	dsn := paths.DBPath() + "?" + q.Encode()

	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// Concurrent writers from hook spawn-bursts: cap connections and force
	// a single writer at a time via busy_timeout above.
	sqlDB.SetMaxOpenConns(1)

	db := &DB{DB: sqlDB}

	// Steady-state fast path. The schema apply, migrations, full-table
	// backfills, and attribution refresh are one-time *upgrade* work, not
	// per-open work — running them on every `posthook ingest` was scanning the
	// whole DB on every agent tool call and, under hook spawn-bursts, piling up
	// processes that saturated the CPU. Once the on-disk schema matches this
	// binary, opening the DB is just connect + pragmas. Maintenance re-runs only
	// when the binary is newer than the DB (a version bump below forces exactly
	// one pass after an upgrade).
	if db.currentSchemaVersion() < schemaVersion {
		if err := db.migrate(); err != nil {
			cached = nil
			_ = sqlDB.Close()
			return nil, err
		}
	}

	cached = db
	return cached, nil
}

// OpenReadOnly opens the database as it is on disk — no directory or file
// creation, no migration, no shared cache — for callers that must not
// change anything, such as `posthook uninstall --dry-run`. The handle is
// the caller's to close. Fails when the file is missing.
func OpenReadOnly() (*DB, error) {
	return openReadOnly(paths.DBPath())
}

func openReadOnly(path string) (*DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	// A file: URI so SQLite itself sees mode=ro; the driver passes the URI
	// through untouched and still applies the _pragma params.
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", "query_only(1)")
	q.Add("_pragma", "busy_timeout(5000)")
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}).String()
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite read-only: %w", err)
	}
	sqlDB.SetMaxOpenConns(1)
	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("open sqlite read-only: %w", err)
	}
	return &DB{DB: sqlDB}, nil
}

// migrate brings an out-of-date database up to the current schemaVersion: it
// applies the schema + idempotent column migrations, runs the data-repair
// backfills, refreshes attribution, and records the new version. Gated by
// Open() so it runs once per upgrade rather than on every ingest.
func (db *DB) migrate() error {
	if _, err := db.Exec(schemaSQL); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	if err := db.applyMigrations(); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}

	currentVersion := db.currentSchemaVersion()
	// Versions through v12 already completed the historical repair passes. A
	// v12 → v13 upgrade only needs to create the new indexes above; repeating
	// every backfill here would make a performance-only upgrade scan the whole
	// database again.
	if currentVersion >= 12 {
		if _, err := db.Exec(`INSERT INTO schema_version (version) VALUES (?)`, schemaVersion); err != nil {
			return err
		}
		return nil
	}

	if err := db.normalizeEventTypes(); err != nil {
		return err
	}

	// Before backfillEventSessions, or the phantom sessions it would create
	// from cursor-compat duplicate events come right back.
	if _, err := db.deleteCursorCompatDuplicates(); err != nil {
		return err
	}
	if _, err := db.backfillEventSessions(); err != nil {
		return err
	}
	if _, err := db.backfillEventRepos(); err != nil {
		return err
	}
	if _, err := db.backfillLineRangeRelPaths(); err != nil {
		return err
	}
	if _, err := db.backfillAfterFileEditLineRanges(); err != nil {
		return err
	}
	if _, err := db.backfillApplyPatchLineRanges(); err != nil {
		return err
	}
	if _, err := db.deleteDuplicateCursorPostToolUseEditRanges(); err != nil {
		return err
	}
	if _, err := db.backfillSessionRepos(); err != nil {
		return err
	}
	if _, err := db.backfillSessionModels(); err != nil {
		return err
	}
	if _, err := db.backfillSessionEngineers(); err != nil {
		return err
	}
	if _, err := db.backfillSessionTokens(); err != nil {
		return err
	}
	if _, err := db.backfillSessionPrompts(); err != nil {
		return err
	}

	// The backfills above only touch rows that need repair; on a one-shot
	// upgrade pass we simply refresh attribution once to fold them in.
	if _, err := db.RefreshCommitAttributions(""); err != nil {
		return err
	}

	if currentVersion < schemaVersion {
		if _, err := db.Exec(`INSERT INTO schema_version (version) VALUES (?)`, schemaVersion); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) currentSchemaVersion() int {
	var n sql.NullInt64
	_ = db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&n)
	if !n.Valid {
		return 0
	}
	return int(n.Int64)
}

// Close shuts down the cached DB. Mostly for tests; the process-cached handle
// otherwise lives until process exit.
func Close() error {
	cachedOnce.Lock()
	defer cachedOnce.Unlock()
	if cached == nil {
		return nil
	}
	err := cached.DB.Close()
	cached = nil
	return err
}

// nullableOf returns sql.NullString for non-empty strings, NULL otherwise.
func nullableOf(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
