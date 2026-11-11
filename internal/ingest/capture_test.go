package ingest

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/bilanc/posthook/internal/spool"
	"github.com/bilanc/posthook/internal/store"
)

// deliverAt ingests a payload whose hook fired at firedAt. The worker handles
// events after the fact, so a test that needs two events to have queued
// (both fired before either is handled) dates them in the past.
func deliverAt(t *testing.T, agent, cwd string, payload map[string]any, firedAt time.Time) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := ProcessAgentEnvelope(spool.Envelope{
		Agent: agent, Cwd: cwd, ReceivedAt: firedAt.UTC().Format(time.RFC3339Nano), Payload: raw,
	}); err != nil {
		t.Fatalf("ProcessAgentEnvelope: %v", err)
	}
}

// End-to-end checks of exact edit capture: a hook payload goes through
// ProcessAgentEnvelope against a real git repository, and the test reads back
// what was stored. Each scenario is one of the over-counts a customer
// measured line by line against `git diff`.

type eventCounts struct {
	generated sql.NullInt64
	reworked  sql.NullInt64
}

// countsForSession returns lines_generated / lines_reworked for the session's
// events in the order they were ingested.
func countsForSession(t *testing.T, db *store.DB, sessionID string) []eventCounts {
	t.Helper()
	rows, err := db.Query(`SELECT lines_generated, lines_reworked FROM events WHERE session_id = ? ORDER BY rowid`, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []eventCounts
	for rows.Next() {
		var c eventCounts
		if err := rows.Scan(&c.generated, &c.reworked); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

// fileLinesForSession returns the decoded events.file_lines of the session's
// events in ingest order; nil where the column is NULL.
func fileLinesForSession(t *testing.T, db *store.DB, sessionID string) [][]fileLines {
	t.Helper()
	rows, err := db.Query(`SELECT file_lines FROM events WHERE session_id = ? ORDER BY rowid`, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out [][]fileLines
	for rows.Next() {
		var raw sql.NullString
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if !raw.Valid {
			out = append(out, nil)
			continue
		}
		var files []fileLines
		if err := json.Unmarshal([]byte(raw.String), &files); err != nil {
			t.Fatalf("file_lines is not JSON: %v: %s", err, raw.String)
		}
		out = append(out, files)
	}
	return out
}

// wantCounts asserts the session's events carry exactly these
// (generated, reworked) pairs.
func wantCounts(t *testing.T, db *store.DB, sessionID string, want ...[2]int64) {
	t.Helper()
	got := countsForSession(t, db, sessionID)
	if len(got) != len(want) {
		t.Fatalf("session %s: %d events, want %d (%+v)", sessionID, len(got), len(want), got)
	}
	for i, w := range want {
		if !got[i].generated.Valid || !got[i].reworked.Valid || got[i].generated.Int64 != w[0] || got[i].reworked.Int64 != w[1] {
			t.Errorf("session %s event %d: generated %v reworked %v, want %d / %d",
				sessionID, i, got[i].generated, got[i].reworked, w[0], w[1])
		}
	}
}

func wantRanges(t *testing.T, db *store.DB, sessionID string, want ...rangeRow) {
	t.Helper()
	got := rangesForSession(t, db, sessionID)
	if len(got) == 0 && len(want) == 0 {
		return
	}
	// rangesForSession orders by start line only; settle ties by end line.
	for _, rows := range [][]rangeRow{got, want} {
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].start != rows[j].start {
				return rows[i].start < rows[j].start
			}
			return rows[i].end < rows[j].end
		})
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("session %s: line ranges %+v, want %+v", sessionID, got, want)
	}
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func numbered(n int, format string) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, format+"\n", i)
	}
	return b.String()
}

// claudeEdit is a Claude Code PostToolUse payload for Edit or Write. hunks is
// the tool_response.structuredPatch; extra is merged into tool_response.
func claudeEdit(session, tool, path string, input map[string]any, hunks []map[string]any, extra map[string]any) map[string]any {
	input["file_path"] = path
	patch := make([]any, len(hunks))
	for i, h := range hunks {
		patch[i] = h
	}
	resp := map[string]any{"filePath": path, "structuredPatch": patch}
	for k, v := range extra {
		resp[k] = v
	}
	return map[string]any{
		"hook_event_name": "PostToolUse", "session_id": session,
		"tool_name": tool, "tool_input": input, "tool_response": resp,
	}
}

func hunk(newStart int, lines ...string) map[string]any {
	raw := make([]any, len(lines))
	for i, l := range lines {
		raw[i] = l
	}
	return map[string]any{"oldStart": float64(newStart), "newStart": float64(newStart), "lines": raw}
}

func TestExactCapture(t *testing.T) {
	// store.Open caches one handle per process; drop any an earlier test
	// left so this one gets its own isolated database, and leave none behind.
	_ = store.Close()
	home := isolateIngest(t)
	t.Cleanup(func() { _ = store.Close() })

	t.Run("claude edit counts the patch, not the repeated context", func(t *testing.T) { testClaudeEditPatch(t, home) })
	t.Run("claude write: new file whole, rewrite only what changed", func(t *testing.T) { testClaudeWrite(t, home) })
	t.Run("rework is the session's own removed lines", func(t *testing.T) { testRework(t, home) })
	t.Run("rework: removing someone else's copy of a line is not rework", func(t *testing.T) { testReworkLeavesSomeoneElsesCopyAlone(t, home) })
	t.Run("rework without the previous content in the payload", func(t *testing.T) { testReworkWithoutPreviousContent(t, home) })
	t.Run("queued shell rewrites each keep their own lines", func(t *testing.T) { testQueuedShellRewrites(t, home) })
	t.Run("queued edit then write: the write is not diffed against a later disk", func(t *testing.T) { testQueuedEditThenWrite(t, home) })
	t.Run("a file too large to snapshot keeps none", func(t *testing.T) { testOversizeFileKeepsNoSnapshot(t, home) })
	t.Run("a shell rewrite found lower in the file points there", func(t *testing.T) { testShellRewriteFoundLowerInTheFile(t, home) })
	t.Run("a shell rewrite with a line changed since keeps the other lines' positions", func(t *testing.T) { testShellRewriteWithALineChangedSince(t, home) })
	t.Run("a patch-less write found lower in the file points there", func(t *testing.T) { testWriteWithoutPatchFoundLowerInTheFile(t, home) })
	t.Run("a shell rewrite is not moved onto a later copy of its text", func(t *testing.T) { testShellRewriteNotMovedOntoALaterCopy(t, home) })
	t.Run("a shell event whose row names another file records the split", func(t *testing.T) { testShellSplitWhenRowNamesAnotherFile(t, home) })
	t.Run("write without a patch diffs against HEAD, then the last copy", func(t *testing.T) { testWriteWithoutPatch(t, home) })
	t.Run("files outside a repo or ignored count for nothing", func(t *testing.T) { testUncountableFiles(t, home) })
	t.Run("shell heredoc rewrite counts only the changed line", func(t *testing.T) { testShellRewrite(t, home) })
	t.Run("apply_patch: per-file ranges, out-of-repo file left out", func(t *testing.T) { testApplyPatchFiles(t, home) })
	t.Run("a patch across files records each file's lines and rework", func(t *testing.T) { testFileLinesSplitReworkByFile(t, home) })
	t.Run("an edit that can no longer be placed still counts", func(t *testing.T) { testUnlocatedEdit(t, home) })
	t.Run("cursor afterFileEdit diffs its old and new strings", func(t *testing.T) { testCursorAfterFileEdit(t, home) })
	t.Run("non-edit events carry no counts", func(t *testing.T) { testNonEditEvent(t, home) })
	t.Run("commit: a rewritten new file is attributed once, the human line is not", func(t *testing.T) { testCommitAfterRewrite(t, home) })
	t.Run("commit: a human change to context the agent repeated stays human", func(t *testing.T) { testCommitKeepsHumanContextLine(t, home) })
}

// commitAndAttribute commits everything in repo, runs the post-commit ingest
// and returns the commit's added lines and the lines attributed to session.
func commitAndAttribute(t *testing.T, repo, session string) (added, attributed int) {
	t.Helper()
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "work")
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.TrimSpace(string(out))
	if err := GitCommit(repo, sha); err != nil {
		t.Fatalf("GitCommit: %v", err)
	}
	db := openDB(t)
	if err := db.QueryRow(`SELECT lines_added FROM commits WHERE sha = ?`, sha).Scan(&added); err != nil {
		t.Fatalf("commit row: %v", err)
	}
	var n sql.NullInt64
	if err := db.QueryRow(`SELECT SUM(cs.lines_attributed) FROM commit_sessions cs JOIN commits c ON c.id = cs.commit_id WHERE c.sha = ? AND cs.session_id = ?`, sha, session).Scan(&n); err != nil {
		t.Fatalf("attribution row: %v", err)
	}
	return added, int(n.Int64)
}

func testCommitAfterRewrite(t *testing.T, home string) {
	repo := filepath.Join(home, "repo-commit-rewrite")
	initRepo(t, repo)
	writeFile(t, filepath.Join(repo, "README.md"), "# demo\n")
	gitIn(t, repo, "add", ".")
	gitIn(t, repo, "commit", "-q", "-m", "init")

	path := filepath.Join(repo, "f.txt")
	v1 := numbered(6, "line %d")
	v2 := strings.Replace(v1, "line 2\n", "line two\n", 1) + "line 7\nline 8\n"

	writeFile(t, path, v1)
	deliver(t, "claude-code", repo, claudeEdit("att-1", "Write", path,
		map[string]any{"content": v1}, nil, map[string]any{"type": "create", "originalFile": nil}))
	writeFile(t, path, v2)
	deliver(t, "claude-code", repo, claudeEdit("att-1", "Write", path,
		map[string]any{"content": v2},
		[]map[string]any{
			hunk(1, " line 1", "-line 2", "+line two", " line 3"),
			hunk(5, " line 5", " line 6", "+line 7", "+line 8"),
		},
		map[string]any{"type": "update", "originalFile": v1}))
	// The engineer adds a ninth line by hand before committing.
	writeFile(t, path, v2+"by hand\n")

	db := openDB(t)
	// Generated: 6, then 3 written of which 1 replaced the session's own
	// line. Net 8, which is what stands in the file.
	wantCounts(t, db, "att-1", [2]int64{6, 0}, [2]int64{3, 1})
	added, attributed := commitAndAttribute(t, repo, "att-1")
	if added != 9 || attributed != 8 {
		t.Errorf("commit added %d, attributed %d; want 9 and 8", added, attributed)
	}
}

func testCommitKeepsHumanContextLine(t *testing.T, home string) {
	repo := filepath.Join(home, "repo-commit-context")
	initRepo(t, repo)
	path := filepath.Join(repo, "cfg.ini")
	writeFile(t, path, numbered(10, "k%d = old"))
	gitIn(t, repo, "add", ".")
	gitIn(t, repo, "commit", "-q", "-m", "init")

	// An agent without a patch in its payload: it repeats lines 3-5 as
	// context, changes line 5 and inserts a line after it.
	oldString := "k3 = old\nk4 = old\nk5 = old\n"
	newString := "k3 = old\nk4 = old\nk5 = new\nk5b = new\n"
	after := strings.Replace(numbered(10, "k%d = old"), oldString, newString, 1)
	writeFile(t, path, after)
	deliver(t, "devin", repo, map[string]any{
		"hook_event_name": "PostToolUse", "session_id": "att-2", "cwd": repo,
		"tool_name": "edit", "tool_input": map[string]any{"file_path": path, "old_string": oldString, "new_string": newString},
	})
	// The engineer then changes line 4, one of the lines the agent repeated.
	writeFile(t, path, strings.Replace(after, "k4 = old\n", "k4 = by hand\n", 1))

	db := openDB(t)
	wantCounts(t, db, "att-2", [2]int64{2, 0})
	wantRanges(t, db, "att-2", rangeRow{5, 6})
	// The commit adds three lines: 4 (human), 5 and 6 (agent). When the
	// range covered the whole new string (3-6), line 4 went to the agent.
	added, attributed := commitAndAttribute(t, repo, "att-2")
	if added != 3 || attributed != 2 {
		t.Errorf("commit added %d, attributed %d; want 3 and 2", added, attributed)
	}
}

func testClaudeEditPatch(t *testing.T, home string) {
	repo := filepath.Join(home, "repo-edit")
	initRepo(t, repo)
	path := filepath.Join(repo, "a.go")
	oldString := "func a() {\n\treturn 1\n}"
	newString := "func a() {\n\t// added by agent\n\treturn 1\n}"
	writeFile(t, path, "package a\n\n"+newString+"\n\nfunc b() {}\n")

	deliver(t, "claude-code", repo, claudeEdit("edit-1", "Edit", path,
		map[string]any{"old_string": oldString, "new_string": newString},
		[]map[string]any{hunk(3, " func a() {", "+\t// added by agent", " \treturn 1", " }")},
		map[string]any{"originalFile": "package a\n\n" + oldString + "\n\nfunc b() {}\n"}))

	db := openDB(t)
	// The new string is four lines and sits at 3-6; the edit added line 4.
	wantCounts(t, db, "edit-1", [2]int64{1, 0})
	wantRanges(t, db, "edit-1", rangeRow{4, 4})
}

func testClaudeWrite(t *testing.T, home string) {
	repo := filepath.Join(home, "repo-write")
	initRepo(t, repo)
	path := filepath.Join(repo, "tables.json")
	first := numbered(26, `  "table_%d": true,`)
	second := first + "  \"new_a\": true,\n  \"new_b\": true\n"

	// A new file: an empty patch and no original.
	writeFile(t, path, first)
	deliver(t, "claude-code", repo, claudeEdit("write-1", "Write", path,
		map[string]any{"content": first}, nil,
		map[string]any{"type": "create", "originalFile": nil}))

	// The same file written whole again with two lines appended. The old
	// extractor scored this 28.
	writeFile(t, path, second)
	deliver(t, "claude-code", repo, claudeEdit("write-1", "Write", path,
		map[string]any{"content": second},
		[]map[string]any{hunk(24, `   "table_24": true,`, `   "table_25": true,`, `   "table_26": true,`, `+  "new_a": true,`, `+  "new_b": true`)},
		map[string]any{"type": "update", "originalFile": first}))

	// And once more with nothing changed.
	deliver(t, "claude-code", repo, claudeEdit("write-1", "Write", path,
		map[string]any{"content": second}, nil,
		map[string]any{"type": "update", "originalFile": second}))

	db := openDB(t)
	wantCounts(t, db, "write-1", [2]int64{26, 0}, [2]int64{2, 0}, [2]int64{0, 0})
	wantRanges(t, db, "write-1", rangeRow{1, 26}, rangeRow{27, 28})
}

func testRework(t *testing.T, home string) {
	repo := filepath.Join(home, "repo-rework")
	initRepo(t, repo)
	path := filepath.Join(repo, "svc.py")
	human := "import os\n"
	v1 := human + "def a():\n    return 1\n\ndef b():\n    return 2\n"
	v2 := strings.Replace(v1, "def a():\n    return 1\n", "def a(n):\n    return n\n", 1)
	v3 := strings.Replace(v2, "import os\n", "import sys\n", 1)
	v4 := strings.Replace(v3, "def b():\n    return 2\n", "def b(): return 2\n", 1)
	writeFile(t, path, human)
	gitIn(t, repo, "add", ".")
	gitIn(t, repo, "commit", "-q", "-m", "human start")

	// edit applies one Claude Code Edit: the file on disk becomes after, and
	// the payload carries the file as the tool read it.
	edit := func(session, before, after string, hunks ...map[string]any) {
		writeFile(t, path, after)
		deliver(t, "claude-code", repo, claudeEdit(session, "Edit", path,
			map[string]any{"old_string": "x", "new_string": "y"}, hunks,
			map[string]any{"originalFile": before}))
	}

	// The session appends five lines to a file a human started.
	edit("rework-1", human, v1, hunk(1, " import os", "+def a():", "+    return 1", "+", "+def b():", "+    return 2"))
	// It then rewrites two of its own lines: two generated, two reworked.
	edit("rework-1", v1, v2, hunk(2, "-def a():", "-    return 1", "+def a(n):", "+    return n"))
	// And changes the human's line: one generated, nothing reworked.
	edit("rework-1", v2, v3, hunk(1, "-import os", "+import sys"))
	// Another session removing the first session's lines is not rework.
	edit("rework-2", v3, v4, hunk(5, "-def b():", "-    return 2", "+def b(): return 2"))

	db := openDB(t)
	wantCounts(t, db, "rework-1", [2]int64{5, 0}, [2]int64{2, 2}, [2]int64{1, 0})
	wantCounts(t, db, "rework-2", [2]int64{1, 0})
}

func testReworkLeavesSomeoneElsesCopyAlone(t *testing.T, home string) {
	repo := filepath.Join(home, "repo-duplicate")
	initRepo(t, repo)
	path := filepath.Join(repo, "notes.txt")
	// A human file with a blank line at line 2.
	v0 := "a\n\nb\nc\nd\ne\nf\ng\nh\n"
	writeFile(t, path, v0)
	gitIn(t, repo, "add", ".")
	gitIn(t, repo, "commit", "-q", "-m", "human")

	edit := func(before, after string, hunks ...map[string]any) {
		writeFile(t, path, after)
		deliver(t, "claude-code", repo, claudeEdit("dup-1", "Edit", path,
			map[string]any{"old_string": "x", "new_string": "y"}, hunks,
			map[string]any{"originalFile": before}))
	}

	// The session adds a blank line of its own at line 10, and a line after.
	v1 := v0 + "\ntail\n"
	edit(v0, v1, hunk(9, " h", "+", "+tail"))
	// It deletes the human's blank line at line 2. Its own blank line is
	// still in the file, so nothing of its work was removed: the ledger
	// cannot tell the two blank lines apart, and must not guess.
	v2 := strings.Replace(v1, "a\n\nb", "a\nb", 1)
	edit(v1, v2, hunk(1, " a", "-", " b"))
	// Now it deletes its own blank line, the only one left. That is rework.
	v3 := strings.Replace(v2, "h\n\ntail", "h\ntail", 1)
	edit(v2, v3, hunk(8, " h", "-", " tail"))

	db := openDB(t)
	wantCounts(t, db, "dup-1", [2]int64{2, 0}, [2]int64{0, 0}, [2]int64{0, 1})
}

func testReworkWithoutPreviousContent(t *testing.T, home string) {
	// An agent whose payload carries neither a patch nor the file it read:
	// what the file held before an Edit is worked back from the file now.
	repo := filepath.Join(home, "repo-rework-plain")
	initRepo(t, repo)
	path := filepath.Join(repo, "vars.py")
	devin := func(tool string, input map[string]any, after string) {
		writeFile(t, path, after)
		input["file_path"] = path
		deliver(t, "devin", repo, map[string]any{
			"hook_event_name": "PostToolUse", "session_id": "plain-1", "cwd": repo,
			"tool_name": tool, "tool_input": input,
		})
	}
	devin("write", map[string]any{"content": "x = 1\ny = 2\n"}, "x = 1\ny = 2\n")
	devin("edit", map[string]any{"old_string": "y = 2", "new_string": "y = 3"}, "x = 1\ny = 3\n")

	db := openDB(t)
	wantCounts(t, db, "plain-1", [2]int64{2, 0}, [2]int64{1, 1})
}

func testQueuedShellRewrites(t *testing.T, home string) {
	// Two heredocs write "one" then "two" to an untracked file, and both
	// hooks fire before the worker handles either: the disk already holds
	// "two" while the first is ingested.
	repo := filepath.Join(home, "repo-queued-shell")
	initRepo(t, repo)
	path := filepath.Join(repo, "state.txt")
	writeFile(t, path, "two\n")
	fired := time.Now().Add(-2 * time.Second)
	bash := func(content string, at time.Time) {
		deliverAt(t, "claude-code", repo, map[string]any{
			"hook_event_name": "PostToolUse", "session_id": "queued-1", "cwd": repo,
			"tool_name": "Bash", "tool_input": map[string]any{"command": "cat > " + path + " <<'EOF'\n" + content + "EOF"},
		}, at)
	}
	bash("one\n", fired)
	bash("two\n", fired.Add(time.Second))

	db := openDB(t)
	// The first wrote one line that is gone again: counted, no position.
	// The second replaced it: one line written, the session's own line
	// reworked. Saving the disk as the first event's snapshot made the
	// second score zero.
	wantCounts(t, db, "queued-1", [2]int64{1, 0}, [2]int64{1, 1})
	wantRanges(t, db, "queued-1", rangeRow{1, 1})
}

func testQueuedEditThenWrite(t *testing.T, home string) {
	// An Edit and then a whole-file Write from an agent that reports no
	// patch, both fired before either is handled. The snapshot the Edit
	// leaves is read from a disk that already holds the Write.
	repo := filepath.Join(home, "repo-queued-write")
	initRepo(t, repo)
	path := filepath.Join(repo, "cfg.ini")
	committed := numbered(5, "k%d = old")
	writeFile(t, path, committed)
	gitIn(t, repo, "add", ".")
	gitIn(t, repo, "commit", "-q", "-m", "init")

	edited := strings.Replace(committed, "k2 = old", "k2 = new", 1)
	written := edited + "k6 = new\nk7 = new\n"
	writeFile(t, path, written)
	fired := time.Now().Add(-2 * time.Second)
	deliverAt(t, "devin", repo, map[string]any{
		"hook_event_name": "PostToolUse", "session_id": "queued-2", "cwd": repo,
		"tool_name": "edit", "tool_input": map[string]any{"file_path": path, "old_string": "k2 = old", "new_string": "k2 = new"},
	}, fired)
	deliverAt(t, "devin", repo, map[string]any{
		"hook_event_name": "PostToolUse", "session_id": "queued-2", "cwd": repo,
		"tool_name": "write", "tool_input": map[string]any{"file_path": path, "content": written},
	}, fired.Add(time.Second))

	db := openDB(t)
	// The Write must not be diffed against that snapshot (it would score
	// zero). It falls back to HEAD, so its two new lines count, and so does
	// line 2 a second time: over, never under, and only in this race.
	wantCounts(t, db, "queued-2", [2]int64{1, 0}, [2]int64{3, 0})
	wantRanges(t, db, "queued-2", rangeRow{2, 2}, rangeRow{2, 2}, rangeRow{6, 7})
}

func testShellRewriteFoundLowerInTheFile(t *testing.T, home string) {
	// A heredoc wrote the whole file; by the time the event is handled a
	// later edit has put two lines above it. Its lines are still there, two
	// lines lower, and that is where its ranges must point.
	repo := filepath.Join(home, "repo-shell-shifted")
	initRepo(t, repo)
	path := filepath.Join(repo, "list.txt")
	writeFile(t, path, "# header\n\nalpha\nbeta\n")
	deliver(t, "claude-code", repo, map[string]any{
		"hook_event_name": "PostToolUse", "session_id": "shifted-1", "cwd": repo,
		"tool_name": "Bash", "tool_input": map[string]any{"command": "cat > " + path + " <<'EOF'\nalpha\nbeta\nEOF"},
	})
	db := openDB(t)
	wantCounts(t, db, "shifted-1", [2]int64{2, 0})
	wantRanges(t, db, "shifted-1", rangeRow{3, 4})
}

func testShellRewriteWithALineChangedSince(t *testing.T, home string) {
	// A heredoc wrote three lines; a later edit changed the middle one in
	// place before the event is handled. That line has no position any
	// more, the other two keep theirs, and all three still count.
	repo := filepath.Join(home, "repo-shell-changed")
	initRepo(t, repo)
	path := filepath.Join(repo, "list.txt")
	writeFile(t, path, "one\nTWO\nthree\n")
	deliver(t, "claude-code", repo, map[string]any{
		"hook_event_name": "PostToolUse", "session_id": "changed-1", "cwd": repo,
		"tool_name": "Bash", "tool_input": map[string]any{"command": "cat > " + path + " <<'EOF'\none\ntwo\nthree\nEOF"},
	})
	db := openDB(t)
	wantCounts(t, db, "changed-1", [2]int64{3, 0})
	wantRanges(t, db, "changed-1", rangeRow{1, 1}, rangeRow{3, 3})
}

func testWriteWithoutPatchFoundLowerInTheFile(t *testing.T, home string) {
	// The same placement for a whole-file Write from an agent that reports
	// no patch: a later edit put a line above what it wrote.
	repo := filepath.Join(home, "repo-write-shifted")
	initRepo(t, repo)
	path := filepath.Join(repo, "list.txt")
	writeFile(t, path, "# header\nx\ny\n")
	deliver(t, "devin", repo, map[string]any{
		"hook_event_name": "PostToolUse", "session_id": "shifted-2", "cwd": repo,
		"tool_name": "write", "tool_input": map[string]any{"file_path": path, "content": "x\ny\n"},
	})
	db := openDB(t)
	wantCounts(t, db, "shifted-2", [2]int64{2, 0})
	wantRanges(t, db, "shifted-2", rangeRow{2, 3})
}

func testShellRewriteNotMovedOntoALaterCopy(t *testing.T, home string) {
	// HEAD holds "old, bar". A heredoc writes "foo, bar": line 1 changed.
	// Before the event is handled, someone changes line 2 and appends an
	// intact copy of the old text, so the file reads "foo, baz, foo, bar".
	// The write's text is in the file verbatim from line 3, but its changed
	// line is still line 1, and that is where the range must stay.
	repo := filepath.Join(home, "repo-shell-duplicate")
	initRepo(t, repo)
	path := filepath.Join(repo, "list.txt")
	writeFile(t, path, "old\nbar\n")
	gitIn(t, repo, "add", ".")
	gitIn(t, repo, "commit", "-q", "-m", "init")
	writeFile(t, path, "foo\nbaz\nfoo\nbar\n")
	deliver(t, "claude-code", repo, map[string]any{
		"hook_event_name": "PostToolUse", "session_id": "copy-1", "cwd": repo,
		"tool_name": "Bash", "tool_input": map[string]any{"command": "cat > " + path + " <<'EOF'\nfoo\nbar\nEOF"},
	})
	db := openDB(t)
	wantCounts(t, db, "copy-1", [2]int64{1, 0})
	wantRanges(t, db, "copy-1", rangeRow{1, 1})
}

func testShellSplitWhenRowNamesAnotherFile(t *testing.T, home string) {
	// The command edits a.go in place (content unknown to posthook) and then
	// writes b.go. The event row names its first target, a.go; the line
	// posthook could measure went to b.go. One file was measured, but it is
	// not the row's, so the split is recorded and says so.
	repo := filepath.Join(home, "repo-shell-split")
	initRepo(t, repo)
	writeFile(t, filepath.Join(repo, "a.go"), "old\n")
	gitIn(t, repo, "add", ".")
	gitIn(t, repo, "commit", "-q", "-m", "init")
	writeFile(t, filepath.Join(repo, "a.go"), "new\n")
	writeFile(t, filepath.Join(repo, "b.go"), "hello\n")
	deliver(t, "claude-code", repo, map[string]any{
		"hook_event_name": "PostToolUse", "session_id": "rowfile-1", "cwd": repo,
		"tool_name": "Bash", "tool_input": map[string]any{"command": "sed -i 's/old/new/' a.go; echo hello > b.go"},
	})

	db := openDB(t)
	var rowFile sql.NullString
	if err := db.QueryRow(`SELECT rel_file_path FROM events WHERE session_id = 'rowfile-1'`).Scan(&rowFile); err != nil {
		t.Fatal(err)
	}
	if rowFile.String != "a.go" {
		t.Fatalf("test setup: the event row names %q, expected its first target a.go", rowFile.String)
	}
	wantCounts(t, db, "rowfile-1", [2]int64{1, 0})
	split := fileLinesForSession(t, db, "rowfile-1")
	if len(split) != 1 || len(split[0]) != 1 {
		t.Fatalf("file_lines = %+v, want one event with one file", split)
	}
	only := split[0][0]
	if only.RelFilePath == nil || *only.RelFilePath != "b.go" || only.LinesGenerated != 1 || only.LinesReworked != 0 {
		t.Errorf("split: %+v, want b.go with the one line", only)
	}
}

func testOversizeFileKeepsNoSnapshot(t *testing.T, home string) {
	repo := filepath.Join(home, "repo-big")
	initRepo(t, repo)
	path := filepath.Join(repo, "big.txt")
	content := numbered(40000, "line %d of a file that is larger than a snapshot is kept for")
	if len(content) <= store.MaxSnapshotBytes {
		t.Fatalf("test file is only %d bytes", len(content))
	}
	writeFile(t, path, content)
	deliver(t, "devin", repo, map[string]any{
		"hook_event_name": "PostToolUse", "session_id": "big-1", "cwd": repo,
		"tool_name": "write", "tool_input": map[string]any{"file_path": path, "content": content},
	})
	db := openDB(t)
	wantCounts(t, db, "big-1", [2]int64{40000, 0})
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM file_snapshots WHERE file_path LIKE '%big.txt'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("kept a snapshot of a %d-byte file", len(content))
	}
}

func testWriteWithoutPatch(t *testing.T, home string) {
	repo := filepath.Join(home, "repo-devin")
	initRepo(t, repo)
	tracked := filepath.Join(repo, "config.yml")
	committed := numbered(20, "key_%d: old")
	writeFile(t, tracked, committed)
	gitIn(t, repo, "add", ".")
	gitIn(t, repo, "commit", "-q", "-m", "config")

	devinWrite := func(path, content string) {
		writeFile(t, path, content)
		deliver(t, "devin", repo, map[string]any{
			"hook_event_name": "PostToolUse", "session_id": "devin-1", "cwd": repo,
			"tool_name": "write", "tool_input": map[string]any{"file_path": path, "content": content},
		})
	}

	// Devin's write carries the whole file and no patch. First rewrite of a
	// committed file: only the two lines that differ from HEAD.
	v2 := committed + "key_21: new\nkey_22: new\n"
	devinWrite(tracked, v2)
	// Second rewrite before any commit: HEAD is now three lines away and
	// posthook's last copy one, so the last copy is the baseline.
	v3 := strings.Replace(v2, "key_21: new", "key_21: newer", 1)
	devinWrite(tracked, v3)

	// A file that never existed: every line, then one changed line.
	fresh := filepath.Join(repo, "fresh.txt")
	devinWrite(fresh, numbered(10, "line %d"))
	devinWrite(fresh, strings.Replace(numbered(10, "line %d"), "line 4\n", "line four\n", 1))

	db := openDB(t)
	wantCounts(t, db, "devin-1",
		[2]int64{2, 0},  // key_21, key_22
		[2]int64{1, 1},  // key_21 again: its own line reworked
		[2]int64{10, 0}, // new file
		[2]int64{1, 1})  // line 4 again
	wantRanges(t, db, "devin-1", rangeRow{1, 10}, rangeRow{4, 4}, rangeRow{21, 21}, rangeRow{21, 22})
}

func testUncountableFiles(t *testing.T, home string) {
	repo := filepath.Join(home, "repo-ignored")
	initRepo(t, repo)
	writeFile(t, filepath.Join(repo, ".gitignore"), "build/\n")

	// A plan file under the home directory, outside every repository.
	plan := filepath.Join(home, "plans", "plan.md")
	writeFile(t, plan, numbered(40, "step %d"))
	deliver(t, "claude-code", repo, claudeEdit("outside-1", "Write", plan,
		map[string]any{"content": numbered(40, "step %d")}, nil,
		map[string]any{"type": "create", "originalFile": nil}))

	// A build artefact inside the repo that git ignores.
	artefact := filepath.Join(repo, "build", "out.js")
	writeFile(t, artefact, numbered(12, "var v%d;"))
	deliver(t, "claude-code", repo, claudeEdit("outside-1", "Write", artefact,
		map[string]any{"content": numbered(12, "var v%d;")}, nil,
		map[string]any{"type": "create", "originalFile": nil}))

	db := openDB(t)
	wantCounts(t, db, "outside-1", [2]int64{0, 0}, [2]int64{0, 0})
	wantRanges(t, db, "outside-1")
	var snapshots int
	if err := db.QueryRow(`SELECT COUNT(*) FROM file_snapshots WHERE file_path LIKE '%plan.md' OR file_path LIKE '%out.js'`).Scan(&snapshots); err != nil {
		t.Fatal(err)
	}
	if snapshots != 0 {
		t.Errorf("kept %d snapshot(s) of files that cannot be committed", snapshots)
	}
}

func testShellRewrite(t *testing.T, home string) {
	repo := filepath.Join(home, "repo-shell")
	initRepo(t, repo)
	path := filepath.Join(repo, "run.sh")
	bash := func(command string) {
		deliver(t, "claude-code", repo, map[string]any{
			"hook_event_name": "PostToolUse", "session_id": "shell-1", "cwd": repo,
			"tool_name": "Bash", "tool_input": map[string]any{"command": command},
		})
	}

	v1 := "#!/bin/sh\nset -e\necho one\necho two\n"
	writeFile(t, path, v1)
	bash("cat > " + path + " <<'EOF'\n" + v1 + "EOF")

	// The agent rewrites the whole script to change one line. The file is
	// still untracked, so only posthook's last copy can tell.
	v2 := "#!/bin/sh\nset -e\necho one\necho 2\n"
	writeFile(t, path, v2)
	bash("cat > " + path + " <<'EOF'\n" + v2 + "EOF")

	// An append is new wherever it lands.
	writeFile(t, path, v2+"echo three\n")
	bash("echo 'echo three' >> " + path)

	db := openDB(t)
	wantCounts(t, db, "shell-1", [2]int64{4, 0}, [2]int64{1, 1}, [2]int64{1, 0})
	wantRanges(t, db, "shell-1", rangeRow{1, 4}, rangeRow{4, 4}, rangeRow{5, 5})
}

func testApplyPatchFiles(t *testing.T, home string) {
	repo := filepath.Join(home, "repo-patch")
	initRepo(t, repo)
	inRepo := filepath.Join(repo, "Question.php")
	outside := filepath.Join(home, "scratch", "notes.md")
	writeFile(t, inRepo, "<?php\nclass Question\n{\n    public function getId()\n    {\n        return 1;\n    }\n\n    public function getTitle()\n    {\n        return 1;\n    }\n}\n")
	writeFile(t, outside, "one\ntwo\n")

	// Three sections for one file (the last two change single lines whose
	// text also appears earlier in the file) and one file outside the repo.
	patch := "*** Begin Patch\n" +
		"*** Update File: " + inRepo + "\n@@\n" +
		"+<?php\n+class Question\n+{\n+    public function getId()\n+    {\n+        return 1;\n" +
		"*** Update File: " + inRepo + "\n@@\n" +
		"     public function getTitle()\n-    (\n+    {\n" +
		"*** Update File: " + inRepo + "\n@@\n" +
		"-        return 2;\n+        return 1;\n" +
		"*** Add File: " + outside + "\n+one\n+two\n" +
		"*** End Patch\n"
	deliver(t, "codex", repo, map[string]any{
		"hook_event_name": "PostToolUse", "session_id": "patch-1", "cwd": repo,
		"tool_name": "apply_patch", "tool_input": map[string]any{"command": patch},
	})

	db := openDB(t)
	// Eight lines in the repo file; the two in the scratch file are left out.
	wantCounts(t, db, "patch-1", [2]int64{8, 0})
	wantRanges(t, db, "patch-1", rangeRow{1, 6}, rangeRow{10, 10}, rangeRow{11, 11})
	var linesAdded sql.NullInt64
	if err := db.QueryRow(`SELECT lines_added FROM events WHERE session_id = 'patch-1'`).Scan(&linesAdded); err != nil {
		t.Fatal(err)
	}
	if linesAdded.Int64 != 10 {
		t.Errorf("lines_added = %d, want the patch's own 10", linesAdded.Int64)
	}
	// Two files written, so the event says which got what: all eight lines
	// are the repo file's, and the scratch file is listed with nothing.
	split := fileLinesForSession(t, db, "patch-1")
	if len(split) != 1 || len(split[0]) != 2 {
		t.Fatalf("file_lines = %+v, want one event with two files", split)
	}
	first, second := split[0][0], split[0][1]
	if first.RelFilePath == nil || *first.RelFilePath != "Question.php" || first.LinesGenerated != 8 || first.LinesReworked != 0 {
		t.Errorf("repo file: %+v", first)
	}
	if second.RelFilePath != nil || !strings.HasSuffix(second.FilePath, "notes.md") || second.LinesGenerated != 0 || second.LinesReworked != 0 {
		t.Errorf("scratch file: %+v", second)
	}
}

func testFileLinesSplitReworkByFile(t *testing.T, home string) {
	repo := filepath.Join(home, "repo-file-lines")
	initRepo(t, repo)
	aPath := filepath.Join(repo, "a.go")
	bPath := filepath.Join(repo, "b.go")
	writeFile(t, aPath, "package a\n")
	gitIn(t, repo, "add", ".")
	gitIn(t, repo, "commit", "-q", "-m", "init")
	patch := func(body string) {
		deliver(t, "codex", repo, map[string]any{
			"hook_event_name": "PostToolUse", "session_id": "split-1", "cwd": repo,
			"tool_name": "apply_patch", "tool_input": map[string]any{"command": "*** Begin Patch\n" + body + "*** End Patch\n"},
		})
	}

	// The session writes b.go: one file, so no split is recorded.
	writeFile(t, bPath, "package b\n\nfunc B() {}\n")
	patch("*** Add File: " + bPath + "\n+package b\n+\n+func B() {}\n")

	// One patch then adds two lines to a.go and rewrites one of the
	// session's own lines in b.go. The event's totals are 3 generated and 1
	// reworked; only the split can say the rework was in b.go.
	writeFile(t, aPath, "package a\n\nfunc A() {}\n")
	writeFile(t, bPath, "package b\n\nfunc B() int { return 1 }\n")
	patch("*** Update File: " + aPath + "\n@@\n package a\n+\n+func A() {}\n" +
		"*** Update File: " + bPath + "\n@@\n-func B() {}\n+func B() int { return 1 }\n")

	db := openDB(t)
	wantCounts(t, db, "split-1", [2]int64{3, 0}, [2]int64{3, 1})
	split := fileLinesForSession(t, db, "split-1")
	if len(split) != 2 || split[0] != nil || len(split[1]) != 2 {
		t.Fatalf("file_lines = %+v, want NULL then two files", split)
	}
	a, b := split[1][0], split[1][1]
	if a.RelFilePath == nil || *a.RelFilePath != "a.go" || a.LinesGenerated != 2 || a.LinesReworked != 0 {
		t.Errorf("a.go: %+v", a)
	}
	if b.RelFilePath == nil || *b.RelFilePath != "b.go" || b.LinesGenerated != 1 || b.LinesReworked != 1 {
		t.Errorf("b.go: %+v", b)
	}
	if a.LinesGenerated+b.LinesGenerated != 3 || a.LinesReworked+b.LinesReworked != 1 {
		t.Errorf("the files do not add up to the event's 3 generated and 1 reworked: %+v %+v", a, b)
	}
}

func testUnlocatedEdit(t *testing.T, home string) {
	repo := filepath.Join(home, "repo-unlocated")
	initRepo(t, repo)
	path := filepath.Join(repo, "a.txt")
	// The file moved on before the worker got to the event.
	writeFile(t, path, "something else entirely\n")
	deliver(t, "devin", repo, map[string]any{
		"hook_event_name": "PostToolUse", "session_id": "lost-1", "cwd": repo,
		"tool_name": "edit", "tool_input": map[string]any{
			"file_path": path, "old_string": "a = 1\nb = 2", "new_string": "a = 1\nb = 3\nc = 4",
		},
	})
	db := openDB(t)
	wantCounts(t, db, "lost-1", [2]int64{2, 0})
	wantRanges(t, db, "lost-1")
}

func testCursorAfterFileEdit(t *testing.T, home string) {
	repo := filepath.Join(home, "repo-cursor")
	initRepo(t, repo)
	path := filepath.Join(repo, "app.ts")
	before := "const a = 1;\nconst b = 2;\nconst c = 3;\n"
	after := "const a = 1;\nconst b = 20;\nconst c = 3;\nconst d = 4;\n"
	writeFile(t, path, after)
	// Cursor hands over the whole old and new file as one edit.
	deliver(t, "cursor", repo, map[string]any{
		"hook_event_name": "afterFileEdit", "conversation_id": "c1", "session_id": "cursor-1",
		"cursor_version": "2.1.0", "file_path": path, "workspace_roots": []any{repo},
		"edits": []any{map[string]any{"old_string": before, "new_string": after}},
	})
	db := openDB(t)
	wantCounts(t, db, "cursor-1", [2]int64{2, 0})
	wantRanges(t, db, "cursor-1", rangeRow{2, 2}, rangeRow{4, 4})
}

func testNonEditEvent(t *testing.T, home string) {
	repo := filepath.Join(home, "repo-read")
	initRepo(t, repo)
	deliver(t, "claude-code", repo, map[string]any{
		"hook_event_name": "PostToolUse", "session_id": "read-1", "cwd": repo,
		"tool_name": "Read", "tool_input": map[string]any{"file_path": filepath.Join(repo, "x")},
	})
	got := countsForSession(t, openDB(t), "read-1")
	if len(got) != 1 || got[0].generated.Valid || got[0].reworked.Valid {
		t.Errorf("a Read should leave both counts NULL, got %+v", got)
	}
}
