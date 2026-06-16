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
	if _, err := db.Exec(schemaSQL); err != nil {
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := db.applyMigrations(); err != nil {
		return nil, fmt.Errorf("apply migrations: %w", err)
	}

	currentVersion := db.currentSchemaVersion()
	if err := db.normalizeEventTypes(); err != nil {
		return nil, err
	}

	eventSessionsBackfilled, err := db.backfillEventSessions()
	if err != nil {
		return nil, err
	}
	eventReposBackfilled, err := db.backfillEventRepos()
	if err != nil {
		return nil, err
	}
	lineRangeRelPathsBackfilled, err := db.backfillLineRangeRelPaths()
	if err != nil {
		return nil, err
	}
	afterFileEditRangesBackfilled, err := db.backfillAfterFileEditLineRanges()
	if err != nil {
		return nil, err
	}
	applyPatchRangesBackfilled, err := db.backfillApplyPatchLineRanges()
	if err != nil {
		return nil, err
	}
	duplicateCursorRangesDeleted, err := db.deleteDuplicateCursorPostToolUseEditRanges()
	if err != nil {
		return nil, err
	}
	if _, err := db.backfillSessionRepos(); err != nil {
		return nil, err
	}
	sessionModelsBackfilled, err := db.backfillSessionModels()
	if err != nil {
		return nil, err
	}
	if _, err := db.backfillSessionEngineers(); err != nil {
		return nil, err
	}
	if _, err := db.backfillSessionTokens(); err != nil {
		return nil, err
	}

	if currentVersion < schemaVersion ||
		eventSessionsBackfilled > 0 ||
		eventReposBackfilled > 0 ||
		lineRangeRelPathsBackfilled > 0 ||
		afterFileEditRangesBackfilled > 0 ||
		applyPatchRangesBackfilled > 0 ||
		duplicateCursorRangesDeleted > 0 ||
		sessionModelsBackfilled > 0 {
		if _, err := db.RefreshCommitAttributions(""); err != nil {
			return nil, err
		}
	}

	if currentVersion < schemaVersion {
		if _, err := db.Exec(`INSERT INTO schema_version (version) VALUES (?)`, schemaVersion); err != nil {
			return nil, err
		}
	}

	cached = db
	return cached, nil
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
