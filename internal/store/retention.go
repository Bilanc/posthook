package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Age-based retention for the local store.
//
// posthook keeps every agent hook event forever by default, which on a busy
// machine grows posthook.db by hundreds of MB a year — almost all of it
// tool-call payloads nobody reads again. Prune deletes rows older than a
// cutoff while leaving everything attribution depends on in place:
//
//   - An event that an event_line_ranges row references is an AI edit behind
//     commit attribution (`posthook blame`, commit_sessions, the dashboard's
//     committed-lines figures). Those events are kept forever, whatever their
//     age, and event_line_ranges itself is never pruned.
//   - sessions, repositories, commits, commit_files, commit_sessions and
//     commit_session_files are never touched: they are small and the history
//     views are built from them.
//   - session_prompts (the engineer's typed prompts) are pruned by age like
//     non-edit events.
//
// Timestamps are compared through SQLite's datetime(), which brings the
// RFC3339Nano strings ingest writes, the millisecond "Z" times transcripts
// carry and the `datetime('now')` default on session_prompts.created_at to
// one shape. Compared as text, the space in that last form sorts before the
// "T" of an RFC3339 cutoff and every created_at on the cutoff day reads as
// older than it.

// PruneOptions controls one prune pass.
type PruneOptions struct {
	// OlderThan: rows whose timestamp is before now-OlderThan are candidates.
	// Zero or negative disables pruning entirely.
	OlderThan time.Duration
	// RequireSynced restricts the prune to rows the cloud sync has already
	// flushed (synced_at IS NOT NULL). Set it whenever cloud sync is enabled
	// so a prune can never drop rows the team dashboard hasn't received.
	RequireSynced bool
	// DryRun only counts the rows a real run would delete.
	DryRun bool
}

// PruneResult reports what a prune pass removed (or, for DryRun, would).
type PruneResult struct {
	Cutoff  time.Time
	Events  int64
	Prompts int64
}

// pruneBatchSize bounds how many rows one DELETE removes. Each batch is its
// own implicit transaction, so a large first prune yields the write lock
// between batches instead of blocking hook ingest for its whole duration.
const pruneBatchSize = 5000

// pruneInterval is how often PruneIfDue lets an automatic prune run.
const pruneInterval = 24 * time.Hour

// cutoffLayout renders the cutoff the way SQLite's datetime() prints, so both
// sides of the comparison are in the same shape.
const cutoffLayout = "2006-01-02 15:04:05"

// lastPruneKey is the maintenance row holding the last successful prune time.
const lastPruneKey = "retention_last_prune_at"

// Prune deletes events and session_prompts older than now-OlderThan, in
// batches, honouring the keep-forever rules described at the top of this
// file. It never VACUUMs; call Vacuum separately to give the space back to
// the filesystem.
func (db *DB) Prune(opts PruneOptions) (PruneResult, error) {
	if opts.OlderThan <= 0 {
		return PruneResult{}, nil
	}
	cutoff := time.Now().UTC().Add(-opts.OlderThan)
	res := PruneResult{Cutoff: cutoff}
	cut := cutoff.Format(cutoffLayout)

	synced := ""
	if opts.RequireSynced {
		synced = " AND e.synced_at IS NOT NULL"
	}
	// Edit events are the ones event_line_ranges points at; they back commit
	// attribution and must outlive the retention window. Everything else
	// (reads, bash calls, stop/start markers, misfires) is prunable.
	eventsWhere := `datetime(e.ts) < datetime(?)
		AND NOT EXISTS (SELECT 1 FROM event_line_ranges r WHERE r.event_id = e.id)` + synced
	// Transcript-derived prompts carry the agent's timestamp in ts; hook-
	// delivered ones may not, so fall back to the row's own created_at.
	promptsWhere := `datetime(COALESCE(e.ts, e.created_at)) < datetime(?)` + synced

	var err error
	if res.Events, err = db.pruneTable("events", eventsWhere, cut, opts.DryRun); err != nil {
		return res, fmt.Errorf("prune events: %w", err)
	}
	if res.Prompts, err = db.pruneTable("session_prompts", promptsWhere, cut, opts.DryRun); err != nil {
		return res, fmt.Errorf("prune session_prompts: %w", err)
	}
	return res, nil
}

// pruneTable counts (dryRun) or batch-deletes the rows of table matching
// where, which must reference the table as alias `e`.
func (db *DB) pruneTable(table, where, cutoff string, dryRun bool) (int64, error) {
	if dryRun {
		var n int64
		err := db.QueryRow(`SELECT COUNT(*) FROM `+table+` e WHERE `+where, cutoff).Scan(&n)
		return n, err
	}
	// Candidates are found by a forward scan that stops at the batch limit;
	// rows are inserted in time order so the oldest sit at the front of the
	// table, and each batch's scan only re-walks the kept edit events ahead
	// of it, not the whole table.
	stmt := `DELETE FROM ` + table + ` WHERE id IN (
		SELECT e.id FROM ` + table + ` e WHERE ` + where + ` LIMIT ?)`
	var total int64
	for {
		r, err := db.Exec(stmt, cutoff, pruneBatchSize)
		if err != nil {
			return total, err
		}
		n, err := r.RowsAffected()
		if err != nil {
			return total, err
		}
		total += n
		if n < pruneBatchSize {
			return total, nil
		}
	}
}

// PruneIfDue runs Prune at most once per pruneInterval, so the background
// worker can call it freely. ran reports whether a prune was attempted: false
// when retention is off (OlderThan <= 0) or the last successful prune was
// less than 24h ago. The timestamp is recorded only after a successful,
// non-DryRun prune, so a failed pass is retried on the next call.
func (db *DB) PruneIfDue(opts PruneOptions) (PruneResult, bool, error) {
	if opts.OlderThan <= 0 {
		return PruneResult{}, false, nil
	}
	if last, ok := db.LastPruneAt(); ok {
		// A negative age means the clock went backwards since the last prune;
		// treat that as due rather than locking retention out until it catches up.
		if since := time.Since(last); since >= 0 && since < pruneInterval {
			return PruneResult{}, false, nil
		}
	}
	res, err := db.Prune(opts)
	if err != nil {
		return res, true, err
	}
	if opts.DryRun {
		return res, true, nil
	}
	if err := db.RecordPrune(time.Now()); err != nil {
		return res, true, err
	}
	return res, true, nil
}

// LastPruneAt returns when the last successful prune ran. ok is false when
// no prune has been recorded yet, including on a database whose maintenance
// table has not been created.
func (db *DB) LastPruneAt() (time.Time, bool) {
	var v string
	if err := db.QueryRow(`SELECT value FROM maintenance WHERE key = ?`, lastPruneKey).Scan(&v); err != nil {
		// "no such table" and sql.ErrNoRows both mean never pruned; a
		// transient error reads the same way and at worst re-runs a prune.
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// RecordPrune stamps the maintenance table with a successful prune time, which
// gates PruneIfDue and feeds the `retention:` line in `posthook status`.
func (db *DB) RecordPrune(at time.Time) error {
	if err := db.ensureMaintenanceTable(); err != nil {
		return err
	}
	_, err := db.Exec(`INSERT INTO maintenance (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		lastPruneKey, at.UTC().Format(time.RFC3339))
	return err
}

// ensureMaintenanceTable creates the small key/value table that holds
// housekeeping state. It lives here rather than in schemaSQL because it is
// not product data: it is never synced, never backfilled, and a missing
// table just reads as "never pruned".
func (db *DB) ensureMaintenanceTable() error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS maintenance (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`)
	return err
}

// FileSize reports the database's size on disk (page_count × page_size) and
// how much of it is free pages a VACUUM would give back to the filesystem
// (freelist_count × page_size). Deleting rows grows the freelist; only
// Vacuum shrinks the file.
func (db *DB) FileSize() (bytes int64, reclaimable int64, err error) {
	var pageCount, pageSize, freelist int64
	if err = db.QueryRow(`PRAGMA page_count`).Scan(&pageCount); err != nil {
		return 0, 0, fmt.Errorf("page_count: %w", err)
	}
	if err = db.QueryRow(`PRAGMA page_size`).Scan(&pageSize); err != nil {
		return 0, 0, fmt.Errorf("page_size: %w", err)
	}
	if err = db.QueryRow(`PRAGMA freelist_count`).Scan(&freelist); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			freelist = 0
		} else {
			return 0, 0, fmt.Errorf("freelist_count: %w", err)
		}
	}
	return pageCount * pageSize, freelist * pageSize, nil
}

// Vacuum rebuilds the database file so freed pages are returned to the
// filesystem. VACUUM cannot run inside a transaction and needs the file to
// itself briefly; with the store's single connection a plain Exec is enough.
func (db *DB) Vacuum() error {
	_, err := db.Exec(`VACUUM`)
	return err
}
