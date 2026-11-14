package ingest

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bilanc/posthook/internal/gitx"
	"github.com/bilanc/posthook/internal/store"
)

// Content-based attribution: the cases that attribution by line number gets
// wrong. Each scenario builds a real git repository, plays the hook payloads
// an agent sends, changes files the way a person or a script does, commits,
// and compares the AI lines posthook attributes with the lines the agent
// wrote that are in the commit.

func TestContentAttribution(t *testing.T) {
	_ = store.Close()
	home := isolateIngest(t)
	t.Cleanup(func() { _ = store.Close() })
	openDB(t)

	t.Run("a line the engineer rewrites after the agent is not the agent's", func(t *testing.T) { testHumanRewritesAgentLines(t, home) })
	t.Run("a line the engineer deletes or adds is not the agent's", func(t *testing.T) { testHumanDeletesAndAdds(t, home) })
	t.Run("agent lines pushed down by a hand insert stay the agent's", func(t *testing.T) { testHandInsertAbove(t, home) })
	t.Run("agent lines pushed down by the agent's own later edit stay the agent's", func(t *testing.T) { testOwnLaterEditAbove(t, home) })
	t.Run("whitespace-only reformatting keeps the agent's lines", func(t *testing.T) { testWhitespaceOnlyReformat(t, home) })
	t.Run("partial staging: each commit gets the agent lines it holds", func(t *testing.T) { testPartialStaging(t, home) })
	t.Run("a rebase that moves the agent's lines keeps them", func(t *testing.T) { testRebaseMovesLines(t, home) })
	t.Run("a block the engineer moves inside the file stays the agent's", func(t *testing.T) { testHandMovedBlock(t, home) })
	t.Run("two agents editing one file are each credited their own lines", func(t *testing.T) { testTwoAgentsOneFile(t, home) })
	t.Run("an amend that keeps few of the original's lines still gets the ones it keeps", func(t *testing.T) { testHeavyAmend(t, home) })
	t.Run("a cherry-pick onto a branch where the lines sit elsewhere", func(t *testing.T) { testCherryPickElsewhere(t, home) })
	t.Run("attributing a commit again keeps the lines it was rightly credited", func(t *testing.T) { testReattributionKeepsCredit(t, home) })
	t.Run("a session that loses its lines in a commit gets a row at zero", func(t *testing.T) { testReattributionLeavesAZeroRow(t, home) })
	t.Run("upgrade: a file last edited before the map is matched by position", func(t *testing.T) { testFileFromBeforeTheMap(t, home) })
	t.Run("upgrade: the next edit carries the earlier edits into the map", func(t *testing.T) { testEditsInFlightAcrossTheUpgrade(t, home) })
	t.Run("upgrade: a rewrite of a commit from before the map is matched by position", func(t *testing.T) { testRewriteOfACommitFromBeforeTheMap(t, home) })
	t.Run("upgrade: a commit made before the map is matched by position", func(t *testing.T) { testCommitFromBeforeTheMap(t, home) })
}

// seededRepo makes a repository whose one committed file holds n lines.
func seededRepo(t *testing.T, home, name, file string, n int) (repo, path, content string) {
	t.Helper()
	repo = filepath.Join(home, name)
	initRepo(t, repo)
	path = filepath.Join(repo, file)
	content = numbered(n, "k%d = old")
	writeFile(t, path, content)
	gitIn(t, repo, "add", ".")
	gitIn(t, repo, "commit", "-q", "-m", "init")
	return repo, path, content
}

// recordHead records the commit at HEAD, as the git shadow does after
// `git commit`, and returns its sha.
func recordHead(t *testing.T, repo string) string {
	t.Helper()
	sha := gitOut(t, repo, "rev-parse", "HEAD")
	if err := GitCommit(repo, sha); err != nil {
		t.Fatalf("GitCommit: %v", err)
	}
	return sha
}

// insertPayload is a Claude Code Edit that inserts lines after line `after`
// of content (0 for the top of the file): the content it leaves and the
// PostToolUse payload it reports.
func insertPayload(session, path, content string, after int, added ...string) (next string, payload map[string]any) {
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	var patch []string
	start := after + 1
	oldString, newString := "", ""
	if after > 0 {
		patch = append(patch, " "+lines[after-1])
		start = after
		oldString = lines[after-1] + "\n"
		newString = oldString
	}
	for _, a := range added {
		patch = append(patch, "+"+a)
		newString += a + "\n"
	}
	if after < len(lines) {
		patch = append(patch, " "+lines[after])
		oldString += lines[after] + "\n"
		newString += lines[after] + "\n"
	}
	out := append(append(append([]string{}, lines[:after]...), added...), lines[after:]...)
	next = strings.Join(out, "\n") + "\n"
	return next, claudeEdit(session, "Edit", path,
		map[string]any{"old_string": oldString, "new_string": newString},
		[]map[string]any{hunk(start, patch...)}, map[string]any{"originalFile": content})
}

// agentInserts makes that edit on disk and delivers its event.
func agentInserts(t *testing.T, session, repo, path, content string, after int, added ...string) string {
	t.Helper()
	next, payload := insertPayload(session, path, content, after, added...)
	writeFile(t, path, next)
	deliver(t, "claude-code", repo, payload)
	return next
}

func wantAttributed(t *testing.T, added, attributed, wantAdded, wantAttributed int) {
	t.Helper()
	if added != wantAdded || attributed != wantAttributed {
		t.Errorf("commit added %d lines, %d attributed to the agent; want %d and %d", added, attributed, wantAdded, wantAttributed)
	}
}

func testHumanRewritesAgentLines(t *testing.T, home string) {
	repo, _, _ := seededRepo(t, home, "content-rewrite", "README.md", 1)
	v := numbered(10, "line %d")
	agentWrites(t, "ca-rewrite", repo, "f.txt", v)
	// The engineer rewrites two of the agent's ten lines before committing.
	byHand := strings.Replace(v, "line 3\n", "line three, by hand\n", 1)
	byHand = strings.Replace(byHand, "line 7\n", "line seven, by hand\n", 1)
	writeFile(t, filepath.Join(repo, "f.txt"), byHand)

	added, attributed := commitAndAttribute(t, repo, "ca-rewrite")
	wantAttributed(t, added, attributed, 10, 8)
}

func testHumanDeletesAndAdds(t *testing.T, home string) {
	repo, _, _ := seededRepo(t, home, "content-delete", "README.md", 1)
	v := numbered(6, "line %d")
	agentWrites(t, "ca-delete", repo, "f.txt", v)
	// The engineer removes the agent's second line and adds one at the end.
	writeFile(t, filepath.Join(repo, "f.txt"), strings.Replace(v, "line 2\n", "", 1)+"by hand\n")

	added, attributed := commitAndAttribute(t, repo, "ca-delete")
	wantAttributed(t, added, attributed, 6, 5)
}

func testHandInsertAbove(t *testing.T, home string) {
	repo, path, content := seededRepo(t, home, "content-insert-above", "cfg.ini", 10)
	after := agentInserts(t, "ca-above", repo, path, content, 5, "a1 = new", "a2 = new", "a3 = new")
	// The engineer adds two lines at the top; the agent's three move down.
	writeFile(t, path, "top1 = by hand\ntop2 = by hand\n"+after)

	added, attributed := commitAndAttribute(t, repo, "ca-above")
	wantAttributed(t, added, attributed, 5, 3)
}

func testOwnLaterEditAbove(t *testing.T, home string) {
	repo, path, content := seededRepo(t, home, "content-own-edit-above", "cfg.ini", 10)
	after := agentInserts(t, "ca-own", repo, path, content, 5, "a1 = new", "a2 = new", "a3 = new")
	agentInserts(t, "ca-own", repo, path, after, 0, "top1 = new", "top2 = new")

	added, attributed := commitAndAttribute(t, repo, "ca-own")
	wantAttributed(t, added, attributed, 5, 5)
}

func testWhitespaceOnlyReformat(t *testing.T, home string) {
	repo, _, _ := seededRepo(t, home, "content-reformat", "README.md", 1)
	v := "func a() int {\n\treturn 1\n}\n\nfunc b() int {\n\treturn 2\n}\n"
	agentWrites(t, "ca-format", repo, "f.go", v)
	// Format-on-save turns the tabs into spaces and nothing else.
	writeFile(t, filepath.Join(repo, "f.go"), strings.ReplaceAll(v, "\t", "    "))

	added, attributed := commitAndAttribute(t, repo, "ca-format")
	wantAttributed(t, added, attributed, 7, 7)
}

func testPartialStaging(t *testing.T, home string) {
	repo, path, content := seededRepo(t, home, "content-partial", "cfg.ini", 12)
	first := agentInserts(t, "ca-partial", repo, path, content, 3, "a = new")
	both := agentInserts(t, "ca-partial", repo, path, first, 10, "b = new")

	// The engineer stages the first change only (as `git add -p` would) and
	// commits it, then commits the rest.
	writeFile(t, path, first)
	gitIn(t, repo, "add", "cfg.ini")
	writeFile(t, path, both)
	gitIn(t, repo, "commit", "-q", "-m", "first change")
	one := recordHead(t, repo)
	laterSecond()
	two := commitAll(t, repo, "second change")
	if err := GitCommit(repo, two); err != nil {
		t.Fatalf("GitCommit: %v", err)
	}

	db := openDB(t)
	wantCommit(t, db, "the commit of the first change", one, 1)
	wantCommit(t, db, "the commit of the second change", two, 1)
}

func testRebaseMovesLines(t *testing.T, home string) {
	repo, _ := repoWithRemote(t, home, "content-rebase")
	path := filepath.Join(repo, "cfg.ini")
	content := numbered(10, "k%d = old")
	writeFile(t, path, content)
	commitAll(t, repo, "add cfg")
	gitOut(t, repo, "push", "-q", "origin", "main")

	gitOut(t, repo, "checkout", "-q", "-b", "feature")
	agentInserts(t, "ca-rebase", repo, path, content, 8, "a1 = new", "a2 = new", "a3 = new")
	original := commitAll(t, repo, "agent change")
	if err := GitCommit(repo, original); err != nil {
		t.Fatal(err)
	}
	db := openDB(t)
	wantCommit(t, db, "the commit as first made", original, 3)

	// main gains two lines at the top of the same file, and the branch is
	// rebased onto it: the agent's lines are the same, two lines lower.
	gitOut(t, repo, "checkout", "-q", "main")
	writeFile(t, path, "top1 = by hand\ntop2 = by hand\n"+content)
	commitAll(t, repo, "main moves")
	gitOut(t, repo, "checkout", "-q", "feature")
	gitOut(t, repo, "rebase", "-q", "main")
	rebased := gitOut(t, repo, "rev-parse", "HEAD")
	if rebased == original {
		t.Fatal("test setup: the rebase did not rewrite the commit")
	}

	sweep(t, repo)
	wantCommit(t, db, "the rebased commit", rebased, 3)
	wantCommit(t, db, "the commit it replaced", original, 0)
}

func testHandMovedBlock(t *testing.T, home string) {
	repo, _, _ := seededRepo(t, home, "content-moved", "README.md", 1)
	header := "package a\n\n"
	first := "func first() int {\n\treturn compute(1)\n}\n"
	second := "\nfunc second() int {\n\treturn compute(2)\n}\n"
	agentWrites(t, "ca-moved", repo, "a.go", header+first+second)
	// The engineer swaps the two functions before committing.
	writeFile(t, filepath.Join(repo, "a.go"), header+strings.TrimPrefix(second, "\n")+"\n"+first)

	added, attributed := commitAndAttribute(t, repo, "ca-moved")
	// All nine lines are the agent's. The function that moved is found
	// again, its closing brace with it; the blank line between the two is
	// the one line that may not be.
	if added != 9 || attributed < 8 || attributed > 9 {
		t.Errorf("commit added %d lines, %d attributed to the agent; want 9 and at least 8", added, attributed)
	}
}

func testTwoAgentsOneFile(t *testing.T, home string) {
	repo, path, content := seededRepo(t, home, "content-two-agents", "cfg.ini", 10)
	// Two sessions edit the same file and both hooks fire before the worker
	// handles either: the disk holds both edits while the first is handled.
	fired := time.Now().Add(-3 * time.Second)
	afterOne, one := insertPayload("ca-agent-one", path, content, 5, "a1 = one", "a2 = one", "a3 = one")
	afterBoth, two := insertPayload("ca-agent-two", path, afterOne, 0, "top1 = two", "top2 = two")
	writeFile(t, path, afterBoth)
	deliverAt(t, "claude-code", repo, one, fired)
	deliverAt(t, "claude-code", repo, two, fired.Add(time.Second))

	sha := commitAll(t, repo, "both agents")
	if err := GitCommit(repo, sha); err != nil {
		t.Fatal(err)
	}
	db := openDB(t)
	for session, want := range map[string]int{"ca-agent-one": 3, "ca-agent-two": 2} {
		var got int
		if err := db.QueryRow(`
			SELECT COALESCE(SUM(cs.lines_attributed), 0) FROM commit_sessions cs
			JOIN commits c ON c.id = cs.commit_id WHERE c.sha = ? AND cs.session_id = ?`, sha, session).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("session %s: %d lines, want %d", session, got, want)
		}
	}
}

func testHeavyAmend(t *testing.T, home string) {
	repo, _ := repoWithRemote(t, home, "content-heavy-amend")
	path := filepath.Join(repo, "f.txt")
	v := numbered(10, "from the agent %d")
	agentWrites(t, "ca-amend", repo, "f.txt", v)
	original := commitAll(t, repo, "add f")
	if err := GitCommit(repo, original); err != nil {
		t.Fatal(err)
	}
	// The engineer rewrites seven of the ten lines by hand and amends. Less
	// than half of what the original added is left, so it is not recognised
	// as carried by what it adds; the three lines it keeps are still the
	// agent's.
	byHand := numbered(3, "from the agent %d") + numbered(7, "typed by hand %d")
	writeFile(t, path, byHand)
	gitOut(t, repo, "add", "-A")
	gitOut(t, repo, "commit", "-q", "--amend", "-m", "add f, mostly by hand")
	amended := recordHead(t, repo)

	db := openDB(t)
	wantCommit(t, db, "the amended commit", amended, 3)
}

func testCherryPickElsewhere(t *testing.T, home string) {
	repo, _ := repoWithRemote(t, home, "content-pick")
	path := filepath.Join(repo, "cfg.ini")
	content := numbered(10, "k%d = old")
	writeFile(t, path, content)
	commitAll(t, repo, "add cfg")
	gitOut(t, repo, "push", "-q", "origin", "main")

	gitOut(t, repo, "checkout", "-q", "-b", "feature")
	agentInserts(t, "ca-pick", repo, path, content, 8, "a1 = new", "a2 = new", "a3 = new")
	original := commitAll(t, repo, "agent change")
	if err := GitCommit(repo, original); err != nil {
		t.Fatal(err)
	}

	// Another branch has two more lines at the top of the file. The commit
	// is cherry-picked there: same lines, two lines lower.
	gitOut(t, repo, "checkout", "-q", "main")
	gitOut(t, repo, "checkout", "-q", "-b", "release")
	writeFile(t, path, "top1 = by hand\ntop2 = by hand\n"+content)
	commitAll(t, repo, "release moves")
	gitOut(t, repo, "cherry-pick", original)
	picked := gitOut(t, repo, "rev-parse", "HEAD")

	sweep(t, repo)
	db := openDB(t)
	wantCommit(t, db, "the cherry-pick", picked, 3)
	// The original is still on its branch and keeps its lines.
	wantCommit(t, db, "the commit it was picked from", original, 3)
}

func testReattributionKeepsCredit(t *testing.T, home string) {
	repo, _ := repoWithRemote(t, home, "content-keep")
	path := filepath.Join(repo, "f.txt")
	v := numbered(10, "from the agent %d")
	agentWrites(t, "ca-keep", repo, "f.txt", v)
	first := commitAll(t, repo, "add f")
	if err := GitCommit(repo, first); err != nil {
		t.Fatal(err)
	}
	db := openDB(t)
	wantCommit(t, db, "the commit, when it is made", first, 10)

	// In a later turn the agent rewrites six of those lines. The file's map
	// now holds the new text; the six lines as committed are gone from it.
	laterSecond()
	v2 := numbered(6, "rewritten %d") + "from the agent 7\nfrom the agent 8\nfrom the agent 9\nfrom the agent 10\n"
	var patch []string
	for i := 1; i <= 6; i++ {
		patch = append(patch, "-"+strings.Split(v, "\n")[i-1])
	}
	for i := 1; i <= 6; i++ {
		patch = append(patch, "+"+strings.Split(v2, "\n")[i-1])
	}
	writeFile(t, path, v2)
	deliver(t, "claude-code", repo, claudeEdit("ca-keep-later", "Write", path,
		map[string]any{"content": v2}, []map[string]any{hunk(1, patch...)},
		map[string]any{"type": "update", "originalFile": v}))

	// A commit made by a git posthook did not see is recorded at the next
	// sweep, which looks at every commit not yet pushed again.
	gitOut(t, repo, "commit", "-q", "--allow-empty", "-m", "made elsewhere")
	sweep(t, repo)

	// The first commit holds what it held: ten lines the agent wrote.
	wantCommit(t, db, "the commit, attributed again after the agent moved on", first, 10)
	if !hasNote(repo, first) {
		t.Error("the commit lost its note")
	}
}

func testReattributionLeavesAZeroRow(t *testing.T, home string) {
	repo, _, _ := seededRepo(t, home, "content-zero-row", "README.md", 1)
	agentWrites(t, "ca-zero", repo, "f.txt", numbered(3, "line %d"))
	sha := commitAll(t, repo, "add f")
	if err := GitCommit(repo, sha); err != nil {
		t.Fatal(err)
	}
	db := openDB(t)
	wantCommit(t, db, "the commit", sha, 3)

	// Nothing supports the credit any more: simulated by forgetting the
	// file's map, the edit's ranges and the commit's own record of its
	// lines. (Losing the map alone changes nothing; the record stands.)
	path := gitx.Canonicalize(filepath.Join(repo, "f.txt"))
	if _, err := db.Exec(`DELETE FROM file_snapshots WHERE file_path = ?`, path); err != nil {
		t.Fatal(err)
	}
	if err := GitCommit(repo, sha); err != nil {
		t.Fatal(err)
	}
	wantCommit(t, db, "the commit, attributed again without the file's map", sha, 3)
	if _, err := db.Exec(`DELETE FROM event_line_ranges WHERE file_path = ?`, path); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM commit_ai_lines WHERE commit_id = (SELECT id FROM commits WHERE sha = ?)`, sha); err != nil {
		t.Fatal(err)
	}
	if err := GitCommit(repo, sha); err != nil {
		t.Fatal(err)
	}
	var rows, lines int
	var synced any
	if err := db.QueryRow(`
		SELECT COUNT(*), COALESCE(SUM(cs.lines_attributed), 0), MAX(cs.synced_at) FROM commit_sessions cs
		JOIN commits c ON c.id = cs.commit_id WHERE c.sha = ? AND cs.session_id = 'ca-zero'`, sha).Scan(&rows, &lines, &synced); err != nil {
		t.Fatal(err)
	}
	// The cloud keeps the latest row per commit and session and has no
	// deletes: without a fresh row at zero it would keep showing three.
	if rows != 1 || lines != 0 || synced != nil {
		t.Errorf("after losing its lines: %d row(s), %d lines, synced_at %v; want one unsynced row at zero", rows, lines, synced)
	}
}

// forgetAuthorship leaves a file's snapshot as schema v16 wrote it: the
// fingerprints, and no authors.
func forgetAuthorship(t *testing.T, path string) {
	t.Helper()
	db := openDB(t)
	res, err := db.Exec(`UPDATE file_snapshots SET line_norms = NULL, line_owners = NULL, owners = NULL, blob_sha = NULL WHERE file_path = ?`, gitx.Canonicalize(path))
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("test setup: no snapshot row for %s", path)
	}
}

func confidenceOf(t *testing.T, sha, session string) string {
	t.Helper()
	var confidence string
	if err := openDB(t).QueryRow(`
		SELECT cs.confidence FROM commit_sessions cs JOIN commits c ON c.id = cs.commit_id
		WHERE c.sha = ? AND cs.session_id = ?`, sha, session).Scan(&confidence); err != nil {
		t.Fatalf("no attribution row for %s in %s: %v", session, sha[:7], err)
	}
	return confidence
}

func testFileFromBeforeTheMap(t *testing.T, home string) {
	repo, path, content := seededRepo(t, home, "content-before-map", "cfg.ini", 10)
	agentInserts(t, "ca-before", repo, path, content, 5, "a1 = new", "a2 = new", "a3 = new")
	// The edit was captured by the release before the authorship map.
	forgetAuthorship(t, path)

	sha := commitAll(t, repo, "agent change")
	if err := GitCommit(repo, sha); err != nil {
		t.Fatal(err)
	}
	wantCommit(t, openDB(t), "the commit", sha, 3)
	if got := confidenceOf(t, sha, "ca-before"); got != "line_range_committed_intersect" {
		t.Errorf("confidence %q, want the position match", got)
	}
}

func testEditsInFlightAcrossTheUpgrade(t *testing.T, home string) {
	repo, path, content := seededRepo(t, home, "content-in-flight", "cfg.ini", 10)
	after := agentInserts(t, "ca-flight", repo, path, content, 5, "a1 = new", "a2 = new", "a3 = new")
	// Upgrade happens here: the first edit left ranges and a plain snapshot.
	forgetAuthorship(t, path)
	// The next edit, of other lines, turns the snapshot into a map. The
	// first edit's lines must come with it.
	afterBoth := agentInserts(t, "ca-flight", repo, path, after, 12, "b1 = new", "b2 = new")
	// And the engineer then pushes everything down by one line.
	writeFile(t, path, "top = by hand\n"+afterBoth)

	sha := commitAll(t, repo, "both edits")
	if err := GitCommit(repo, sha); err != nil {
		t.Fatal(err)
	}
	wantCommit(t, openDB(t), "the commit", sha, 5)
	if got := confidenceOf(t, sha, "ca-flight"); got != "line_content_match" {
		t.Errorf("confidence %q, want the content match", got)
	}
}

func testRewriteOfACommitFromBeforeTheMap(t *testing.T, home string) {
	repo, _ := repoWithRemote(t, home, "content-old-original")
	agentWrites(t, "ca-old", repo, "f.txt", numbered(4, "f %d"))
	original := commitAll(t, repo, "add f")
	if err := GitCommit(repo, original); err != nil {
		t.Fatal(err)
	}
	db := openDB(t)
	// The original was recorded by the release before the map: no lines on
	// record, a plain snapshot.
	if _, err := db.Exec(`DELETE FROM commit_ai_lines`); err != nil {
		t.Fatal(err)
	}
	forgetAuthorship(t, filepath.Join(repo, "f.txt"))

	gitOut(t, repo, "commit", "-q", "--amend", "-m", "add f, reworded")
	amended := recordHead(t, repo)
	wantCommit(t, db, "the amended commit", amended, 4)
	wantCommit(t, db, "the commit it replaced", original, 0)
}

func testCommitFromBeforeTheMap(t *testing.T, home string) {
	repo, path, content := seededRepo(t, home, "content-old-commit", "cfg.ini", 10)
	agentInserts(t, "ca-old-commit", repo, path, content, 5, "a1 = new", "a2 = new", "a3 = new")
	sha := commitAll(t, repo, "agent change")

	// The commit was made before this database began to match by content,
	// and is only recorded now.
	db := openDB(t)
	if _, err := db.Exec(`UPDATE schema_version SET applied_at = datetime('now', '+1 hour')`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`UPDATE schema_version SET applied_at = datetime('now', '-1 hour')`)
	})
	if err := GitCommit(repo, sha); err != nil {
		t.Fatal(err)
	}
	wantCommit(t, db, "the commit", sha, 3)
	if got := confidenceOf(t, sha, "ca-old-commit"); got != "line_range_committed_intersect" {
		t.Errorf("confidence %q, want the position match", got)
	}
}
