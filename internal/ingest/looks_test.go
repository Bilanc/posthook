package ingest

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bilanc/posthook/internal/gitx"
	"github.com/bilanc/posthook/internal/spool"
	"github.com/bilanc/posthook/internal/store"
)

// Looks at the working tree: edits no hook payload describes.
//
// Each scenario plays what an agent session sends (a prompt, shell tool
// calls, edits), changes files the way the command in question does, and
// runs the looks the worker would run once it has caught up.

// settleLooks runs every queued look, as the worker does when its queue is
// empty and the looks have settled.
func settleLooks(t *testing.T) {
	t.Helper()
	if _, err := RunLooks(time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("RunLooks: %v", err)
	}
}

// promptSubmitted delivers a Claude Code prompt-submit event and runs its
// look: the start of a turn.
func promptSubmitted(t *testing.T, session, repo string) {
	t.Helper()
	deliver(t, "claude-code", repo, map[string]any{
		"hook_event_name": "UserPromptSubmit", "session_id": session, "cwd": repo, "prompt": "do the thing",
	})
	settleLooks(t)
}

func shellPayload(session, repo, command string) map[string]any {
	return map[string]any{
		"hook_event_name": "PostToolUse", "session_id": session, "cwd": repo, "tool_name": "Bash",
		"tool_input": map[string]any{"command": command},
	}
}

// shellRan delivers the event of a shell tool call that has just finished.
// The test has already done to the repository what the command does.
func shellRan(t *testing.T, session, repo, command string) {
	t.Helper()
	deliver(t, "claude-code", repo, shellPayload(session, repo, command))
}

const scriptCommand = "python3 - <<'EOF'\nimport pathlib\n# rewrites files in place\nEOF"

// generatedBy is the session's lines generated less the lines reworked.
func generatedBy(t *testing.T, session string) (generated, reworked int) {
	t.Helper()
	if err := openDB(t).QueryRow(`
		SELECT COALESCE(SUM(lines_generated), 0), COALESCE(SUM(lines_reworked), 0)
		FROM events WHERE session_id = ?`, session).Scan(&generated, &reworked); err != nil {
		t.Fatal(err)
	}
	return generated, reworked
}

func wantGenerated(t *testing.T, session string, want int) {
	t.Helper()
	if got, _ := generatedBy(t, session); got != want {
		t.Errorf("session %s generated %d lines, want %d", session, got, want)
	}
}

// lookRepo is a repository with a remote and one committed, pushed file of
// ten lines.
func lookRepo(t *testing.T, home, name string) (repo, remote, path, content string) {
	t.Helper()
	repo, remote = repoWithRemote(t, home, name)
	path = filepath.Join(repo, "cfg.ini")
	content = numbered(10, "k%d = old")
	writeFile(t, path, content)
	commitAll(t, repo, "add cfg")
	gitOut(t, repo, "push", "-q", "origin", "main")
	return repo, remote, path, content
}

// scripted rewrites the named keys of content, as a script would.
func scripted(content string, keys ...string) string {
	for _, k := range keys {
		content = strings.Replace(content, k+" = old\n", k+" = scripted\n", 1)
	}
	return content
}

func TestLooks(t *testing.T) {
	_ = store.Close()
	home := isolateIngest(t)
	t.Cleanup(func() { _ = store.Close() })
	openDB(t)

	t.Run("a script the agent runs from the shell is credited with what it changed", func(t *testing.T) { testScriptEdit(t, home) })
	t.Run("sed -i is credited with the lines it changed", func(t *testing.T) { testSedInPlace(t, home) })
	t.Run("files a script creates in a new directory", func(t *testing.T) { testScriptCreatesDirectory(t, home) })
	t.Run("a script that removes the session's own lines reworks them", func(t *testing.T) { testScriptReworks(t, home) })
	t.Run("a hand edit between turns is not the next shell command's", func(t *testing.T) { testHandEditBetweenTurns(t, home) })
	t.Run("the first look at a repository credits nothing", func(t *testing.T) { testFirstLookRecordsOnly(t, home) })
	t.Run("a formatter the agent runs does not take over lines it only re-indents", func(t *testing.T) { testFormatterRun(t, home) })
	t.Run("lines a command restores from git are not new", func(t *testing.T) { testRestoredLinesAreNotNew(t, home) })

	t.Run("a script and git commit in one command", func(t *testing.T) { testScriptAndCommitInOneCommand(t, home) })
	t.Run("a script, then git commit in the next command", func(t *testing.T) { testScriptThenCommit(t, home) })
	t.Run("a script and git commit --amend in one command", func(t *testing.T) { testScriptAndAmend(t, home) })
	t.Run("a commit made before its edits were handled is attributed once they are", func(t *testing.T) { testCommitBeforeEditsHandled(t, home) })

	t.Run("git pull brings a teammate's commit: nothing is generated", func(t *testing.T) { testAgentPulls(t, home) })
	t.Run("git merge of another branch", func(t *testing.T) { testAgentMerges(t, home) })
	t.Run("git rebase onto a moved base", func(t *testing.T) { testAgentRebases(t, home) })
	t.Run("git checkout of another branch and back", func(t *testing.T) { testAgentSwitchesBranch(t, home) })
	t.Run("git stash pop of hand-written changes", func(t *testing.T) { testStashPopOfHandWrittenChanges(t, home) })
	t.Run("git stash pop of the agent's own changes keeps them the agent's", func(t *testing.T) { testStashPopOfAgentChanges(t, home) })
	t.Run("git reset --hard and git checkout -- file", func(t *testing.T) { testAgentDiscardsChanges(t, home) })
	t.Run("git cherry-pick of a teammate's commit", func(t *testing.T) { testAgentCherryPicks(t, home) })
	t.Run("git revert", func(t *testing.T) { testAgentReverts(t, home) })

	t.Run("queued: a shell command, then an edit of another file", func(t *testing.T) { testQueuedShellThenEditElsewhere(t, home) })
	t.Run("queued: a shell command, then an edit of the same file", func(t *testing.T) { testQueuedShellThenEditSameFile(t, home) })
	t.Run("a prompt handled late leaves files changed since to the shell command", func(t *testing.T) { testPromptHandledLate(t, home) })
	t.Run("a prompt's text is not stored for an agent whose transcript has it", func(t *testing.T) { testPromptTextIsNotStored(t, home) })
	t.Run("git that does not answer in time", func(t *testing.T) { testGitDoesNotAnswer(t, home) })

	t.Run("the shape of the reported controlled test", func(t *testing.T) { testReportedShape(t, home) })
}

func testScriptEdit(t *testing.T, home string) {
	repo, _, path, content := lookRepo(t, home, "look-script")
	const session = "look-script"
	promptSubmitted(t, session, repo)
	// The agent rewrites three lines with a script fed to an interpreter. The
	// command names no file, so its text says nothing about what changed.
	writeFile(t, path, scripted(content, "k2", "k5", "k8"))
	shellRan(t, session, repo, scriptCommand)
	settleLooks(t)

	wantGenerated(t, session, 3)
	wantRanges(t, openDB(t), session, rangeRow{2, 2}, rangeRow{5, 5}, rangeRow{8, 8})
	// The event named no file; it takes the one the look found.
	var file sql.NullString
	if err := openDB(t).QueryRow(`SELECT rel_file_path FROM events WHERE session_id = ? AND event_type = 'PostToolUse'`, session).Scan(&file); err != nil || file.String != "cfg.ini" {
		t.Errorf("the shell event's file is %q (err %v), want cfg.ini", file.String, err)
	}
	added, attributed := commitAndAttribute(t, repo, session)
	wantAttributed(t, added, attributed, 3, 3)
}

func testSedInPlace(t *testing.T, home string) {
	repo, _, path, content := lookRepo(t, home, "look-sed")
	const session = "look-sed"
	promptSubmitted(t, session, repo)
	writeFile(t, path, strings.ReplaceAll(content, "k4 = old", "k4 = new"))
	shellRan(t, session, repo, "sed -i '' 's/k4 = old/k4 = new/' cfg.ini")
	settleLooks(t)

	wantGenerated(t, session, 1)
	added, attributed := commitAndAttribute(t, repo, session)
	wantAttributed(t, added, attributed, 1, 1)
}

func testScriptCreatesDirectory(t *testing.T, home string) {
	repo, _, _, _ := lookRepo(t, home, "look-newdir")
	const session = "look-newdir"
	promptSubmitted(t, session, repo)
	writeFile(t, filepath.Join(repo, "gen", "a.txt"), numbered(3, "a %d"))
	writeFile(t, filepath.Join(repo, "gen", "deep", "b.txt"), numbered(2, "b %d"))
	// What git ignores can never reach a commit and is not read.
	writeFile(t, filepath.Join(repo, ".gitignore"), "*.log\n")
	writeFile(t, filepath.Join(repo, "gen", "run.log"), numbered(50, "log %d"))
	shellRan(t, session, repo, "python3 tools/generate.py")
	settleLooks(t)

	// a.txt, b.txt and the .gitignore the command wrote.
	wantGenerated(t, session, 6)
	var split sql.NullString
	if err := openDB(t).QueryRow(`SELECT file_lines FROM events WHERE session_id = ? AND event_type = 'PostToolUse'`, session).Scan(&split); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"gen/a.txt", "gen/deep/b.txt", ".gitignore"} {
		if !strings.Contains(split.String, `"rel_file_path":"`+name+`"`) {
			t.Errorf("the per-file split does not name %s: %s", name, split.String)
		}
	}
	if strings.Contains(split.String, "run.log") {
		t.Errorf("an ignored file is in the split: %s", split.String)
	}
	added, attributed := commitAndAttribute(t, repo, session)
	wantAttributed(t, added, attributed, 6, 6)
}

func testScriptReworks(t *testing.T, home string) {
	repo, _, _, _ := lookRepo(t, home, "look-rework")
	const session = "look-rework"
	promptSubmitted(t, session, repo)
	v := numbered(5, "line %d")
	agentWrites(t, session, repo, "f.txt", v)
	// A script then drops two of the session's lines and adds one.
	next := strings.Replace(strings.Replace(v, "line 2\n", "", 1), "line 4\n", "", 1) + "line 6\n"
	writeFile(t, filepath.Join(repo, "f.txt"), next)
	shellRan(t, session, repo, scriptCommand)
	settleLooks(t)

	generated, reworked := generatedBy(t, session)
	if generated != 6 || reworked != 2 {
		t.Errorf("generated %d reworked %d, want 6 (5 written, then 1) and 2", generated, reworked)
	}
	added, attributed := commitAndAttribute(t, repo, session)
	wantAttributed(t, added, attributed, 4, 4)
}

func testHandEditBetweenTurns(t *testing.T, home string) {
	repo, _, path, content := lookRepo(t, home, "look-between")
	const session = "look-between"
	promptSubmitted(t, session, repo)
	shellRan(t, session, repo, "ls")
	settleLooks(t)
	// The turn is over. The engineer edits two lines by hand, then prompts
	// again; the agent's first command is a script that changes three more.
	byHand := strings.Replace(strings.Replace(content, "k1 = old", "k1 = by hand", 1), "k10 = old", "k10 = by hand", 1)
	writeFile(t, path, byHand)
	promptSubmitted(t, session, repo)
	writeFile(t, path, scripted(byHand, "k3", "k4", "k5"))
	shellRan(t, session, repo, scriptCommand)
	settleLooks(t)

	wantGenerated(t, session, 3)
	added, attributed := commitAndAttribute(t, repo, session)
	wantAttributed(t, added, attributed, 5, 3)
}

func testFirstLookRecordsOnly(t *testing.T, home string) {
	repo, _, path, content := lookRepo(t, home, "look-first")
	const session = "look-first"
	// No prompt was ever seen here (an agent with no prompt hook, or a
	// session that began before the upgrade). The file already has a
	// hand-written change when the agent's first script runs.
	byHand := strings.Replace(content, "k1 = old", "k1 = by hand", 1)
	writeFile(t, path, scripted(byHand, "k3"))
	shellRan(t, session, repo, scriptCommand)
	settleLooks(t)
	// Nothing says which of the two changed lines the script wrote.
	wantGenerated(t, session, 0)

	// From here on there is an earlier look to compare with.
	writeFile(t, path, scripted(byHand, "k3", "k7"))
	shellRan(t, session, repo, scriptCommand)
	settleLooks(t)
	wantGenerated(t, session, 1)
	added, attributed := commitAndAttribute(t, repo, session)
	wantAttributed(t, added, attributed, 3, 1)
}

func testFormatterRun(t *testing.T, home string) {
	repo, _ := repoWithRemote(t, home, "look-format")
	path := filepath.Join(repo, "a.go")
	byHand := "package a\n\nfunc a() int {\n\treturn 1\n}\n"
	writeFile(t, path, byHand)
	commitAll(t, repo, "add a, by hand")
	const session = "look-format"
	promptSubmitted(t, session, repo)
	// The agent runs a formatter that turns the tab into spaces and adds a
	// comment line: one line is new, the rest only changed whitespace.
	writeFile(t, path, "// Package a.\npackage a\n\nfunc a() int {\n    return 1\n}\n")
	shellRan(t, session, repo, "make fmt")
	settleLooks(t)

	wantGenerated(t, session, 1)
	added, attributed := commitAndAttribute(t, repo, session)
	wantAttributed(t, added, attributed, 2, 1)
}

func testRestoredLinesAreNotNew(t *testing.T, home string) {
	repo, _, path, content := lookRepo(t, home, "look-restore")
	const session = "look-restore"
	promptSubmitted(t, session, repo)
	// The agent removes three committed lines with an edit...
	trimmed := strings.Replace(content, "k3 = old\nk4 = old\nk5 = old\n", "", 1)
	writeFile(t, path, trimmed)
	deliver(t, "claude-code", repo, claudeEdit(session, "Edit", path,
		map[string]any{"old_string": "k3 = old\nk4 = old\nk5 = old\n", "new_string": ""},
		[]map[string]any{hunk(2, " k2 = old", "-k3 = old", "-k4 = old", "-k5 = old", " k6 = old")},
		map[string]any{"originalFile": content}))
	// ...and a command then puts the file back as git has it, plus one line.
	// Nothing in the command says it used git.
	writeFile(t, path, content+"extra = new\n")
	shellRan(t, session, repo, "make restore")
	settleLooks(t)

	// The three restored lines are in HEAD's copy of the file: not written
	// by anyone just now. Only the extra line is new.
	wantGenerated(t, session, 1)
	added, attributed := commitAndAttribute(t, repo, session)
	wantAttributed(t, added, attributed, 1, 1)
}

func testScriptAndCommitInOneCommand(t *testing.T, home string) {
	repo, _, path, content := lookRepo(t, home, "look-commit-inside")
	const session = "look-commit-inside"
	promptSubmitted(t, session, repo)
	// `python3 gen.py && git commit -am …`: the commit is made, and recorded
	// by the git shadow, before the tool call's hook fires.
	writeFile(t, path, scripted(content, "k2", "k5", "k8"))
	sha := commitAll(t, repo, "generated")
	if err := GitCommit(repo, sha); err != nil {
		t.Fatal(err)
	}
	db := openDB(t)
	wantCommit(t, db, "the commit, when it is made", sha, 0)

	shellRan(t, session, repo, "python3 gen.py && git add -A && git commit -q -m generated")
	settleLooks(t)
	wantGenerated(t, session, 3)
	wantCommit(t, db, "the commit, once the command's look has run", sha, 3)
}

func testScriptThenCommit(t *testing.T, home string) {
	repo, _, path, content := lookRepo(t, home, "look-commit-next")
	const session = "look-commit-next"
	promptSubmitted(t, session, repo)
	// Two tool calls in quick succession: the script, then the commit. Both
	// hooks have fired by the time the worker looks.
	writeFile(t, path, scripted(content, "k2", "k5", "k8"))
	shellRan(t, session, repo, scriptCommand)
	sha := commitAll(t, repo, "generated")
	if err := GitCommit(repo, sha); err != nil {
		t.Fatal(err)
	}
	shellRan(t, session, repo, "git add -A && git commit -q -m generated")
	settleLooks(t)

	wantGenerated(t, session, 3)
	wantCommit(t, openDB(t), "the commit", sha, 3)
}

func testScriptAndAmend(t *testing.T, home string) {
	repo, _, path, content := lookRepo(t, home, "look-amend")
	const session = "look-amend"
	promptSubmitted(t, session, repo)
	after := agentInserts(t, session, repo, path, content, 5, "a1 = new", "a2 = new")
	first := commitAll(t, repo, "agent change")
	if err := GitCommit(repo, first); err != nil {
		t.Fatal(err)
	}
	shellRan(t, session, repo, "git add -A && git commit -q -m 'agent change'")
	settleLooks(t)

	// `python3 fix.py && git commit --amend`: a script changes three more
	// lines and the commit is amended to hold them, in one command.
	writeFile(t, path, scripted(after, "k2", "k7", "k9"))
	gitOut(t, repo, "add", "-A")
	gitOut(t, repo, "commit", "-q", "--amend", "--no-edit")
	amended := recordHead(t, repo)
	shellRan(t, session, repo, "python3 fix.py && git add -A && git commit -q --amend --no-edit")
	settleLooks(t)

	wantGenerated(t, session, 5)
	db := openDB(t)
	wantCommit(t, db, "the amended commit", amended, 5)
	wantCommit(t, db, "the commit it replaced", first, 0)
}

func testCommitBeforeEditsHandled(t *testing.T, home string) {
	repo, path, content := seededRepo(t, home, "look-queued-commit", "cfg.ini", 10)
	// The agent edits and commits straight away. The hook fired, but the
	// worker has not handled the event when the commit is recorded.
	fired := time.Now().Add(-2 * time.Second)
	next, payload := insertPayload("look-queued-commit", path, content, 5, "a1 = new", "a2 = new", "a3 = new")
	writeFile(t, path, next)
	sha := commitAll(t, repo, "agent change")
	if err := GitCommit(repo, sha); err != nil {
		t.Fatal(err)
	}
	db := openDB(t)
	wantCommit(t, db, "the commit, when it is made", sha, 0)
	deliverAt(t, "claude-code", repo, payload, fired)
	wantCommit(t, db, "the commit, once its edit is handled", sha, 3)
	if !hasNote(repo, sha) {
		t.Error("the commit has no note after it was attributed again")
	}
}

// teammatePushes has a teammate, in a clone of their own, add two lines at
// the top of cfg.ini and push. It returns the pushed commit.
func teammatePushes(t *testing.T, home, remote, name string) string {
	t.Helper()
	mate := filepath.Join(home, name)
	gitOut(t, home, "clone", "-q", remote, mate)
	gitOut(t, mate, "config", "user.email", "mate@example.com")
	gitOut(t, mate, "config", "user.name", "Mate")
	data, err := os.ReadFile(filepath.Join(mate, "cfg.ini"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(mate, "cfg.ini"), "mate1 = theirs\nmate2 = theirs\n"+string(data))
	writeFile(t, filepath.Join(mate, "mate.txt"), numbered(4, "theirs %d"))
	gitOut(t, mate, "add", "-A")
	gitOut(t, mate, "commit", "-q", "-m", "teammate change")
	gitOut(t, mate, "push", "-q", "origin", "HEAD:main")
	return gitOut(t, mate, "rev-parse", "HEAD")
}

func testAgentPulls(t *testing.T, home string) {
	repo, remote, path, content := lookRepo(t, home, "look-pull")
	const session = "look-pull"
	promptSubmitted(t, session, repo)
	// The agent edits the file and the work is committed.
	agentInserts(t, session, repo, path, content, 8, "a1 = new", "a2 = new", "a3 = new")
	mine := commitAll(t, repo, "agent change")
	if err := GitCommit(repo, mine); err != nil {
		t.Fatal(err)
	}
	teammatePushes(t, home, remote, "look-pull-mate")

	// The agent pulls: the teammate's lines arrive in the same file.
	gitOut(t, repo, "pull", "-q", "--no-rebase", "origin", "main")
	shellRan(t, session, repo, "git pull origin main")
	settleLooks(t)
	wantGenerated(t, session, 3)

	// The same through a wrapper that never says git: the reflog shows HEAD
	// moved by something other than a commit.
	teammate2 := filepath.Join(home, "look-pull-mate")
	writeFile(t, filepath.Join(teammate2, "mate.txt"), numbered(6, "theirs %d"))
	gitOut(t, teammate2, "commit", "-q", "-am", "teammate again")
	gitOut(t, teammate2, "push", "-q", "origin", "HEAD:main")
	gitOut(t, repo, "pull", "-q", "--no-rebase", "origin", "main")
	shellRan(t, session, repo, "make sync")
	settleLooks(t)
	wantGenerated(t, session, 3)

	// A script the agent runs next is credited its own line, and not the
	// teammate's two that the file's map has never seen.
	data, _ := os.ReadFile(path)
	writeFile(t, path, strings.Replace(string(data), "k2 = old", "k2 = scripted", 1))
	shellRan(t, session, repo, scriptCommand)
	settleLooks(t)
	wantGenerated(t, session, 4)

	db := openDB(t)
	wantCommit(t, db, "the agent's commit", mine, 3)
	added, attributed := commitAndAttribute(t, repo, session)
	wantAttributed(t, added, attributed, 1, 1)
}

func testAgentMerges(t *testing.T, home string) {
	repo, _, path, content := lookRepo(t, home, "look-merge")
	const session = "look-merge"
	// Another branch, written by hand.
	gitOut(t, repo, "checkout", "-q", "-b", "other")
	writeFile(t, path, "other1 = by hand\nother2 = by hand\n"+content)
	writeFile(t, filepath.Join(repo, "other.txt"), numbered(5, "other %d"))
	commitAll(t, repo, "other branch")
	gitOut(t, repo, "checkout", "-q", "main")
	promptSubmitted(t, session, repo)

	gitOut(t, repo, "merge", "-q", "--no-ff", "-m", "merge other", "other")
	shellRan(t, session, repo, "git merge --no-ff other")
	settleLooks(t)
	wantGenerated(t, session, 0)

	// And when the command does not say git.
	gitOut(t, repo, "checkout", "-q", "other")
	writeFile(t, filepath.Join(repo, "other.txt"), numbered(8, "other %d"))
	commitAll(t, repo, "other branch again")
	gitOut(t, repo, "checkout", "-q", "main")
	settleAsPrompt(t, session, repo)
	gitOut(t, repo, "merge", "-q", "other")
	shellRan(t, session, repo, "./scripts/sync-branches.sh")
	settleLooks(t)
	wantGenerated(t, session, 0)
}

// settleAsPrompt records the repository as a new turn finds it.
func settleAsPrompt(t *testing.T, session, repo string) {
	t.Helper()
	promptSubmitted(t, session, repo)
}

func testAgentRebases(t *testing.T, home string) {
	repo, _, path, content := lookRepo(t, home, "look-rebase")
	const session = "look-rebase"
	gitOut(t, repo, "checkout", "-q", "-b", "feature")
	promptSubmitted(t, session, repo)
	agentInserts(t, session, repo, path, content, 8, "a1 = new", "a2 = new", "a3 = new")
	original := commitAll(t, repo, "agent change")
	if err := GitCommit(repo, original); err != nil {
		t.Fatal(err)
	}
	// main moves by hand, and the agent rebases the branch onto it.
	gitOut(t, repo, "checkout", "-q", "main")
	writeFile(t, path, "top1 = by hand\ntop2 = by hand\n"+content)
	writeFile(t, filepath.Join(repo, "main.txt"), numbered(6, "main %d"))
	commitAll(t, repo, "main moves")
	gitOut(t, repo, "checkout", "-q", "feature")
	settleAsPrompt(t, session, repo)
	gitOut(t, repo, "rebase", "-q", "main")
	rebased := gitOut(t, repo, "rev-parse", "HEAD")
	shellRan(t, session, repo, "git rebase main")
	settleLooks(t)

	// The rebase wrote nothing: the session generated its three lines, once.
	wantGenerated(t, session, 3)
	sweep(t, repo)
	db := openDB(t)
	wantCommit(t, db, "the rebased commit", rebased, 3)
	wantCommit(t, db, "the commit it replaced", original, 0)
}

func testAgentSwitchesBranch(t *testing.T, home string) {
	repo, _, path, content := lookRepo(t, home, "look-switch")
	const session = "look-switch"
	gitOut(t, repo, "checkout", "-q", "-b", "other")
	writeFile(t, path, "other1 = by hand\nother2 = by hand\n"+content)
	commitAll(t, repo, "other branch")
	gitOut(t, repo, "checkout", "-q", "main")
	promptSubmitted(t, session, repo)
	// The agent edits the file on main and the work is committed, so the
	// file has a map of its main version.
	agentInserts(t, session, repo, path, content, 8, "a1 = new")
	commitAll(t, repo, "agent change on main")

	gitOut(t, repo, "checkout", "-q", "other")
	shellRan(t, session, repo, "git checkout other")
	settleLooks(t)
	wantGenerated(t, session, 1)

	// A script on the other branch: its own line, and not the two lines the
	// branch has that the file's map, taken on main, never saw.
	data, _ := os.ReadFile(path)
	writeFile(t, path, strings.Replace(string(data), "k2 = old", "k2 = scripted", 1))
	shellRan(t, session, repo, scriptCommand)
	settleLooks(t)
	wantGenerated(t, session, 2)
	added, attributed := commitAndAttribute(t, repo, session)
	wantAttributed(t, added, attributed, 1, 1)

	gitOut(t, repo, "checkout", "-q", "main")
	shellRan(t, session, repo, "git switch main")
	settleLooks(t)
	wantGenerated(t, session, 2)
}

func testStashPopOfHandWrittenChanges(t *testing.T, home string) {
	repo, _, path, content := lookRepo(t, home, "look-stash-hand")
	const session = "look-stash-hand"
	promptSubmitted(t, session, repo)
	shellRan(t, session, repo, "ls")
	settleLooks(t)
	// Mid-turn the engineer writes two lines by hand; no look has seen them
	// when the agent stashes them away and brings them back.
	byHand := strings.Replace(content, "k1 = old", "k1 = by hand", 1) + "k11 = by hand\n"
	writeFile(t, path, byHand)
	gitOut(t, repo, "stash", "-q")
	gitOut(t, repo, "stash", "pop", "-q")
	shellRan(t, session, repo, "git stash && git pull --rebase && git stash pop")
	settleLooks(t)

	wantGenerated(t, session, 0)
	added, attributed := commitAndAttribute(t, repo, session)
	wantAttributed(t, added, attributed, 2, 0)
}

func testStashPopOfAgentChanges(t *testing.T, home string) {
	repo, _, path, content := lookRepo(t, home, "look-stash-agent")
	const session = "look-stash-agent"
	promptSubmitted(t, session, repo)
	agentInserts(t, session, repo, path, content, 5, "a1 = new", "a2 = new", "a3 = new")
	gitOut(t, repo, "stash", "-q")
	shellRan(t, session, repo, "git stash")
	settleLooks(t)
	gitOut(t, repo, "stash", "pop", "-q")
	shellRan(t, session, repo, "git stash pop")
	settleLooks(t)

	// Stashing and popping wrote nothing new, and the lines are still the
	// agent's when they are committed.
	wantGenerated(t, session, 3)
	added, attributed := commitAndAttribute(t, repo, session)
	wantAttributed(t, added, attributed, 3, 3)
}

func testAgentDiscardsChanges(t *testing.T, home string) {
	repo, _, path, content := lookRepo(t, home, "look-discard")
	const session = "look-discard"
	promptSubmitted(t, session, repo)
	after := agentInserts(t, session, repo, path, content, 5, "a1 = new", "a2 = new", "a3 = new")
	writeFile(t, filepath.Join(repo, "scratch.txt"), "tmp\n")
	gitOut(t, repo, "add", "-A")
	gitOut(t, repo, "reset", "-q", "--hard")
	shellRan(t, session, repo, "git reset --hard")
	settleLooks(t)
	wantGenerated(t, session, 3)

	// The agent writes its lines again, a script changes another line, and
	// `git checkout -- file` throws both away.
	writeFile(t, path, content)
	agentInserts(t, session, repo, path, content, 5, "a1 = new", "a2 = new", "a3 = new")
	writeFile(t, path, scripted(after, "k9"))
	gitOut(t, repo, "checkout", "--", "cfg.ini")
	shellRan(t, session, repo, "python3 tweak.py; git checkout -- cfg.ini")
	settleLooks(t)
	// Six lines generated by the two edits; the discarded script line never
	// counts, and nothing of git's does.
	wantGenerated(t, session, 6)

	writeFile(t, filepath.Join(repo, "notes.txt"), "by hand\n")
	added, attributed := commitAndAttribute(t, repo, session)
	wantAttributed(t, added, attributed, 1, 0)
}

func testAgentCherryPicks(t *testing.T, home string) {
	repo, remote, _, _ := lookRepo(t, home, "look-pick")
	const session = "look-pick"
	theirs := teammatePushes(t, home, remote, "look-pick-mate")
	gitOut(t, repo, "fetch", "-q", "origin")
	gitOut(t, repo, "checkout", "-q", "-b", "release")
	promptSubmitted(t, session, repo)

	gitOut(t, repo, "cherry-pick", theirs)
	picked := gitOut(t, repo, "rev-parse", "HEAD")
	shellRan(t, session, repo, "git cherry-pick "+theirs[:8])
	settleLooks(t)
	wantGenerated(t, session, 0)

	// Through a wrapper: the reflog says cherry-pick, not commit.
	gitOut(t, repo, "reset", "-q", "--hard", "HEAD~1")
	settleAsPrompt(t, session, repo)
	gitOut(t, repo, "cherry-pick", theirs)
	picked = gitOut(t, repo, "rev-parse", "HEAD")
	shellRan(t, session, repo, "./scripts/backport.sh")
	settleLooks(t)
	wantGenerated(t, session, 0)

	sweep(t, repo)
	wantCommit(t, openDB(t), "the cherry-pick of a teammate's commit", picked, 0)
}

func testAgentReverts(t *testing.T, home string) {
	repo, _, path, content := lookRepo(t, home, "look-revert")
	const session = "look-revert"
	// A hand-written commit removes three lines; the agent reverts it.
	writeFile(t, path, strings.Replace(content, "k3 = old\nk4 = old\nk5 = old\n", "", 1))
	commitAll(t, repo, "drop three lines, by hand")
	promptSubmitted(t, session, repo)
	gitOut(t, repo, "revert", "--no-edit", "HEAD")
	reverted := gitOut(t, repo, "rev-parse", "HEAD")
	shellRan(t, session, repo, "git revert --no-edit HEAD")
	settleLooks(t)

	wantGenerated(t, session, 0)
	sweep(t, repo)
	wantCommit(t, openDB(t), "the revert", reverted, 0)
}

func testQueuedShellThenEditElsewhere(t *testing.T, home string) {
	repo, _, path, content := lookRepo(t, home, "look-queued-other")
	promptSubmitted(t, "look-queued-other-script", repo)
	// A script changes cfg.ini; right after, an edit by another session adds
	// lines to a second file. Both hooks fired before the worker handled
	// either, so the disk holds both changes throughout.
	fired := time.Now().Add(-3 * time.Second)
	writeFile(t, path, scripted(content, "k2", "k5", "k8"))
	other := filepath.Join(repo, "other.txt")
	writeFile(t, other, numbered(4, "other %d"))
	deliverAt(t, "claude-code", repo, shellPayload("look-queued-other-script", repo, scriptCommand), fired)
	deliverAt(t, "claude-code", repo, claudeEdit("look-queued-other-edit", "Write", other,
		map[string]any{"content": numbered(4, "other %d")}, nil, map[string]any{"type": "create", "originalFile": nil}), fired.Add(time.Second))
	settleLooks(t)

	wantGenerated(t, "look-queued-other-script", 3)
	wantGenerated(t, "look-queued-other-edit", 4)
}

func testQueuedShellThenEditSameFile(t *testing.T, home string) {
	repo, _, path, content := lookRepo(t, home, "look-queued-same")
	promptSubmitted(t, "look-queued-same-script", repo)
	// A script changes one line of cfg.ini; right after, an edit by another
	// session inserts three lines into the same file. The edit is handled
	// before the script's look runs, and finds a line it cannot explain.
	fired := time.Now().Add(-3 * time.Second)
	afterScript := scripted(content, "k2")
	afterEdit, edit := insertPayload("look-queued-same-edit", path, afterScript, 5, "a1 = new", "a2 = new", "a3 = new")
	writeFile(t, path, afterEdit)
	deliverAt(t, "claude-code", repo, shellPayload("look-queued-same-script", repo, scriptCommand), fired)
	deliverAt(t, "claude-code", repo, edit, fired.Add(time.Second))
	// Until the look has run, the unexplained line is nobody's.
	settleLooks(t)

	wantGenerated(t, "look-queued-same-script", 1)
	wantGenerated(t, "look-queued-same-edit", 3)
	sha := commitAll(t, repo, "both")
	if err := GitCommit(repo, sha); err != nil {
		t.Fatal(err)
	}
	db := openDB(t)
	for session, want := range map[string]int{"look-queued-same-script": 1, "look-queued-same-edit": 3} {
		var got int
		if err := db.QueryRow(`
			SELECT COALESCE(SUM(cs.lines_attributed), 0) FROM commit_sessions cs
			JOIN commits c ON c.id = cs.commit_id WHERE c.sha = ? AND cs.session_id = ?`, sha, session).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("session %s: %d committed lines, want %d", session, got, want)
		}
	}
}

func testPromptHandledLate(t *testing.T, home string) {
	repo, _, path, content := lookRepo(t, home, "look-late-prompt")
	const session = "look-late-prompt"
	promptSubmitted(t, session, repo)
	shellRan(t, session, repo, "ls")
	settleLooks(t)
	// A new turn: the prompt's hook fired three seconds ago, the agent's
	// script has already run, and only now does the worker get to either.
	fired := time.Now().Add(-3 * time.Second)
	writeFile(t, path, scripted(content, "k2", "k5", "k8"))
	deliverAt(t, "claude-code", repo, map[string]any{
		"hook_event_name": "UserPromptSubmit", "session_id": session, "cwd": repo, "prompt": "next",
	}, fired)
	deliverAt(t, "claude-code", repo, shellPayload(session, repo, scriptCommand), fired.Add(time.Second))
	settleLooks(t)

	// The file changed after the prompt was submitted, so the prompt's look
	// leaves it to the shell command's.
	wantGenerated(t, session, 3)
}

func testPromptTextIsNotStored(t *testing.T, home string) {
	repo, _, _, _ := lookRepo(t, home, "look-prompt-text")
	const session = "look-prompt-text"
	const secret = "the launch date is the fourteenth"
	deliver(t, "claude-code", repo, map[string]any{
		"hook_event_name": "UserPromptSubmit", "session_id": session, "cwd": repo, "prompt": secret,
	})
	db := openDB(t)
	var payload string
	if err := db.QueryRow(`SELECT payload FROM events WHERE session_id = ? AND event_type = 'UserPromptSubmit'`, session).Scan(&payload); err != nil {
		t.Fatalf("the prompt event was not recorded: %v", err)
	}
	if strings.Contains(payload, "launch date") || strings.Contains(payload, `"prompt"`) {
		t.Errorf("the stored event carries the prompt: %s", payload)
	}
	var prompts int
	if err := db.QueryRow(`SELECT COUNT(*) FROM session_prompts WHERE session_id = ?`, session).Scan(&prompts); err != nil {
		t.Fatal(err)
	}
	if prompts != 0 {
		t.Errorf("%d prompt(s) stored from the hook; Claude Code's prompts come from its transcript", prompts)
	}
	// An agent with no transcript to read still has its prompt kept.
	deliver(t, "devin", repo, map[string]any{
		"hook_event_name": "UserPromptSubmit", "session_id": session + "-devin", "cwd": repo, "prompt": "devin prompt",
	})
	if err := db.QueryRow(`SELECT COUNT(*) FROM session_prompts WHERE session_id = ?`, session+"-devin").Scan(&prompts); err != nil || prompts != 1 {
		t.Errorf("devin's prompt: %d stored (err %v), want 1", prompts, err)
	}
	settleLooks(t)
}

func testGitDoesNotAnswer(t *testing.T, home string) {
	repo, _, path, content := lookRepo(t, home, "look-slow")
	const session = "look-slow"
	promptSubmitted(t, session, repo)
	writeFile(t, path, scripted(content, "k2"))

	saved := lookTimeout
	lookTimeout = time.Nanosecond
	shellRan(t, session, repo, scriptCommand)
	settleLooks(t)
	lookTimeout = saved

	// The event stays unmeasured, and the repository is left alone for a
	// while: a slow repository costs one timeout, not one per shell call.
	var generated sql.NullInt64
	if err := openDB(t).QueryRow(`SELECT lines_generated FROM events WHERE session_id = ? AND event_type = 'PostToolUse'`, session).Scan(&generated); err != nil {
		t.Fatal(err)
	}
	if generated.Valid {
		t.Errorf("lines_generated = %d on an event whose look could not run", generated.Int64)
	}
	look, ok := openDB(t).RepoLook(gitx.Canonicalize(repo))
	if !ok || !look.SlowUntil.After(time.Now().Add(5*time.Minute)) {
		t.Fatalf("the repository is not marked slow: %+v ok=%v", look, ok)
	}
	writeFile(t, path, scripted(content, "k2", "k3"))
	shellRan(t, session, repo, scriptCommand)
	settleLooks(t)
	wantGenerated(t, session, 0)
}

// The controlled test in the report that started this work: an agent session
// whose lines an engineer touched up before committing, and a second commit
// made of lines written by scripts. Built by hand in the same shape.
func testReportedShape(t *testing.T, home string) {
	repo, _, _, _ := lookRepo(t, home, "look-reported")
	const session = "look-reported"
	promptSubmitted(t, session, repo)

	// Commit one: the agent writes a 30-line file. Before committing, the
	// engineer rewrites four of its lines and adds three of their own.
	v := numbered(30, "$row[%d] = compute();")
	agentWrites(t, session, repo, "List.php", v)
	byHand := v
	for _, n := range []string{"3", "11", "19", "27"} {
		byHand = strings.Replace(byHand, "$row["+n+"] = compute();\n", "$row["+n+"] = reviewed();\n", 1)
	}
	byHand = "<?php\n// reviewed\n" + byHand + "// end\n"
	writeFile(t, filepath.Join(repo, "List.php"), byHand)
	added, attributed := commitAndAttribute(t, repo, session)
	wantAttributed(t, added, attributed, 33, 26)

	// Commit two, in the same turn: ten lines, every one written by a script
	// the agent ran. The engineer's touch-ups and commit above were made by
	// hand while the agent was at work; they are not the script's.
	laterSecond()
	writeFile(t, filepath.Join(repo, "schema.json"), numbered(10, `  "table_%d": true,`))
	shellRan(t, session, repo, "python3 - <<'EOF'\nimport json\n# writes schema.json\nEOF")
	settleLooks(t)
	added, attributed = commitAndAttribute(t, repo, session)
	wantAttributed(t, added, attributed, 10, 10)

	// Generated is what the session wrote, net of nothing here: 30 and 10.
	generated, reworked := generatedBy(t, session)
	if generated != 40 || reworked != 0 {
		t.Errorf("generated %d reworked %d, want 40 and 0", generated, reworked)
	}
	// And the first commit is still credited what it was.
	var first int
	if err := openDB(t).QueryRow(`
		SELECT COALESCE(SUM(cs.lines_attributed), 0) FROM commit_sessions cs JOIN commits c ON c.id = cs.commit_id
		WHERE cs.session_id = ? AND c.lines_added = 33`, session).Scan(&first); err != nil || first != 26 {
		t.Errorf("the first commit is credited %d lines after the script's look (err %v), want 26", first, err)
	}
}

func TestCommandMovesContent(t *testing.T) {
	moves := []string{
		"git pull",
		"git pull origin main",
		"git -C ../other merge feature",
		"cd repo && git checkout main",
		"git stash pop",
		"git stash && make build && git stash apply",
		"python3 gen.py; git reset --hard",
		"git -c user.name=x rebase main",
		"git cherry-pick abc123",
		"git revert --no-edit HEAD",
		"git restore src/a.go",
		"git switch -c feature",
		"git apply fix.patch",
		"git am < series.mbox",
		"git mv a.go b.go",
		"(git pull)",
		"echo start; git --no-pager checkout -- file.txt",
		// git named by its path, or past an alias.
		"/usr/bin/git stash pop",
		"/usr/bin/git apply fix.patch",
		"cd repo && /opt/homebrew/bin/git -C . checkout main",
		"./tools/git reset --hard",
		`\git stash apply`,
		`"/usr/local/bin/git" pull`,
	}
	for _, c := range moves {
		if !commandMovesContent(c) {
			t.Errorf("%q should count as moving content", c)
		}
	}
	writes := []string{
		"python3 gen.py",
		"git add -A && git commit -m x",
		"git status",
		"git diff HEAD~1",
		"git log --oneline",
		"git stash",
		"git stash list",
		"git push origin main",
		"git commit --amend --no-edit",
		"echo 'git pull' > notes.txt",
		"legit pull",
		"/usr/bin/legit pull",
		"ls /usr/bin/git",
		"/usr/bin/git status",
		"git fetch origin",
		"gh pr checkout 12",
	}
	for _, c := range writes {
		if c == "echo 'git pull' > notes.txt" {
			// Text that only mentions a command is taken for one: the side
			// to err on is crediting nothing.
			continue
		}
		if commandMovesContent(c) {
			t.Errorf("%q should not count as moving content", c)
		}
	}
}

func TestCommandCommits(t *testing.T) {
	for command, want := range map[string]bool{
		"git commit -m x": true,
		"python3 gen.py && git add -A && git commit -q -m generated": true,
		"git -C ../repo commit -am wip":                              true,
		"git -c user.name=x commit --allow-empty -m e":               true,
		"/usr/bin/git commit -m x":                                   true,
		"python3 gen.py && /opt/homebrew/bin/git -C . commit -am x":  true,
		"ls /usr/bin/git && echo commit":                             false,
		"git status":                                                 false,
		"git log --grep commit":                                      false,
		"python3 gen.py":                                             false,
		"gitcommit -m x":                                             false,
	} {
		if got := commandCommits(command); got != want {
			t.Errorf("commandCommits(%q) = %v, want %v", command, got, want)
		}
	}
}

func TestParseStatus(t *testing.T) {
	out := strings.Join([]string{
		"# branch.oid 1111111111111111111111111111111111111111",
		"# branch.head main",
		"1 .M N... 100644 100644 100644 aaaa bbbb src/changed.go",
		"1 M. N... 100644 100644 100644 aaaa bbbb staged only.go",
		"1 .D N... 100644 100644 000000 aaaa aaaa gone.go",
		"1 D. N... 100644 000000 000000 aaaa 0000 staged-delete.go",
		"2 R. N... 100644 100644 100644 aaaa bbbb R100 new name.go",
		"old name.go",
		"u UU N... 100644 100644 100644 100644 aaaa bbbb cccc conflicted.go",
		"? new.txt",
		"? newdir/",
		"! ignored.log",
		"",
	}, "\x00")
	status, dirs := parseStatus(out)
	if status.head != "1111111111111111111111111111111111111111" {
		t.Errorf("head %q", status.head)
	}
	want := []string{"src/changed.go", "staged only.go", "new name.go", "conflicted.go", "new.txt"}
	if !reflect.DeepEqual(status.files, want) {
		t.Errorf("files %q, want %q", status.files, want)
	}
	if !reflect.DeepEqual(dirs, []string{"newdir/"}) {
		t.Errorf("untracked directories %q", dirs)
	}
	if status, _ := parseStatus("# branch.oid (initial)\x00? a.txt\x00"); status.head != "" || len(status.files) != 1 {
		t.Errorf("a repository with no commit: %+v", status)
	}
}

func TestALookThatCannotReadEverythingIsNotTrustedForUnseenFiles(t *testing.T) {
	_ = store.Close()
	home := isolateIngest(t)
	t.Cleanup(func() { _ = store.Close() })
	openDB(t)
	repo, _, path, content := lookRepo(t, home, "look-partial")
	const session = "look-partial"
	// The engineer has more files changed than one look reads, cfg.ini among
	// the ones it does not get to (names sort before it).
	for i := 0; i < maxLookFiles+5; i++ {
		writeFile(t, filepath.Join(repo, "a", fmt.Sprintf("f%03d.txt", i)), "by hand\n")
	}
	gitOut(t, repo, "add", "-A")
	writeFile(t, path, strings.Replace(content, "k1 = old", "k1 = by hand", 1))
	promptSubmitted(t, session, repo)
	look, ok := openDB(t).RepoLook(gitx.Canonicalize(repo))
	if !ok || !look.Partial {
		t.Fatalf("test setup: the prompt's look read everything (%+v)", look)
	}

	// A script now changes another line of cfg.ini. Whether the prompt's
	// look saw the file or not, the hand-written line must not be credited.
	data, _ := os.ReadFile(path)
	writeFile(t, path, strings.Replace(string(data), "k9 = old", "k9 = scripted", 1))
	shellRan(t, session, repo, scriptCommand)
	settleLooks(t)
	// cfg.ini was among the files the prompt's look did not get to, so it
	// has no map, and nothing says its hand-written line was there before
	// the script ran. The script's own line goes uncredited with it: the
	// side to err on.
	wantGenerated(t, session, 0)
	var byHand int
	m, _ := openDB(t).FileMap(gitx.Canonicalize(path))
	for i := range m.Hashes {
		if _, ok := m.AuthorOf(i); ok && m.Hashes[i] == store.LineHash("k1 = by hand") {
			byHand++
		}
	}
	if byHand != 0 {
		t.Error("the hand-written line has an author")
	}
}

func TestALookThatRunsLongAfterItsCommandOnlyRecords(t *testing.T) {
	_ = store.Close()
	home := isolateIngest(t)
	t.Cleanup(func() { _ = store.Close() })
	openDB(t)
	repo, _, path, content := lookRepo(t, home, "look-stale")
	const session = "look-stale"
	promptSubmitted(t, session, repo)
	// The worker was not running: the shell call's hook fired half an hour
	// ago, and the file has seen a script and a hand edit since. Nothing
	// says which is which any more.
	writeFile(t, path, scripted(content, "k2")+"k11 = by hand\n")
	deliverAt(t, "claude-code", repo, shellPayload(session, repo, scriptCommand), time.Now().Add(-30*time.Minute))
	settleLooks(t)
	wantGenerated(t, session, 0)

	// The file is on record as it stands, so the next command is credited
	// only what it changes.
	data, _ := os.ReadFile(path)
	writeFile(t, path, strings.Replace(string(data), "k5 = old", "k5 = scripted", 1))
	shellRan(t, session, repo, scriptCommand)
	settleLooks(t)
	wantGenerated(t, session, 1)
}

func TestLooksAgainstWeakEvidence(t *testing.T) {
	_ = store.Close()
	home := isolateIngest(t)
	t.Cleanup(func() { _ = store.Close() })
	openDB(t)

	t.Run("a file the agent copies or moves is not written by it", func(t *testing.T) { testCopyAndMove(t, home) })
	t.Run("when git cannot say what a file was, nothing in it is credited", func(t *testing.T) { testBaseUnknown(t, home) })
	t.Run("the first look after one that could not be made only records", func(t *testing.T) { testLookAfterABlackout(t, home) })
	t.Run("a commit made by hand before the agent's own commit stays the engineer's", func(t *testing.T) { testHandCommitBeforeAgentCommit(t, home) })
	t.Run("a commit whose message the command does not name is not the command's", func(t *testing.T) { testCommitTheCommandDoesNotName(t, home) })
	t.Run("a changed file that stays as it is keeps its map", func(t *testing.T) { testUnchangedDirtyFileKeepsItsMap(t, home) })
	t.Run("a look that ran before a later edit was handled gives its lines back", func(t *testing.T) { testLookRanAheadOfAnEdit(t, home) })
	t.Run("looks wait for events that are still queued", func(t *testing.T) { testLooksWaitForTheQueue(t, home) })
	t.Run("a commit that a rewrite replaced is not credited again", func(t *testing.T) { testReplacedCommitStaysRetired(t, home) })
	t.Run("a note written after its commit was pushed is sent", func(t *testing.T) { testNoteSentAfterLatePush(t, home) })
	t.Run("lines a look gives back are no longer in the shell call's line ranges", func(t *testing.T) { testGivenBackLinesLeaveTheRanges(t, home) })
	t.Run("line ranges already uploaded are corrected in place when lines are given back", func(t *testing.T) { testGivenBackLinesAfterUpload(t, home) })
	t.Run("a committed file too large to keep a map for is not read from git", func(t *testing.T) { testLargeBaseFileIsNotRead(t, home) })
	t.Run("after a look that could not read everything, a queued edit still holds a script's lines in a file with a map", func(t *testing.T) { testPartialLookThenScriptAndEdit(t, home) })
	t.Run("after a look that could not read everything, nothing is held in a file with no map", func(t *testing.T) { testPartialLookHoldsNothingWithoutAMap(t, home) })
}

func testCopyAndMove(t *testing.T, home string) {
	repo, _, path, content := lookRepo(t, home, "weak-copy")
	const session = "weak-copy"
	writeFile(t, filepath.Join(repo, "big.txt"), numbered(40, "written by hand %d"))
	commitAll(t, repo, "add big, by hand")
	promptSubmitted(t, session, repo)

	// `cp cfg.ini cfg.copy.ini` and `mv big.txt renamed.txt`: every line of
	// both new files was already in the repository.
	writeFile(t, filepath.Join(repo, "cfg.copy.ini"), content)
	if err := os.Rename(filepath.Join(repo, "big.txt"), filepath.Join(repo, "renamed.txt")); err != nil {
		t.Fatal(err)
	}
	shellRan(t, session, repo, "cp cfg.ini cfg.copy.ini && mv big.txt renamed.txt")
	settleLooks(t)
	wantGenerated(t, session, 0)

	// A file the command really wrote, next to them, is still credited.
	writeFile(t, filepath.Join(repo, "fresh.txt"), numbered(3, "generated %d"))
	shellRan(t, session, repo, "python3 gen.py")
	settleLooks(t)
	wantGenerated(t, session, 3)

	added, attributed := commitAndAttribute(t, repo, session)
	// The commit adds the copy (10 lines) and the fresh file (3); git pairs
	// the move as a rename. Only the fresh file is the agent's.
	if attributed != 3 || added < 13 {
		t.Errorf("commit added %d lines, %d attributed to the agent; want at least 13 and exactly 3", added, attributed)
	}
	_ = path
}

func testBaseUnknown(t *testing.T, home string) {
	repo, _, path, content := lookRepo(t, home, "weak-base")
	const session = "weak-base"
	promptSubmitted(t, session, repo)
	// The agent removes three committed lines with an edit, so the file's
	// map no longer holds them...
	trimmed := strings.Replace(content, "k3 = old\nk4 = old\nk5 = old\n", "", 1)
	writeFile(t, path, trimmed)
	deliver(t, "claude-code", repo, claudeEdit(session, "Edit", path,
		map[string]any{"old_string": "k3 = old\nk4 = old\nk5 = old\n", "new_string": ""},
		[]map[string]any{hunk(2, " k2 = old", "-k3 = old", "-k4 = old", "-k5 = old", " k6 = old")},
		map[string]any{"originalFile": content}))
	// ...and a command puts them back, plus one line of its own.
	writeFile(t, path, content+"extra = new\n")

	// git answers everything but the question "what was this file".
	saved := lookBlobs
	lookBlobs = func(root string, names []string, limit int64) ([]blobAnswer, error) {
		return nil, os.ErrDeadlineExceeded
	}
	shellRan(t, session, repo, "make restore")
	settleLooks(t)
	lookBlobs = saved

	// With the file's version in git, the three restored lines are known
	// not to be new. Without it they cannot be told from the one that is,
	// so none is credited. The file is on record for the next look.
	wantGenerated(t, session, 0)
	writeFile(t, path, scripted(content, "k9")+"extra = new\n")
	shellRan(t, session, repo, scriptCommand)
	settleLooks(t)
	wantGenerated(t, session, 1)
}

func testLookAfterABlackout(t *testing.T, home string) {
	repo, _, path, content := lookRepo(t, home, "weak-blackout")
	const session = "weak-blackout"
	promptSubmitted(t, session, repo)

	// git stops answering: this shell call's look cannot be made, and the
	// repository is left alone for a while.
	saved := lookTimeout
	lookTimeout = time.Nanosecond
	shellRan(t, session, repo, "ls")
	settleLooks(t)
	lookTimeout = saved
	// During that while a turn ends, the engineer edits by hand and prompts
	// again; neither the prompt's look nor anything else is made.
	byHand := strings.Replace(content, "k1 = old", "k1 = by hand", 1)
	writeFile(t, path, byHand)
	promptSubmitted(t, session, repo)

	// The while is over. The next shell call changes one more line.
	db := openDB(t)
	if _, err := db.Exec(`UPDATE repo_looks SET slow_until = ? WHERE root = ?`,
		time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano), gitx.Canonicalize(repo)); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, scripted(byHand, "k5"))
	shellRan(t, session, repo, scriptCommand)
	settleLooks(t)
	// What changed since the last look that was made cannot be put down to
	// this one command.
	wantGenerated(t, session, 0)

	writeFile(t, path, scripted(byHand, "k5", "k8"))
	shellRan(t, session, repo, scriptCommand)
	settleLooks(t)
	wantGenerated(t, session, 1)
}

func testHandCommitBeforeAgentCommit(t *testing.T, home string) {
	repo, _, path, content := lookRepo(t, home, "weak-hand-commit")
	const session = "weak-hand-commit"
	promptSubmitted(t, session, repo)
	shellRan(t, session, repo, "ls")
	settleLooks(t)

	// Mid-turn, in another terminal, the engineer writes a file and commits
	// it. Then the agent's script changes three lines and the agent commits.
	writeFile(t, filepath.Join(repo, "notes.txt"), numbered(6, "by hand %d"))
	byHand := commitAll(t, repo, "notes, by hand")
	if err := GitCommit(repo, byHand); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, scripted(content, "k2", "k5", "k8"))
	agents := commitAll(t, repo, "regenerate cfg")
	if err := GitCommit(repo, agents); err != nil {
		t.Fatal(err)
	}
	shellRan(t, session, repo, "python3 gen.py && git add -A && git commit -q -m 'regenerate cfg'")
	settleLooks(t)

	wantGenerated(t, session, 3)
	db := openDB(t)
	wantCommit(t, db, "the engineer's commit", byHand, 0)
	wantCommit(t, db, "the agent's commit", agents, 3)
}

func testCommitTheCommandDoesNotName(t *testing.T, home string) {
	repo, _, path, content := lookRepo(t, home, "weak-unnamed")
	const session = "weak-unnamed"
	promptSubmitted(t, session, repo)
	shellRan(t, session, repo, "ls")
	settleLooks(t)

	// The engineer commits hand-written changes while an agent command that
	// also runs `git commit` is under way, but for nothing of its own (it
	// finds nothing to commit).
	writeFile(t, path, strings.Replace(content, "k1 = old", "k1 = by hand", 1))
	byHand := commitAll(t, repo, "tweak k1")
	if err := GitCommit(repo, byHand); err != nil {
		t.Fatal(err)
	}
	shellRan(t, session, repo, "npm test && git commit -am 'fix tests' || true")
	settleLooks(t)

	wantGenerated(t, session, 0)
	wantCommit(t, openDB(t), "the engineer's commit", byHand, 0)
}

func testUnchangedDirtyFileKeepsItsMap(t *testing.T, home string) {
	repo, _, path, content := lookRepo(t, home, "weak-expiry")
	const session = "weak-expiry"
	// A local change the engineer keeps for weeks without committing it.
	writeFile(t, path, strings.Replace(content, "k1 = old", "k1 = local", 1))
	promptSubmitted(t, session, repo)
	db := openDB(t)
	key := gitx.Canonicalize(path)
	old := time.Now().Add(-10 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	if res, err := db.Exec(`UPDATE file_snapshots SET updated_at = ? WHERE file_path = ?`, old, key); err != nil {
		t.Fatal(err)
	} else if n, _ := res.RowsAffected(); n != 1 {
		t.Fatal("test setup: the prompt's look left no map for the file")
	}

	shellRan(t, session, repo, "ls")
	settleLooks(t)
	// Seen again, unchanged: the map is marked as current, so the expiry of
	// rows untouched for a month does not take it and make the file look new.
	m, ok := db.FileMap(key)
	if !ok || time.Since(m.AsOf) > time.Hour {
		t.Errorf("the map was not refreshed: as of %v (found %v)", m.AsOf, ok)
	}
	wantGenerated(t, session, 0)
}

func testLookRanAheadOfAnEdit(t *testing.T, home string) {
	repo, _, path, content := lookRepo(t, home, "weak-ahead")
	promptSubmitted(t, "weak-ahead-script", repo)
	// A script changes one line; an edit by another session then inserts
	// three. The edit is on disk, but its event has not reached the worker
	// when the script's look runs.
	fired := time.Now().Add(-3 * time.Second)
	afterScript := scripted(content, "k2")
	afterEdit, edit := insertPayload("weak-ahead-edit", path, afterScript, 5, "a1 = new", "a2 = new", "a3 = new")
	writeFile(t, path, afterEdit)
	deliverAt(t, "claude-code", repo, shellPayload("weak-ahead-script", repo, scriptCommand), fired)
	settleLooks(t)
	// For now the look has credited the script with all four lines.
	wantGenerated(t, "weak-ahead-script", 4)

	// The edit's event arrives. It claims its three lines, and the script
	// gives them back.
	deliverAt(t, "claude-code", repo, edit, fired.Add(time.Second))
	wantGenerated(t, "weak-ahead-script", 1)
	wantGenerated(t, "weak-ahead-edit", 3)
}

func testLooksWaitForTheQueue(t *testing.T, home string) {
	repo, _, path, content := lookRepo(t, home, "weak-queue")
	const session = "weak-queue"
	promptSubmitted(t, session, repo)
	writeFile(t, path, scripted(content, "k2"))
	deliverAt(t, "claude-code", repo, shellPayload(session, repo, scriptCommand), time.Now().Add(-2*time.Second))

	// An event is waiting in the spool: it may be an edit of the very file.
	t.Cleanup(func() {
		entries, _ := os.ReadDir(spool.Dir())
		for _, e := range entries {
			if !e.IsDir() {
				_ = os.Remove(filepath.Join(spool.Dir(), e.Name()))
			}
		}
	})
	if err := spool.Write(spool.Envelope{Agent: "claude-code", Cwd: repo, ReceivedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Payload: []byte(`{"hook_event_name":"PostToolUse","session_id":"weak-queue","tool_name":"Read"}`)}); err != nil {
		t.Fatal(err)
	}
	if ran, err := RunLooks(time.Now()); err != nil || ran != 0 {
		t.Fatalf("a look ran ahead of a queued event: ran=%d err=%v", ran, err)
	}
	wantGenerated(t, session, 0)

	// A look that has waited longer than the backlog bound runs regardless,
	// so a steady stream of events cannot hold it off for good.
	if _, err := openDB(t).Exec(`UPDATE pending_looks SET fired_at = ?`,
		time.Now().Add(-2*LookBacklog).UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if ran, err := RunLooks(time.Now()); err != nil || ran != 1 {
		t.Fatalf("an overdue look did not run: ran=%d err=%v", ran, err)
	}
	wantGenerated(t, session, 1)
}

func testReplacedCommitStaysRetired(t *testing.T, home string) {
	repo, _, path, content := lookRepo(t, home, "weak-retired")
	const session = "weak-retired"
	promptSubmitted(t, session, repo)
	after := agentInserts(t, session, repo, path, content, 5, "a1 = new", "a2 = new", "a3 = new")
	original := commitAll(t, repo, "agent change")
	if err := GitCommit(repo, original); err != nil {
		t.Fatal(err)
	}
	gitOut(t, repo, "commit", "-q", "--amend", "-m", "agent change, reworded")
	amended := recordHead(t, repo)
	db := openDB(t)
	wantCommit(t, db, "the amended commit", amended, 3)
	wantCommit(t, db, "the commit it replaced", original, 0)

	// An edit of the same file is handled late: it fired before both
	// commits were made. Both are recent commits of that file, but only the
	// one still in the branch's history is looked at again. The replaced
	// one is left exactly as it is: its rows are not even rewritten.
	if _, err := db.Exec(`UPDATE commit_sessions SET synced_at = 'sent' WHERE commit_id = (SELECT id FROM commits WHERE sha = ?)`, original); err != nil {
		t.Fatal(err)
	}
	_, late := insertPayload(session, path, after, 0, "top = new")
	deliverAt(t, "claude-code", repo, late, time.Now().Add(-30*time.Second))
	wantCommit(t, db, "the amended commit, after the late edit", amended, 3)
	wantCommit(t, db, "the commit it replaced, after the late edit", original, 0)
	var touched int
	if err := db.QueryRow(`SELECT COUNT(*) FROM commit_sessions WHERE synced_at IS NULL AND commit_id = (SELECT id FROM commits WHERE sha = ?)`, original).Scan(&touched); err != nil {
		t.Fatal(err)
	}
	if touched != 0 {
		t.Error("the replaced commit was attributed again")
	}
}

func testNoteSentAfterLatePush(t *testing.T, home string) {
	repo, remote, path, content := lookRepo(t, home, "weak-push")
	const session = "weak-push"
	promptSubmitted(t, session, repo)
	// `python3 gen.py && git commit -am … && git push`, one command: the
	// commit is made, recorded with nothing to its name, and pushed, all
	// before the tool call's hook fires.
	writeFile(t, path, scripted(content, "k2", "k5"))
	sha := commitAll(t, repo, "regenerate and ship")
	if err := GitCommit(repo, sha); err != nil {
		t.Fatal(err)
	}
	gitOut(t, repo, "push", "-q", "origin", "main")
	if hasNote(repo, sha) || hasNote(remote, sha) {
		t.Fatal("test setup: the commit has a note before its lines are known")
	}
	shellRan(t, session, repo, "python3 gen.py && git commit -q -am 'regenerate and ship' && git push -q origin main")
	settleLooks(t)

	wantCommit(t, openDB(t), "the commit", sha, 2)
	if !hasNote(repo, sha) {
		t.Fatal("the commit has no note after the look")
	}
	if !hasNote(remote, sha) {
		t.Error("the note was written after the push and never sent")
	}
}

// lineRangesOf lists the line ranges on record for a session's events, each
// as "first-last", in file order. A range emptied in place (see
// store.RetractEventLines) is not a range.
func lineRangesOf(t *testing.T, session string) []string {
	t.Helper()
	rows, err := openDB(t).Query(`
		SELECT elr.start_line, elr.end_line FROM event_line_ranges elr
		JOIN events e ON e.id = elr.event_id
		WHERE e.session_id = ? AND elr.end_line >= elr.start_line
		ORDER BY elr.start_line`, session)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var first, last int
		if err := rows.Scan(&first, &last); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%d-%d", first, last))
	}
	return out
}

// committedBy is the lines of a commit attributed to a session.
func committedBy(t *testing.T, sha, session string) int {
	t.Helper()
	var n int
	if err := openDB(t).QueryRow(`
		SELECT COALESCE(SUM(cs.lines_attributed), 0) FROM commit_sessions cs
		JOIN commits c ON c.id = cs.commit_id WHERE c.sha = ? AND cs.session_id = ?`, sha, session).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// lookRanAhead plays a script that changes one line and an edit, by another
// session, that inserts three, with the script's look run before the edit's
// event arrives. It returns the repository, the file and the edit's payload,
// still to be delivered at the returned time.
func lookRanAhead(t *testing.T, home, name string) (repo, path string, edit map[string]any, at time.Time) {
	t.Helper()
	repo, _, path, content := lookRepo(t, home, name)
	promptSubmitted(t, name+"-script", repo)
	fired := time.Now().Add(-3 * time.Second)
	afterScript := scripted(content, "k2")
	afterEdit, edit := insertPayload(name+"-edit", path, afterScript, 5, "a1 = new", "a2 = new", "a3 = new")
	writeFile(t, path, afterEdit)
	deliverAt(t, "claude-code", repo, shellPayload(name+"-script", repo, scriptCommand), fired)
	settleLooks(t)
	wantGenerated(t, name+"-script", 4)
	if got, want := lineRangesOf(t, name+"-script"), []string{"2-2", "6-8"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("test setup: the look recorded ranges %v for the script, want %v", got, want)
	}
	return repo, path, edit, fired.Add(time.Second)
}

func testGivenBackLinesLeaveTheRanges(t *testing.T, home string) {
	const script, editor = "weak-ranges-script", "weak-ranges-edit"
	repo, path, edit, at := lookRanAhead(t, home, "weak-ranges")
	deliverAt(t, "claude-code", repo, edit, at)
	wantGenerated(t, script, 1)
	wantGenerated(t, editor, 3)

	// The script's ranges name its own line and nothing else: the three
	// lines it gave back are the edit's alone.
	if got, want := lineRangesOf(t, script), []string{"2-2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("the script's line ranges are %v, want %v", got, want)
	}
	var lines int
	if err := openDB(t).QueryRow(`
		SELECT COALESCE(SUM(elr.new_text_lines), 0) FROM event_line_ranges elr
		JOIN events e ON e.id = elr.event_id WHERE e.session_id = ?`, script).Scan(&lines); err != nil {
		t.Fatal(err)
	}
	if lines != 1 {
		t.Errorf("the script's range rows count %d line(s), want 1", lines)
	}
	for _, r := range openDB(t).UncommittedRanges(gitx.Canonicalize(repo), "cfg.ini") {
		if r.End < r.Start {
			t.Errorf("a range that names no line is read as one: %+v", r)
		}
	}

	// A commit matched by position (the file's map is gone) credits each
	// line once, to the event that wrote it, and its note names the same
	// lines.
	if _, err := openDB(t).Exec(`DELETE FROM file_snapshots WHERE file_path = ?`, gitx.Canonicalize(path)); err != nil {
		t.Fatal(err)
	}
	sha := commitAll(t, repo, "both")
	if err := GitCommit(repo, sha); err != nil {
		t.Fatal(err)
	}
	if got := committedBy(t, sha, script); got != 1 {
		t.Errorf("the script is credited %d committed line(s), want 1", got)
	}
	if got := committedBy(t, sha, editor); got != 3 {
		t.Errorf("the edit is credited %d committed line(s), want 3", got)
	}
	if got, want := rangesIn(noteOf(t, repo, sha), "cfg.ini"), "2,6-8"; got != want {
		t.Errorf("the note names lines %s of cfg.ini, want %s", got, want)
	}
}

func testGivenBackLinesAfterUpload(t *testing.T, home string) {
	const script = "weak-uploaded-script"
	repo, _, edit, at := lookRanAhead(t, home, "weak-uploaded")
	// The sync loop uploaded the script's ranges before the edit's event
	// was handled.
	db := openDB(t)
	if _, err := db.Exec(`
		UPDATE event_line_ranges SET synced_at = '2026-11-14T10:00:00Z'
		WHERE event_id IN (SELECT id FROM events WHERE session_id = ?)`, script); err != nil {
		t.Fatal(err)
	}
	deliverAt(t, "claude-code", repo, edit, at)
	wantGenerated(t, script, 1)
	if got, want := lineRangesOf(t, script), []string{"2-2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("the script's line ranges are %v, want %v", got, want)
	}
	// The row that named the three lines is not deleted: its uploaded copy
	// would keep counting them. It stays, names no line, and is due for
	// upload again. The row that did not change is not sent again.
	rows, err := db.Query(`
		SELECT elr.start_line, elr.end_line, elr.new_text_lines, elr.synced_at IS NULL
		FROM event_line_ranges elr JOIN events e ON e.id = elr.event_id
		WHERE e.session_id = ? ORDER BY elr.start_line`, script)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type row struct {
		first, last, lines int
		pending            bool
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.first, &r.last, &r.lines, &r.pending); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	want := []row{{2, 2, 1, false}, {6, 5, 0, true}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("range rows %+v, want %+v", got, want)
	}
}

func testLargeBaseFileIsNotRead(t *testing.T, home string) {
	repo, _, path, content := lookRepo(t, home, "weak-large-base")
	const session = "weak-large-base"
	// A generated file, larger than posthook keeps a map for, is in git,
	// with a small file on either side of it by name.
	big := filepath.Join(repo, "generated.txt")
	rows := numbered(60000, "row %07d of data")
	if len(rows) <= store.MaxSnapshotBytes {
		t.Fatalf("test setup: the generated file is only %d bytes", len(rows))
	}
	writeFile(t, big, rows)
	last := filepath.Join(repo, "zz.ini")
	writeFile(t, last, content)
	commitAll(t, repo, "add generated data")
	promptSubmitted(t, session, repo)

	// A command replaces the generated file with three lines, and changes a
	// line of each small file.
	writeFile(t, big, "a = 1\nb = 2\nc = 3\n")
	writeFile(t, path, scripted(content, "k2"))
	writeFile(t, last, scripted(content, "k7"))
	saved := lookBlobs
	calls, read, tooLarge := 0, 0, 0
	lookBlobs = func(root string, names []string, limit int64) ([]blobAnswer, error) {
		answers, err := saved(root, names, limit)
		calls++
		for _, a := range answers {
			read += len(a.content)
			if a.tooLarge {
				tooLarge++
			}
		}
		return answers, err
	}
	shellRan(t, session, repo, scriptCommand)
	settleLooks(t)
	lookBlobs = saved

	if tooLarge != 1 || read > 2*len(content) {
		t.Errorf("the look read %d bytes of committed versions and found %d too large, want the two small files only and 1", read, tooLarge)
	}
	if calls != 2 {
		t.Errorf("git was asked %d times, want 2: once up to the large file, once for the files after it", calls)
	}
	// The small files are credited as always, the one after the large file
	// too. What the large one was is not known, so nothing in it is credited.
	wantGenerated(t, session, 2)
}

// The reader stops at a file that is too large without reading it, and says
// which answers it has.
func TestRunLookBlobsStopsAtALargeFile(t *testing.T) {
	_ = store.Close()
	home := isolateIngest(t)
	t.Cleanup(func() { _ = store.Close() })
	repo, _, _, content := lookRepo(t, home, "blobs")
	writeFile(t, filepath.Join(repo, "big.txt"), numbered(2000, "row %05d"))
	writeFile(t, filepath.Join(repo, "dir", "inner.txt"), "inner\n")
	commitAll(t, repo, "more files")

	names := []string{"HEAD:cfg.ini", "HEAD:nothing.txt", "HEAD:dir", "HEAD:big.txt", "HEAD:dir/inner.txt"}
	answers, err := runLookBlobs(repo, names, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(answers) != 4 {
		t.Fatalf("%d answers, want 4: the reader stops at the large file", len(answers))
	}
	if a := answers[0]; !a.isBlob || a.tooLarge || string(a.content) != content {
		t.Errorf("cfg.ini: %+v", a)
	}
	if a := answers[1]; a.isBlob || a.tooLarge || a.content != nil {
		t.Errorf("a name with nothing under it: %+v", a)
	}
	if a := answers[2]; a.isBlob || a.tooLarge {
		t.Errorf("a directory: %+v", a)
	}
	if a := answers[3]; !a.isBlob || !a.tooLarge || a.content != nil {
		t.Errorf("the large file: %+v", a)
	}
	// With room for it, every name is answered.
	answers, err = runLookBlobs(repo, names, 1<<20)
	if err != nil || len(answers) != 5 || string(answers[4].content) != "inner\n" || len(answers[3].content) == 0 {
		t.Errorf("with a larger limit: %d answers, err %v", len(answers), err)
	}
}

// manyChangedFiles leaves a repository with more changed files than one look
// reads, all sorting before cfg.ini.
func manyChangedFiles(t *testing.T, repo string) {
	t.Helper()
	for i := 0; i < maxLookFiles+5; i++ {
		writeFile(t, filepath.Join(repo, "a", fmt.Sprintf("f%03d.txt", i)), "by hand\n")
	}
	gitOut(t, repo, "add", "-A")
}

func wantPartialLook(t *testing.T, repo string) {
	t.Helper()
	if look, ok := openDB(t).RepoLook(gitx.Canonicalize(repo)); !ok || !look.Partial {
		t.Fatalf("test setup: the look read everything (%+v)", look)
	}
}

func testPartialLookThenScriptAndEdit(t *testing.T, home string) {
	repo, _, path, content := lookRepo(t, home, "weak-partial-held")
	const script, editor = "weak-partial-held-script", "weak-partial-held-edit"
	// cfg.ini has a map: an edit put a line into it a minute ago.
	withEdit, first := insertPayload(editor, path, content, 8, "e0 = new")
	writeFile(t, path, withEdit)
	deliverAt(t, "claude-code", repo, first, time.Now().Add(-time.Minute))
	// The engineer has more files changed than one look reads.
	manyChangedFiles(t, repo)
	promptSubmitted(t, script, repo)
	wantPartialLook(t, repo)

	// A script changes one line of cfg.ini; right after, an edit inserts
	// three. The edit is handled first and holds the script's line for the
	// script's look. The file's map is older than the script, so the line
	// is known to be new, whatever the last look did not get to.
	fired := time.Now().Add(-3 * time.Second)
	afterScript := scripted(withEdit, "k2")
	afterEdit, edit := insertPayload(editor, path, afterScript, 5, "a1 = new", "a2 = new", "a3 = new")
	writeFile(t, path, afterEdit)
	deliverAt(t, "claude-code", repo, shellPayload(script, repo, scriptCommand), fired)
	deliverAt(t, "claude-code", repo, edit, fired.Add(time.Second))
	settleLooks(t)
	wantGenerated(t, script, 1)
	wantGenerated(t, editor, 4)
}

func testPartialLookHoldsNothingWithoutAMap(t *testing.T, home string) {
	repo, _, path, content := lookRepo(t, home, "weak-partial-nomap")
	const script, editor = "weak-partial-nomap-script", "weak-partial-nomap-edit"
	// More files are changed than one look reads, and cfg.ini, with a line
	// changed by hand, is among the ones the look does not get to.
	manyChangedFiles(t, repo)
	byHand := strings.Replace(content, "k1 = old", "k1 = by hand", 1)
	writeFile(t, path, byHand)
	promptSubmitted(t, script, repo)
	wantPartialLook(t, repo)

	// A script changes another line; right after, an edit inserts three and
	// is handled first. The file had no map: nothing says the hand-written
	// line was there before the script ran, so the edit holds nothing for
	// the script's look, and neither line is credited.
	fired := time.Now().Add(-3 * time.Second)
	afterScript := scripted(byHand, "k9")
	afterEdit, edit := insertPayload(editor, path, afterScript, 5, "a1 = new", "a2 = new", "a3 = new")
	writeFile(t, path, afterEdit)
	deliverAt(t, "claude-code", repo, shellPayload(script, repo, scriptCommand), fired)
	deliverAt(t, "claude-code", repo, edit, fired.Add(time.Second))
	settleLooks(t)
	wantGenerated(t, script, 0)
	wantGenerated(t, editor, 3)
	m, _ := openDB(t).FileMap(gitx.Canonicalize(path))
	for i := range m.Hashes {
		if a, ok := m.AuthorOf(i); ok && a.SessionID != editor {
			t.Errorf("line %d has author %+v: only the edit's lines have one", i+1, a)
		}
	}
}
