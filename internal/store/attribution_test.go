package store

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// newTestDB opens a throwaway sqlite DB with the production schema applied.
func newTestDB(t *testing.T) *DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "posthook-test.db")
	sqlDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	db := &DB{DB: sqlDB}
	if _, err := db.Exec(schemaSQL); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	return db
}

// initGitRepo creates a git repo at dir containing file with the given lines,
// committed once, and returns the commit sha.
func initGitRepo(t *testing.T, dir, file, content string) string {
	t.Helper()
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	run("add", file)
	run("commit", "-q", "-m", "add "+file)
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	return string(out[:40])
}

// TestAttributionDedupesChurnAndBoundsByAdditions is the core regression test:
// two AI edits that each (re)write the same 5 committed lines plus one edit that
// targets lines never added by the commit. The old churn-summing logic returned
// 5+5+11 = 21; the new committed-diff intersection must return exactly 5.
func TestAttributionDedupesChurnAndBoundsByAdditions(t *testing.T) {
	repoDir := t.TempDir()
	const file = "f.txt"
	sha := initGitRepo(t, repoDir, file, "a\nb\nc\nd\ne\n") // 5 added lines: 1..5

	db := newTestDB(t)
	const repoID = "repo1"
	const sessionID = "sess1"
	const commitID = "commit1"

	mustExec(t, db, `INSERT INTO repositories (id, name, root_path) VALUES (?, 'r', ?)`, repoID, repoDir)
	mustExec(t, db, `INSERT INTO sessions (id, agent_slug, model_slug, repo_id, started_at)
		VALUES (?, 'claude-code', 'claude-x', ?, '2024-01-01T10:00:00Z')`, sessionID, repoID)
	mustExec(t, db, `INSERT INTO commits (id, repo_id, sha, committed_at, lines_added)
		VALUES (?, ?, ?, '2024-01-01T11:00:00Z', 5)`, commitID, repoID, sha)
	mustExec(t, db, `INSERT INTO commit_files (commit_id, file_path, lines_added) VALUES (?, ?, 5)`, commitID, file)

	// Three AI edits in-window, all before the commit.
	addEdit := func(eventID, ts string, start, end int) {
		mustExec(t, db, `INSERT INTO events (id, session_id, ts, event_type, agent_slug, repo_id, rel_file_path, payload)
			VALUES (?, ?, ?, 'PostToolUse', 'claude-code', ?, ?, '{"tool_name":"Edit"}')`,
			eventID, sessionID, ts, repoID, file)
		mustExec(t, db, `INSERT INTO event_line_ranges (id, event_id, file_path, rel_file_path, blob_sha_after, start_line, end_line, new_text_lines)
			VALUES (?, ?, ?, ?, 'sha', ?, ?, ?)`,
			"lr-"+eventID, eventID, file, file, start, end, end-start+1)
	}
	addEdit("e1", "2024-01-01T10:10:00Z", 1, 5)     // wrote lines 1..5
	addEdit("e2", "2024-01-01T10:20:00Z", 1, 5)     // rewrote lines 1..5 (churn)
	addEdit("e3", "2024-01-01T10:30:00Z", 100, 110) // never-committed lines

	n, err := db.RefreshCommitAttributions(commitID)
	if err != nil {
		t.Fatalf("RefreshCommitAttributions: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 attributed session, got %d", n)
	}

	var lines int
	var confidence string
	if err := db.QueryRow(`SELECT lines_attributed, confidence FROM commit_sessions WHERE commit_id = ? AND session_id = ?`,
		commitID, sessionID).Scan(&lines, &confidence); err != nil {
		t.Fatalf("query commit_sessions: %v", err)
	}
	if lines != 5 {
		t.Errorf("lines_attributed = %d, want 5 (deduped, bounded by additions)", lines)
	}
	if confidence != "line_range_committed_intersect" {
		t.Errorf("confidence = %q, want line_range_committed_intersect", confidence)
	}

	// Invariant: attributed must never exceed the commit's additions.
	var added int
	mustQueryRow(t, db, `SELECT lines_added FROM commit_files WHERE commit_id = ? AND file_path = ?`, []any{commitID, file}, &added)
	if lines > added {
		t.Errorf("attributed %d exceeds additions %d", lines, added)
	}
}

// TestAttributionFallbackCapsAtAdditions covers the no-git path: when the repo
// root is unreachable, per-file churn is capped at recorded lines_added.
func TestAttributionFallbackCapsAtAdditions(t *testing.T) {
	db := newTestDB(t)
	const repoID = "repo1"
	const sessionID = "sess1"
	const commitID = "commit1"
	const file = "f.txt"

	// root_path points nowhere -> addedLineSet fails -> fallback path.
	mustExec(t, db, `INSERT INTO repositories (id, name, root_path) VALUES (?, 'r', '/nonexistent/path')`, repoID)
	mustExec(t, db, `INSERT INTO sessions (id, agent_slug, repo_id, started_at)
		VALUES (?, 'claude-code', ?, '2024-01-01T10:00:00Z')`, sessionID, repoID)
	mustExec(t, db, `INSERT INTO commits (id, repo_id, sha, committed_at, lines_added)
		VALUES (?, ?, 'deadbeef', '2024-01-01T11:00:00Z', 8)`, commitID, repoID)
	mustExec(t, db, `INSERT INTO commit_files (commit_id, file_path, lines_added) VALUES (?, ?, 8)`, commitID, file)

	mustExec(t, db, `INSERT INTO events (id, session_id, ts, event_type, agent_slug, repo_id, rel_file_path, payload)
		VALUES ('e1', ?, '2024-01-01T10:10:00Z', 'PostToolUse', 'claude-code', ?, ?, '{}')`, sessionID, repoID, file)
	// 50 churn lines on an 8-line-additions commit.
	mustExec(t, db, `INSERT INTO event_line_ranges (id, event_id, file_path, rel_file_path, blob_sha_after, start_line, end_line, new_text_lines)
		VALUES ('lr1', 'e1', ?, ?, 'sha', 1, 50, 50)`, file, file)

	if _, err := db.RefreshCommitAttributions(commitID); err != nil {
		t.Fatalf("RefreshCommitAttributions: %v", err)
	}
	var lines int
	var confidence string
	if err := db.QueryRow(`SELECT lines_attributed, confidence FROM commit_sessions WHERE commit_id = ?`, commitID).Scan(&lines, &confidence); err != nil {
		t.Fatalf("query: %v", err)
	}
	if lines != 8 {
		t.Errorf("lines_attributed = %d, want 8 (capped at additions)", lines)
	}
	if confidence != "fallback_capped" {
		t.Errorf("confidence = %q, want fallback_capped", confidence)
	}
}

// TestAttributionEmitsZeroRowForChurnOnlySession guards the re-sync tombstone:
// a session whose only edits were churn (never landed in the commit) must still
// get a lines_attributed=0 row, so the corrected value overwrites any previously
// synced inflated blob in the append-only landing zone.
func TestAttributionEmitsZeroRowForChurnOnlySession(t *testing.T) {
	repoDir := t.TempDir()
	const file = "f.txt"
	sha := initGitRepo(t, repoDir, file, "a\nb\nc\n") // adds lines 1..3

	db := newTestDB(t)
	const repoID = "repo1"
	const commitID = "commit1"
	mustExec(t, db, `INSERT INTO repositories (id, name, root_path) VALUES (?, 'r', ?)`, repoID, repoDir)
	mustExec(t, db, `INSERT INTO sessions (id, agent_slug, repo_id, started_at) VALUES ('real', 'claude-code', ?, '2024-01-01T10:00:00Z')`, repoID)
	mustExec(t, db, `INSERT INTO sessions (id, agent_slug, repo_id, started_at) VALUES ('churn', 'claude-code', ?, '2024-01-01T10:00:00Z')`, repoID)
	mustExec(t, db, `INSERT INTO commits (id, repo_id, sha, committed_at, lines_added) VALUES (?, ?, ?, '2024-01-01T11:00:00Z', 3)`, commitID, repoID, sha)
	mustExec(t, db, `INSERT INTO commit_files (commit_id, file_path, lines_added) VALUES (?, ?, 3)`, commitID, file)

	addRange := func(sessionID, eventID string, start, end int) {
		mustExec(t, db, `INSERT INTO events (id, session_id, ts, event_type, agent_slug, repo_id, rel_file_path, payload)
			VALUES (?, ?, '2024-01-01T10:10:00Z', 'PostToolUse', 'claude-code', ?, ?, '{}')`, eventID, sessionID, repoID, file)
		mustExec(t, db, `INSERT INTO event_line_ranges (id, event_id, file_path, rel_file_path, blob_sha_after, start_line, end_line, new_text_lines)
			VALUES (?, ?, ?, ?, 'sha', ?, ?, ?)`, "lr-"+eventID, eventID, file, file, start, end, end-start+1)
	}
	addRange("real", "e1", 1, 3)    // landed in the commit
	addRange("churn", "e2", 50, 60) // never committed

	if _, err := db.RefreshCommitAttributions(commitID); err != nil {
		t.Fatalf("RefreshCommitAttributions: %v", err)
	}

	got := map[string]int{}
	rows, err := db.Query(`SELECT session_id, lines_attributed FROM commit_sessions WHERE commit_id = ?`, commitID)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[s] = n
	}
	if got["real"] != 3 {
		t.Errorf("real session lines_attributed = %d, want 3", got["real"])
	}
	if n, ok := got["churn"]; !ok {
		t.Errorf("churn-only session has no row; expected a 0-line tombstone row")
	} else if n != 0 {
		t.Errorf("churn session lines_attributed = %d, want 0", n)
	}
}

func mustExec(t *testing.T, db *DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

func mustQueryRow(t *testing.T, db *DB, query string, args []any, dest ...any) {
	t.Helper()
	if err := db.QueryRow(query, args...).Scan(dest...); err != nil {
		t.Fatalf("queryrow %q: %v", query, err)
	}
}
