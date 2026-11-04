package store

import (
	"database/sql"
	"fmt"
	"testing"
	"time"
)

const (
	oldTS    = "2020-01-01T00:00:00.000000Z"
	oldTS2   = "2020-06-01T12:30:00.000000Z"
	pruneAge = 30 * 24 * time.Hour
)

// newRetentionDB mirrors the production handle: a single connection with
// foreign keys enforced, so deleting a referenced event would fail loudly
// instead of silently orphaning its line ranges.
func newRetentionDB(t *testing.T) *DB {
	t.Helper()
	db := newTestDB(t)
	db.SetMaxOpenConns(1)
	mustExec(t, db, `PRAGMA foreign_keys = ON`)
	mustExec(t, db, `INSERT INTO sessions (id, agent_slug, started_at) VALUES ('s1', 'claude-code', ?)`, oldTS)
	return db
}

func insertEvent(t *testing.T, db *DB, id, ts string, synced bool) {
	t.Helper()
	var syncedAt sql.NullString
	if synced {
		syncedAt = sql.NullString{String: ts, Valid: true}
	}
	mustExec(t, db, `INSERT INTO events (id, session_id, ts, event_type, agent_slug, payload, synced_at)
		VALUES (?, 's1', ?, 'PostToolUse', 'claude-code', '{"tool_name":"Bash"}', ?)`, id, ts, syncedAt)
}

func insertLineRange(t *testing.T, db *DB, eventID string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO event_line_ranges (id, event_id, file_path, start_line, end_line, new_text_lines)
		VALUES (?, ?, 'f.txt', 1, 3, 3)`, "lr-"+eventID, eventID)
}

func insertPrompt(t *testing.T, db *DB, id string, ts sql.NullString, createdAt string, seq int, synced bool) {
	t.Helper()
	var syncedAt sql.NullString
	if synced {
		syncedAt = sql.NullString{String: createdAt, Valid: true}
	}
	mustExec(t, db, `INSERT INTO session_prompts (id, session_id, ts, seq, prompt_text, created_at, synced_at)
		VALUES (?, 's1', ?, ?, 'hi', ?, ?)`, id, ts, seq, createdAt, syncedAt)
}

func count(t *testing.T, db *DB, table string) int {
	t.Helper()
	var n int
	mustQueryRow(t, db, `SELECT COUNT(*) FROM `+table, nil, &n)
	return n
}

func recentTS() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func TestPruneDeletesOldNonEditEvents(t *testing.T) {
	db := newRetentionDB(t)
	insertEvent(t, db, "old", oldTS, false)

	res, err := db.Prune(PruneOptions{OlderThan: pruneAge})
	if err != nil {
		t.Fatal(err)
	}
	if res.Events != 1 {
		t.Fatalf("events deleted = %d, want 1", res.Events)
	}
	if got := count(t, db, "events"); got != 0 {
		t.Fatalf("events remaining = %d, want 0", got)
	}
	if res.Cutoff.IsZero() || time.Since(res.Cutoff) < pruneAge-time.Minute {
		t.Fatalf("cutoff %v is not ~%s ago", res.Cutoff, pruneAge)
	}
}

func TestPruneKeepsOldEventsWithLineRanges(t *testing.T) {
	db := newRetentionDB(t)
	insertEvent(t, db, "edit", oldTS, false)
	insertLineRange(t, db, "edit")
	insertEvent(t, db, "bash", oldTS2, false)

	res, err := db.Prune(PruneOptions{OlderThan: pruneAge})
	if err != nil {
		t.Fatal(err)
	}
	if res.Events != 1 {
		t.Fatalf("events deleted = %d, want 1 (only the non-edit event)", res.Events)
	}
	var remaining string
	mustQueryRow(t, db, `SELECT id FROM events`, nil, &remaining)
	if remaining != "edit" {
		t.Fatalf("remaining event = %q, want the edit event", remaining)
	}
	if got := count(t, db, "event_line_ranges"); got != 1 {
		t.Fatalf("event_line_ranges = %d, want 1 (never pruned)", got)
	}
}

func TestPruneKeepsRecentEvents(t *testing.T) {
	db := newRetentionDB(t)
	insertEvent(t, db, "recent", recentTS(), false)
	insertEvent(t, db, "old", oldTS, false)

	res, err := db.Prune(PruneOptions{OlderThan: pruneAge})
	if err != nil {
		t.Fatal(err)
	}
	if res.Events != 1 {
		t.Fatalf("events deleted = %d, want 1", res.Events)
	}
	var remaining string
	mustQueryRow(t, db, `SELECT id FROM events`, nil, &remaining)
	if remaining != "recent" {
		t.Fatalf("remaining event = %q, want recent", remaining)
	}
}

func TestPruneDeletesOldPrompts(t *testing.T) {
	db := newRetentionDB(t)
	// ts set and old.
	insertPrompt(t, db, "p-old-ts", sql.NullString{String: oldTS, Valid: true}, recentTS(), 1, false)
	// ts NULL, created_at old — falls back to created_at.
	insertPrompt(t, db, "p-old-created", sql.NullString{}, oldTS2, 2, false)
	// ts recent — kept.
	insertPrompt(t, db, "p-recent", sql.NullString{String: recentTS(), Valid: true}, recentTS(), 3, false)

	res, err := db.Prune(PruneOptions{OlderThan: pruneAge})
	if err != nil {
		t.Fatal(err)
	}
	if res.Prompts != 2 {
		t.Fatalf("prompts deleted = %d, want 2", res.Prompts)
	}
	var remaining string
	mustQueryRow(t, db, `SELECT id FROM session_prompts`, nil, &remaining)
	if remaining != "p-recent" {
		t.Fatalf("remaining prompt = %q, want p-recent", remaining)
	}
}

func TestPruneReadsCreatedAtInSQLiteShape(t *testing.T) {
	db := newRetentionDB(t)
	// session_prompts.created_at defaults to datetime('now'), "YYYY-MM-DD
	// HH:MM:SS" with a space. Compared as text against an RFC3339 cutoff the
	// space sorts before the "T", so a prompt created an hour ago on the
	// cutoff day would be deleted.
	const sqliteLayout = "2006-01-02 15:04:05"
	insertPrompt(t, db, "p-fresh", sql.NullString{}, time.Now().UTC().Add(-time.Hour).Format(sqliteLayout), 1, false)
	insertPrompt(t, db, "p-stale", sql.NullString{}, time.Now().UTC().Add(-3*time.Hour).Format(sqliteLayout), 2, false)

	res, err := db.Prune(PruneOptions{OlderThan: 2 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if res.Prompts != 1 {
		t.Fatalf("prompts deleted = %d, want 1", res.Prompts)
	}
	var remaining string
	mustQueryRow(t, db, `SELECT id FROM session_prompts`, nil, &remaining)
	if remaining != "p-fresh" {
		t.Fatalf("remaining prompt = %q, want p-fresh", remaining)
	}
}

func TestPruneRequireSyncedKeepsUnsyncedRows(t *testing.T) {
	db := newRetentionDB(t)
	insertEvent(t, db, "unsynced", oldTS, false)
	insertEvent(t, db, "synced", oldTS2, true)
	insertPrompt(t, db, "p-unsynced", sql.NullString{String: oldTS, Valid: true}, oldTS, 1, false)
	insertPrompt(t, db, "p-synced", sql.NullString{String: oldTS2, Valid: true}, oldTS2, 2, true)

	res, err := db.Prune(PruneOptions{OlderThan: pruneAge, RequireSynced: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Events != 1 || res.Prompts != 1 {
		t.Fatalf("deleted events=%d prompts=%d, want 1/1", res.Events, res.Prompts)
	}
	var ev, pr string
	mustQueryRow(t, db, `SELECT id FROM events`, nil, &ev)
	mustQueryRow(t, db, `SELECT id FROM session_prompts`, nil, &pr)
	if ev != "unsynced" || pr != "p-unsynced" {
		t.Fatalf("remaining event=%q prompt=%q, want the unsynced rows", ev, pr)
	}

	// Without the gate the leftovers go too.
	res, err = db.Prune(PruneOptions{OlderThan: pruneAge})
	if err != nil {
		t.Fatal(err)
	}
	if res.Events != 1 || res.Prompts != 1 {
		t.Fatalf("second pass deleted events=%d prompts=%d, want 1/1", res.Events, res.Prompts)
	}
}

func TestPruneDryRunDeletesNothing(t *testing.T) {
	db := newRetentionDB(t)
	insertEvent(t, db, "old", oldTS, false)
	insertEvent(t, db, "edit", oldTS2, false)
	insertLineRange(t, db, "edit")
	insertPrompt(t, db, "p-old", sql.NullString{String: oldTS, Valid: true}, oldTS, 1, false)

	res, err := db.Prune(PruneOptions{OlderThan: pruneAge, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Events != 1 || res.Prompts != 1 {
		t.Fatalf("dry run counted events=%d prompts=%d, want 1/1", res.Events, res.Prompts)
	}
	if got := count(t, db, "events"); got != 2 {
		t.Fatalf("events after dry run = %d, want 2", got)
	}
	if got := count(t, db, "session_prompts"); got != 1 {
		t.Fatalf("prompts after dry run = %d, want 1", got)
	}
}

func TestPruneDisabledWhenOlderThanNotPositive(t *testing.T) {
	db := newRetentionDB(t)
	insertEvent(t, db, "old", oldTS, false)
	for _, age := range []time.Duration{0, -time.Hour} {
		res, err := db.Prune(PruneOptions{OlderThan: age})
		if err != nil {
			t.Fatal(err)
		}
		if res.Events != 0 || !res.Cutoff.IsZero() {
			t.Fatalf("OlderThan=%v pruned %+v, want nothing", age, res)
		}
	}
	if got := count(t, db, "events"); got != 1 {
		t.Fatalf("events = %d, want 1", got)
	}
}

func TestPruneRunsInBatches(t *testing.T) {
	db := newRetentionDB(t)
	const n = pruneBatchSize + 3
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if _, err := tx.Exec(`INSERT INTO events (id, session_id, ts, event_type, agent_slug)
			VALUES (?, 's1', ?, 'PostToolUse', 'claude-code')`, fmt.Sprintf("e%d", i), oldTS); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	res, err := db.Prune(PruneOptions{OlderThan: pruneAge})
	if err != nil {
		t.Fatal(err)
	}
	if res.Events != n {
		t.Fatalf("events deleted = %d, want %d", res.Events, n)
	}
	if got := count(t, db, "events"); got != 0 {
		t.Fatalf("events remaining = %d, want 0", got)
	}
}

func TestPruneIfDueRunsOnceThenSkips(t *testing.T) {
	db := newRetentionDB(t)
	if _, ok := db.LastPruneAt(); ok {
		t.Fatal("LastPruneAt should be unset on a fresh database")
	}
	insertEvent(t, db, "old1", oldTS, false)

	res, ran, err := db.PruneIfDue(PruneOptions{OlderThan: pruneAge})
	if err != nil {
		t.Fatal(err)
	}
	if !ran || res.Events != 1 {
		t.Fatalf("first call ran=%v events=%d, want ran with 1 deleted", ran, res.Events)
	}
	last, ok := db.LastPruneAt()
	if !ok || time.Since(last) > time.Minute {
		t.Fatalf("LastPruneAt = %v ok=%v, want just now", last, ok)
	}

	insertEvent(t, db, "old2", oldTS, false)
	res, ran, err = db.PruneIfDue(PruneOptions{OlderThan: pruneAge})
	if err != nil {
		t.Fatal(err)
	}
	if ran || res.Events != 0 {
		t.Fatalf("second call within 24h ran=%v events=%d, want skipped", ran, res.Events)
	}
	if got := count(t, db, "events"); got != 1 {
		t.Fatalf("events = %d, want the second old event untouched", got)
	}

	// Backdate the stamp past the interval: due again.
	if err := db.RecordPrune(time.Now().Add(-pruneInterval - time.Hour)); err != nil {
		t.Fatal(err)
	}
	res, ran, err = db.PruneIfDue(PruneOptions{OlderThan: pruneAge})
	if err != nil {
		t.Fatal(err)
	}
	if !ran || res.Events != 1 {
		t.Fatalf("call after 24h ran=%v events=%d, want ran with 1 deleted", ran, res.Events)
	}
}

func TestPruneIfDueSkipsWhenRetentionOff(t *testing.T) {
	db := newRetentionDB(t)
	insertEvent(t, db, "old", oldTS, false)
	_, ran, err := db.PruneIfDue(PruneOptions{OlderThan: 0})
	if err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Fatal("PruneIfDue ran with OlderThan=0")
	}
	if _, ok := db.LastPruneAt(); ok {
		t.Fatal("a skipped prune must not be recorded")
	}
}

func TestPruneIfDueDryRunDoesNotRecord(t *testing.T) {
	db := newRetentionDB(t)
	insertEvent(t, db, "old", oldTS, false)
	res, ran, err := db.PruneIfDue(PruneOptions{OlderThan: pruneAge, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !ran || res.Events != 1 {
		t.Fatalf("ran=%v events=%d, want ran with 1 counted", ran, res.Events)
	}
	if _, ok := db.LastPruneAt(); ok {
		t.Fatal("a dry run must not be recorded")
	}
	if got := count(t, db, "events"); got != 1 {
		t.Fatalf("events = %d, want 1 (dry run)", got)
	}
}

func TestFileSizeAndVacuum(t *testing.T) {
	db := newRetentionDB(t)
	size, reclaimable, err := db.FileSize()
	if err != nil {
		t.Fatal(err)
	}
	if size <= 0 {
		t.Fatalf("size = %d, want > 0", size)
	}
	if reclaimable < 0 || reclaimable > size {
		t.Fatalf("reclaimable = %d out of range for size %d", reclaimable, size)
	}

	// Fill, delete, and check the freelist grows then Vacuum empties it.
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	big := make([]byte, 4096)
	for i := range big {
		big[i] = 'x'
	}
	for i := 0; i < 500; i++ {
		if _, err := tx.Exec(`INSERT INTO events (id, session_id, ts, event_type, agent_slug, payload)
			VALUES (?, 's1', ?, 'PostToolUse', 'claude-code', ?)`, fmt.Sprintf("e%d", i), oldTS, string(big)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Prune(PruneOptions{OlderThan: pruneAge}); err != nil {
		t.Fatal(err)
	}
	after, reclaimable, err := db.FileSize()
	if err != nil {
		t.Fatal(err)
	}
	if reclaimable == 0 {
		t.Fatalf("reclaimable = 0 after deleting %d rows from a %d-byte file", 500, after)
	}
	if err := db.Vacuum(); err != nil {
		t.Fatal(err)
	}
	shrunk, reclaimable, err := db.FileSize()
	if err != nil {
		t.Fatal(err)
	}
	if reclaimable != 0 {
		t.Fatalf("reclaimable = %d after VACUUM, want 0", reclaimable)
	}
	if shrunk >= after {
		t.Fatalf("size after VACUUM = %d, want < %d", shrunk, after)
	}
}
