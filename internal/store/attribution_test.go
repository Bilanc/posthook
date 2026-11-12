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

func TestAddedLinesByCommit(t *testing.T) {
	// `git log --format=%x00%H -p --unified=0` for two commits. The first
	// adds a line that itself starts with "++ ", a line of punctuation and a
	// blank one; a file header follows a hunk.
	out := "\x00aaa\n\n" +
		"diff --git a/f.go b/f.go\nindex 1..2 100644\n--- a/f.go\n+++ b/f.go\n" +
		"@@ -2,0 +3,5 @@ func f() {\n+\tx := load()\n+++ odd\n+\n+}\n+\tx := load()\n" +
		"@@ -9 +14 @@\n-old\n+new line\r\n" +
		"diff --git a/g.go b/g.go\nnew file mode 100644\n--- /dev/null\n+++ b/g.go\n@@ -0,0 +1,2 @@\n+package g\n+x := load()\n" +
		"\x00bbb\n\n" +
		"diff --git a/f.go b/f.go\n--- a/f.go\n+++ b/f.go\n@@ -1,0 +2 @@\n+x := load()\n"
	got := addedLinesByCommit([]byte(out))
	const f, g = "a/f.go b/f.go", "a/g.go b/g.go"
	want := map[string]map[addedLine]int{
		"aaa": {{f, "x := load()"}: 2, {f, "++ odd"}: 1, {f, "new line"}: 1, {g, "package g"}: 1, {g, "x := load()"}: 1},
		"bbb": {{f, "x := load()"}: 1},
	}
	if len(got) != len(want) {
		t.Fatalf("commits = %v, want %v", got, want)
	}
	for sha, lines := range want {
		if len(got[sha]) != len(lines) {
			t.Errorf("%s: lines = %v, want %v", sha, got[sha], lines)
			continue
		}
		for line, n := range lines {
			if got[sha][line] != n {
				t.Errorf("%s: %v counted %d times, want %d", sha, line, got[sha][line], n)
			}
		}
	}
}

func TestCarries(t *testing.T) {
	const f, g = "a/f.go b/f.go", "a/g.go b/g.go"
	mine := map[addedLine]int{{f, "a := 1"}: 1, {f, "b := 2"}: 2, {f, "return a"}: 1, {g, "c := 3"}: 2}
	for _, c := range []struct {
		name   string
		theirs map[addedLine]int
		want   bool
	}{
		{"the same lines", map[addedLine]int{{f, "a := 1"}: 1, {f, "b := 2"}: 2}, true},
		{"more than half", map[addedLine]int{{f, "a := 1"}: 1, {f, "b := 2"}: 1, {f, "d := 4"}: 1}, true},
		{"exactly half", map[addedLine]int{{f, "a := 1"}: 1, {f, "d := 4"}: 1}, false},
		{"a repeated line counts as often as both have it", map[addedLine]int{{f, "a := 1"}: 3}, false},
		{"the same text in another file", map[addedLine]int{{f, "c := 3"}: 2}, false},
		{"other lines", map[addedLine]int{{f, "d := 4"}: 4}, false},
		{"nothing to compare", map[addedLine]int{}, false},
	} {
		if got := carries(mine, c.theirs); got != c.want {
			t.Errorf("%s: carries = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestCarriedCommitIsCreditedOnlyWhenAttributedOnItsOwn covers the two ways a
// commit is attributed. On its own, an amended commit carries its original
// and is credited with the session's lines. In the whole-database pass every
// earlier commit closes the window, so the lines are counted once, on the
// original.
func TestCarriedCommitIsCreditedOnlyWhenAttributedOnItsOwn(t *testing.T) {
	repoDir := t.TempDir()
	const file = "f.txt"
	original := initGitRepo(t, repoDir, file, "alpha\nbravo\ncharlie\ndelta\necho\n")
	amend := exec.Command("git", "commit", "-q", "--amend", "-m", "reworded")
	amend.Dir = repoDir
	amend.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
	if out, err := amend.CombinedOutput(); err != nil {
		t.Fatalf("git commit --amend: %v\n%s", err, out)
	}
	head := exec.Command("git", "rev-parse", "HEAD")
	head.Dir = repoDir
	out, err := head.Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	amended := string(out[:40])
	if amended == original {
		t.Fatal("test setup: the amend did not rewrite the commit")
	}

	db := newTestDB(t)
	mustExec(t, db, `INSERT INTO repositories (id, name, root_path) VALUES ('repo1', 'r', ?)`, repoDir)
	mustExec(t, db, `INSERT INTO sessions (id, agent_slug, repo_id, started_at)
		VALUES ('sess1', 'claude-code', 'repo1', '2024-01-01T10:00:00Z')`)
	for _, c := range []struct{ id, sha, at string }{
		{"original", original, "2024-01-01T11:00:00Z"},
		{"amended", amended, "2024-01-01T11:05:00Z"},
	} {
		mustExec(t, db, `INSERT INTO commits (id, repo_id, sha, committed_at, lines_added) VALUES (?, 'repo1', ?, ?, 5)`, c.id, c.sha, c.at)
		mustExec(t, db, `INSERT INTO commit_files (commit_id, file_path, lines_added) VALUES (?, ?, 5)`, c.id, file)
	}
	mustExec(t, db, `INSERT INTO events (id, session_id, ts, event_type, agent_slug, repo_id, rel_file_path, payload)
		VALUES ('e1', 'sess1', '2024-01-01T10:10:00Z', 'PostToolUse', 'claude-code', 'repo1', ?, '{}')`, file)
	mustExec(t, db, `INSERT INTO event_line_ranges (id, event_id, file_path, rel_file_path, blob_sha_after, start_line, end_line, new_text_lines)
		VALUES ('lr1', 'e1', ?, ?, 'sha', 1, 5, 5)`, file, file)

	lines := func(commitID string) int {
		var n int
		mustQueryRow(t, db, `SELECT COALESCE(SUM(lines_attributed), 0) FROM commit_sessions WHERE commit_id = ?`, []any{commitID}, &n)
		return n
	}

	if carried := db.CarriedCommits("amended", amended, "repo1", "2024-01-01T11:05:00Z", repoDir); len(carried) != 1 || carried[0] != "original" {
		t.Fatalf("the amended commit carries %v, want [original]", carried)
	}
	if carried := db.CarriedCommits("original", original, "repo1", "2024-01-01T11:00:00Z", repoDir); len(carried) != 0 {
		t.Errorf("the original carries %v, want nothing: the amend came after it", carried)
	}
	if _, err := db.RefreshCommitAttributions("amended"); err != nil {
		t.Fatalf("RefreshCommitAttributions: %v", err)
	}
	if got := lines("amended"); got != 5 {
		t.Errorf("the amended commit, attributed on its own: %d lines, want 5", got)
	}

	if _, err := db.RefreshCommitAttributions(""); err != nil {
		t.Fatalf("RefreshCommitAttributions (all): %v", err)
	}
	if o, a := lines("original"), lines("amended"); o != 5 || a != 0 {
		t.Errorf("whole-database pass: original %d, amended %d lines, want 5 and 0", o, a)
	}
}

// TestGiveUpAttributionBringsSessionTotalsInLine covers a retirement that was
// cut short: the file rows are at zero already, the per-session total is not.
// The next call has no file row left to change and still has to settle it.
func TestGiveUpAttributionBringsSessionTotalsInLine(t *testing.T) {
	db := newTestDB(t)
	mustExec(t, db, `INSERT INTO repositories (id, name, root_path) VALUES ('repo1', 'r', '/nonexistent/path')`)
	mustExec(t, db, `INSERT INTO sessions (id, agent_slug, repo_id, started_at)
		VALUES ('sess1', 'claude-code', 'repo1', '2024-01-01T10:00:00Z')`)
	for _, id := range []string{"retired", "replacement"} {
		mustExec(t, db, `INSERT INTO commits (id, repo_id, sha, committed_at, lines_added) VALUES (?, 'repo1', ?, '2024-01-01T11:00:00Z', 5)`, id, id)
	}
	row := func(table, commitID string, lines int) {
		cols, vals := "commit_id, session_id, first_event_ts, last_event_ts, lines_attributed", "?, 'sess1', 't', 't', ?"
		if table == "commit_session_files" {
			cols, vals = cols+", file_path", vals+", 'f.txt'"
		} else {
			cols, vals = cols+", agent_slug, files_touched", vals+", 'claude-code', 1"
		}
		mustExec(t, db, `INSERT INTO `+table+` (`+cols+`) VALUES (`+vals+`)`, commitID, lines)
	}
	row("commit_session_files", "replacement", 5)
	row("commit_sessions", "replacement", 5)
	row("commit_session_files", "retired", 0) // already given up
	row("commit_sessions", "retired", 5)      // left behind
	mustExec(t, db, `UPDATE commit_sessions SET synced_at = 'synced' WHERE commit_id = 'retired'`)

	changed, err := db.GiveUpAttribution("retired", "replacement")
	if err != nil {
		t.Fatalf("GiveUpAttribution: %v", err)
	}
	if !changed {
		t.Errorf("GiveUpAttribution reported no change")
	}
	var lines, files int
	var synced sql.NullString
	mustQueryRow(t, db, `SELECT lines_attributed, files_touched, synced_at FROM commit_sessions WHERE commit_id = 'retired'`, nil, &lines, &files, &synced)
	if lines != 0 || files != 0 {
		t.Errorf("retired commit's session total: %d lines in %d files, want 0 in 0", lines, files)
	}
	if synced.Valid {
		t.Errorf("the corrected row is not marked for sync")
	}
	mustQueryRow(t, db, `SELECT lines_attributed FROM commit_sessions WHERE commit_id = 'replacement'`, nil, &lines)
	if lines != 5 {
		t.Errorf("replacement's session total changed to %d", lines)
	}
	// Nothing left to do: a further call changes nothing.
	if changed, err := db.GiveUpAttribution("retired", "replacement"); err != nil || changed {
		t.Errorf("a second call: changed %v, err %v", changed, err)
	}
}
