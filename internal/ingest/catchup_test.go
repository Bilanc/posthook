package ingest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bilanc/posthook/internal/paths"
	"github.com/bilanc/posthook/internal/spool"
	"github.com/bilanc/posthook/internal/store"
)

// Commits made in these tests go through plain git with POSTHOOK_BYPASS=1,
// so nothing records them as they are made: exactly the case catch-up is
// for. A test that wants a commit recorded "at commit time" calls GitCommit,
// as the git shadow does.

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// repoWithRemote makes a bare remote and a clone of it with one pushed
// commit on main, and returns the clone and the remote.
func repoWithRemote(t *testing.T, home, name string) (repo, remote string) {
	t.Helper()
	remote = filepath.Join(home, name+".git")
	gitOut(t, home, "init", "-q", "--bare", "--initial-branch=main", remote)
	repo = filepath.Join(home, name)
	initRepo(t, repo)
	gitOut(t, repo, "checkout", "-q", "-B", "main")
	gitOut(t, repo, "remote", "add", "origin", remote)
	writeFile(t, filepath.Join(repo, "README.md"), "# "+name+"\n")
	gitOut(t, repo, "add", "-A")
	gitOut(t, repo, "commit", "-q", "-m", "init")
	gitOut(t, repo, "push", "-q", "-u", "origin", "main")
	return repo, remote
}

// agentWrites has a Claude Code session create a file, as a Write of a new
// file reports it.
func agentWrites(t *testing.T, session, repo, rel, content string) {
	t.Helper()
	path := filepath.Join(repo, rel)
	writeFile(t, path, content)
	deliver(t, "claude-code", repo, claudeEdit(session, "Write", path,
		map[string]any{"content": content}, nil, map[string]any{"type": "create", "originalFile": nil}))
}

func commitAll(t *testing.T, repo, message string) string {
	t.Helper()
	gitOut(t, repo, "add", "-A")
	gitOut(t, repo, "commit", "-q", "-m", message)
	return gitOut(t, repo, "rev-parse", "HEAD")
}

// attributed returns the AI lines the store holds for a commit, and whether
// the commit is recorded at all.
func attributed(t *testing.T, db *store.DB, sha string) (int, bool) {
	t.Helper()
	var id string
	if err := db.QueryRow(`SELECT id FROM commits WHERE sha = ?`, sha).Scan(&id); err != nil {
		return 0, false
	}
	var n int
	if err := db.QueryRow(`SELECT COALESCE(SUM(lines_attributed), 0) FROM commit_sessions WHERE commit_id = ?`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n, true
}

func wantCommit(t *testing.T, db *store.DB, what, sha string, wantLines int) {
	t.Helper()
	got, ok := attributed(t, db, sha)
	if !ok {
		t.Errorf("%s (%s) is not recorded", what, sha[:7])
		return
	}
	if got != wantLines {
		t.Errorf("%s (%s): %d AI lines, want %d", what, sha[:7], got, wantLines)
	}
}

func wantNotRecorded(t *testing.T, db *store.DB, what, sha string) {
	t.Helper()
	if _, ok := attributed(t, db, sha); ok {
		t.Errorf("%s (%s) was recorded", what, sha[:7])
	}
}

// sweep looks at a repository now, whatever the Stop throttle says.
func sweep(t *testing.T, repo string) int {
	t.Helper()
	catchUpMu.Lock()
	lastCatchUp = map[string]time.Time{}
	catchUpMu.Unlock()
	n, err := CatchUpRepo(repo)
	if err != nil {
		t.Fatalf("CatchUpRepo: %v", err)
	}
	return n
}

// laterSecond waits until the clock's second has changed. A commit's time
// has one-second resolution, and an earlier commit closes a later one's
// window only when their times differ, as the times of unrelated commits do
// outside a test. The rewrites below do not wait: an amend or a rebase in the
// second of its original is the usual case.
func laterSecond() { time.Sleep(1100 * time.Millisecond) }

// gitAt runs git with the committer and author clocks set to when.
func gitAt(t *testing.T, dir string, when time.Time, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	stamp := when.UTC().Format(time.RFC3339)
	cmd.Env = append(os.Environ(), "GIT_COMMITTER_DATE="+stamp, "GIT_AUTHOR_DATE="+stamp)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// hasNote reports whether a commit has a posthook note in the repository at
// dir (a working tree or a bare remote).
func hasNote(dir, sha string) bool {
	cmd := exec.Command("git", "notes", "--ref="+paths.NotesRef, "show", sha)
	cmd.Dir = dir
	return cmd.Run() == nil
}

func TestCommitCatchUp(t *testing.T) {
	_ = store.Close()
	home := isolateIngest(t)
	t.Cleanup(func() { _ = store.Close() })
	// posthook is "installed" from here: the store exists.
	openDB(t)

	t.Run("a commit made by a git posthook never saw is recorded when the turn ends", func(t *testing.T) { testCatchUpOnStop(t, home) })
	t.Run("commits fetched from a teammate are not recorded; a local merge commit is", func(t *testing.T) { testCatchUpSkipsFetched(t, home) })
	t.Run("commits made before posthook was installed are not recorded", func(t *testing.T) { testCatchUpSkipsPreInstall(t, home) })
	t.Run("a rebased commit gets its lines and the original gives them up", func(t *testing.T) { testCatchUpRebase(t, home) })
	t.Run("an amended commit gets its lines and the original gives them up", func(t *testing.T) { testCatchUpAmend(t, home) })
	t.Run("a cherry-pick is attributed and the original keeps its lines", func(t *testing.T) { testCatchUpCherryPick(t, home) })
	t.Run("a squash of two commits carries both", func(t *testing.T) { testCatchUpSquash(t, home) })
	t.Run("an amend in the second of its original retires the original", func(t *testing.T) { testAmendInTheSameSecond(t, home) })
	t.Run("a replaced commit keeps the lines its replacement does not hold", func(t *testing.T) { testAmendThatDropsAFile(t, home) })
	t.Run("a commit on a deleted branch keeps its lines when a later commit only touches the same file", func(t *testing.T) { testCatchUpDeletedBranch(t, home) })
	t.Run("the same lines added to another file do not make a commit a rewrite", func(t *testing.T) { testCatchUpSameLinesInAnotherFile(t, home) })
	t.Run("an ancestor whose lines a commit adds again is not carried", func(t *testing.T) { testCatchUpRevertOfRevert(t, home) })
	t.Run("a note whose ranges belong to a commit recorded later is removed", func(t *testing.T) { testCatchUpRemovesStaleNote(t, home) })
	t.Run("a push job records commits that are on the remote by the time it runs, and sends their notes", func(t *testing.T) { testCatchUpPushJob(t, home) })
	t.Run("a force-pushed rebase retires the original it replaced on the remote", func(t *testing.T) { testCatchUpForcePush(t, home) })
	t.Run("one repository is looked at once a minute on Stop", func(t *testing.T) { testCatchUpThrottle(t, home) })
	t.Run("a commit is recorded from the worktree it was made in, and only once", func(t *testing.T) { testCatchUpWorktrees(t, home) })
	t.Run("a rewrite made in another checkout carries the commit recorded in a worktree", func(t *testing.T) { testCatchUpRewriteFromAnotherCheckout(t, home) })
	t.Run("a rewrite reported twice from another checkout is recorded once", func(t *testing.T) { testRewriteReportedTwice(t, home) })
	t.Run("a squash of commits from two worktrees carries both", func(t *testing.T) { testSquashAcrossWorktrees(t, home) })
	t.Run("a cherry-pick from another clone leaves the original on its branch alone", func(t *testing.T) { testCherryPickFromAnotherClone(t, home) })
	t.Run("an original is not retired when its clone cannot be looked at", func(t *testing.T) { testCherryPickWhenOriginalCloneCannotBeRead(t, home) })
	t.Run("a commit recorded in an unrelated repository is left alone", func(t *testing.T) { testCatchUpLeavesOtherRepositoriesAlone(t, home) })
	t.Run("a second rewrite is carried after git has dropped the first original", func(t *testing.T) { testRewriteAfterOriginalIsPruned(t, home) })
	t.Run("a second rewrite keeps one session's lines when the first dropped another's", func(t *testing.T) { testRewriteAfterPartialRetirement(t, home) })
}

func testCatchUpWorktrees(t *testing.T, home string) {
	repo, _ := repoWithRemote(t, home, "trees")
	tree := filepath.Join(home, "trees-wt-feature")
	gitOut(t, repo, "worktree", "add", "-q", "-b", "feature", tree)

	// The agent works in the worktree; the commit is made there by a git
	// posthook does not see.
	agentWrites(t, "trees-1", tree, "w.txt", numbered(4, "w %d"))
	sha := commitAll(t, tree, "add w")

	// Worktrees share their branches, so the main checkout could list this
	// commit too. Its edits were made in the worktree, though, and a look
	// from the main checkout must leave it alone.
	if n := sweep(t, repo); n != 0 {
		t.Errorf("a look from the main checkout recorded %d commit(s) of another worktree's branch", n)
	}
	db := openDB(t)
	wantNotRecorded(t, db, "the worktree's commit, seen from the main checkout", sha)

	if n := sweep(t, tree); n != 1 {
		t.Errorf("a look from the worktree recorded %d, want 1", n)
	}
	wantCommit(t, db, "the worktree's commit", sha, 4)

	// The branch is later checked out in the main checkout: the commit is
	// already recorded and is not recorded a second time there.
	gitOut(t, tree, "checkout", "-q", "--detach")
	gitOut(t, repo, "checkout", "-q", "feature")
	if n := sweep(t, repo); n != 0 {
		t.Errorf("recorded %d commit(s) again from the main checkout", n)
	}
	var copies int
	if err := db.QueryRow(`SELECT COUNT(*) FROM commits WHERE sha = ?`, sha).Scan(&copies); err != nil {
		t.Fatal(err)
	}
	if copies != 1 {
		t.Errorf("the commit is recorded %d times", copies)
	}
}

func testCatchUpRewriteFromAnotherCheckout(t *testing.T, home string) {
	repo, _ := repoWithRemote(t, home, "across")
	tree := filepath.Join(home, "across-wt-topic")
	gitOut(t, repo, "worktree", "add", "-q", "-b", "topic", tree)
	// The agent works in a worktree and the commit is recorded there.
	agentWrites(t, "across-1", tree, "x.txt", numbered(4, "x %d"))
	original := commitAll(t, tree, "add x")
	if err := GitCommit(tree, original); err != nil {
		t.Fatal(err)
	}
	db := openDB(t)
	wantCommit(t, db, "the commit made in the worktree", original, 4)

	// The worktree is removed. Its branch is checked out in the main
	// checkout and the commit is amended there, by a git posthook does not
	// see. The edits are the worktree's, and the main checkout has none.
	gitOut(t, repo, "worktree", "remove", "--force", tree)
	gitOut(t, repo, "checkout", "-q", "topic")
	gitOut(t, repo, "commit", "-q", "--amend", "-m", "add x, reworded")
	amended := gitOut(t, repo, "rev-parse", "HEAD")
	if amended == original {
		t.Fatal("test setup: the amend did not rewrite the commit")
	}

	sweep(t, repo)
	wantCommit(t, db, "the amended commit, made in the main checkout", amended, 4)
	wantCommit(t, db, "the commit it replaced", original, 0)
	// The amended commit is recorded with the checkout its edits were made
	// in, like the commit it replaces.
	var sameRow bool
	if err := db.QueryRow(`SELECT (SELECT repo_id FROM commits WHERE sha = ?) = (SELECT repo_id FROM commits WHERE sha = ?)`,
		amended, original).Scan(&sameRow); err != nil {
		t.Fatal(err)
	}
	if !sameRow {
		t.Errorf("the amended commit is recorded under another repository row than its original")
	}

	// A later look from the main checkout still finds it, and settles it
	// again without recording anything.
	if n := sweep(t, repo); n != 0 {
		t.Errorf("a second look recorded %d commit(s)", n)
	}
	wantCommit(t, db, "the amended commit after a second look", amended, 4)
}

func testRewriteReportedTwice(t *testing.T, home string) {
	repo, _ := repoWithRemote(t, home, "twice")
	tree := filepath.Join(home, "twice-wt-topic")
	gitOut(t, repo, "worktree", "add", "-q", "-b", "topic", tree)
	agentWrites(t, "twice-1", tree, "y.txt", numbered(4, "y %d"))
	original := commitAll(t, tree, "add y")
	if err := GitCommit(tree, original); err != nil {
		t.Fatal(err)
	}
	gitOut(t, repo, "worktree", "remove", "--force", tree)
	gitOut(t, repo, "checkout", "-q", "topic")
	gitOut(t, repo, "commit", "-q", "--amend", "-m", "add y, reworded")
	amended := gitOut(t, repo, "rev-parse", "HEAD")
	// The post-commit hook and the git shadow both report a commit.
	for i := 0; i < 2; i++ {
		if err := GitCommit(repo, amended); err != nil {
			t.Fatal(err)
		}
	}
	db := openDB(t)
	var copies int
	if err := db.QueryRow(`SELECT COUNT(*) FROM commits WHERE sha = ?`, amended).Scan(&copies); err != nil {
		t.Fatal(err)
	}
	if copies != 1 {
		t.Errorf("the amended commit is recorded %d times", copies)
	}
	wantCommit(t, db, "the amended commit", amended, 4)

	// A later look records another commit and settles this one again. It
	// keeps its lines and its note.
	writeFile(t, filepath.Join(repo, "z.txt"), "z\n")
	commitAll(t, repo, "add z")
	if n := sweep(t, repo); n != 1 {
		t.Errorf("recorded %d commit(s), want the new one", n)
	}
	wantCommit(t, db, "the amended commit after a later look", amended, 4)
	if !hasNote(repo, amended) {
		t.Errorf("the amended commit lost its note")
	}
}

func testSquashAcrossWorktrees(t *testing.T, home string) {
	repo, _ := repoWithRemote(t, home, "both")
	// One branch, worked on in two worktrees one after the other.
	first := filepath.Join(home, "both-wt-one")
	gitOut(t, repo, "worktree", "add", "-q", "-b", "topic", first)
	agentWrites(t, "both-1", first, "a.txt", numbered(4, "a %d"))
	one := commitAll(t, first, "add a")
	if err := GitCommit(first, one); err != nil {
		t.Fatal(err)
	}
	// The first worktree stays, on other work.
	gitOut(t, first, "checkout", "-q", "--detach", "main")
	second := filepath.Join(home, "both-wt-two")
	gitOut(t, repo, "worktree", "add", "-q", second, "topic")
	agentWrites(t, "both-2", second, "b.txt", numbered(3, "b %d"))
	two := commitAll(t, second, "add b")
	if err := GitCommit(second, two); err != nil {
		t.Fatal(err)
	}
	gitOut(t, repo, "worktree", "remove", "--force", second)

	// The branch is squashed in the main checkout before it is pushed.
	gitOut(t, repo, "checkout", "-q", "topic")
	gitOut(t, repo, "reset", "-q", "--soft", "main")
	squashed := commitAll(t, repo, "add a and b")

	sweep(t, repo)
	db := openDB(t)
	wantCommit(t, db, "the squashed commit", squashed, 7)
	wantCommit(t, db, "the commit from the first worktree", one, 0)
	wantCommit(t, db, "the commit from the second worktree", two, 0)

	// The squashed commit is recorded with the second worktree. In the
	// first, the commit it replaced still stands between its edits and
	// later work there: a file written by hand on the same line numbers
	// takes none of them.
	laterSecond()
	writeFile(t, filepath.Join(first, "a.txt"), numbered(4, "typed by hand %d"))
	byHand := commitAll(t, first, "add a by hand")
	sweep(t, first)
	wantCommit(t, db, "a later commit in the first worktree, typed by hand", byHand, 0)
	wantCommit(t, db, "the squashed commit after it", squashed, 7)
}

func testCherryPickFromAnotherClone(t *testing.T, home string) {
	repo, remote := repoWithRemote(t, home, "twins")
	other := filepath.Join(home, "twins-other")
	gitOut(t, home, "clone", "-q", remote, other)
	gitOut(t, other, "config", "user.email", "mate@example.com")
	gitOut(t, other, "config", "user.name", "Mate")

	// An unpublished commit on a branch of the first clone.
	gitOut(t, repo, "checkout", "-q", "-b", "topic")
	agentWrites(t, "twins-1", repo, "t.txt", numbered(4, "t %d"))
	original := commitAll(t, repo, "add t")
	if err := GitCommit(repo, original); err != nil {
		t.Fatal(err)
	}
	// The second clone takes it straight from the first and cherry-picks it.
	// No branch of the second clone holds the original.
	gitOut(t, other, "fetch", "-q", repo, "topic")
	gitOut(t, other, "cherry-pick", "FETCH_HEAD")
	picked := gitOut(t, other, "rev-parse", "HEAD")
	if picked == original {
		t.Fatal("test setup: the cherry-pick did not make a new commit")
	}

	sweep(t, other)
	db := openDB(t)
	wantCommit(t, db, "the cherry-pick", picked, 4)
	wantCommit(t, db, "the original, still on its branch in the first clone", original, 4)
}

func testCherryPickWhenOriginalCloneCannotBeRead(t *testing.T, home string) {
	repo, remote := repoWithRemote(t, home, "blind")
	other := filepath.Join(home, "blind-other")
	gitOut(t, home, "clone", "-q", remote, other)
	gitOut(t, other, "config", "user.email", "mate@example.com")
	gitOut(t, other, "config", "user.name", "Mate")
	gitOut(t, repo, "checkout", "-q", "-b", "topic")
	agentWrites(t, "blind-1", repo, "v.txt", numbered(4, "v %d"))
	original := commitAll(t, repo, "add v")
	if err := GitCommit(repo, original); err != nil {
		t.Fatal(err)
	}
	gitOut(t, other, "fetch", "-q", repo, "topic")
	gitOut(t, other, "cherry-pick", "FETCH_HEAD")
	picked := gitOut(t, other, "rev-parse", "HEAD")

	// git fails in the first clone for now. Its directory is there, so its
	// branches cannot be taken to be gone.
	gitDir, away := filepath.Join(repo, ".git"), filepath.Join(repo, ".git-away")
	if err := os.Rename(gitDir, away); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Rename(away, gitDir) })

	sweep(t, other)
	db := openDB(t)
	wantCommit(t, db, "the cherry-pick", picked, 4)
	wantCommit(t, db, "the original, in a clone that cannot be looked at", original, 4)
}

func testCatchUpLeavesOtherRepositoriesAlone(t *testing.T, home string) {
	upstream, upstreamRemote := repoWithRemote(t, home, "up")
	agentWrites(t, "up-1", upstream, "u.txt", numbered(3, "u %d"))
	shared := commitAll(t, upstream, "add u")
	if err := GitCommit(upstream, shared); err != nil {
		t.Fatal(err)
	}

	// A fork: a repository with a remote of its own that holds the same
	// commit, taken straight from the upstream clone and not yet pushed.
	forkRemote := filepath.Join(home, "up-fork.git")
	gitOut(t, home, "clone", "-q", "--bare", upstreamRemote, forkRemote)
	fork := filepath.Join(home, "up-fork")
	gitOut(t, home, "clone", "-q", forkRemote, fork)
	gitOut(t, fork, "config", "user.email", "dev@example.com")
	gitOut(t, fork, "config", "user.name", "Dev")
	gitOut(t, fork, "pull", "-q", "--ff-only", upstream, "main")
	writeFile(t, filepath.Join(fork, "own.txt"), "own\n")
	own := commitAll(t, fork, "add own")

	if n := sweep(t, fork); n != 1 {
		t.Errorf("recorded %d commit(s) in the fork, want its own one", n)
	}
	db := openDB(t)
	wantCommit(t, db, "the fork's own commit", own, 0)
	wantCommit(t, db, "the commit recorded in the upstream clone", shared, 3)
	// The fork has none of that commit's edits. Its look must not attribute
	// the other repository's record or write a note for it here.
	if hasNote(fork, shared) {
		t.Errorf("a look from the fork wrote a note for a commit recorded in another repository")
	}
}

func testRewriteAfterOriginalIsPruned(t *testing.T, home string) {
	repo, _ := repoWithRemote(t, home, "pruned")
	agentWrites(t, "pruned-1", repo, "p.txt", numbered(4, "p %d"))
	original := commitAll(t, repo, "add p")
	if err := GitCommit(repo, original); err != nil {
		t.Fatal(err)
	}
	gitOut(t, repo, "commit", "-q", "--amend", "-m", "add p, reworded")
	firstRewrite := gitOut(t, repo, "rev-parse", "HEAD")
	if err := GitCommit(repo, firstRewrite); err != nil {
		t.Fatal(err)
	}
	// git drops the original: nothing refers to it any more.
	gitOut(t, repo, "reflog", "expire", "--expire=now", "--all")
	gitOut(t, repo, "gc", "-q", "--prune=now")
	gone := exec.Command("git", "cat-file", "-e", original)
	gone.Dir = repo
	if gone.Run() == nil {
		t.Fatal("test setup: git still has the original")
	}

	// The store still lists the original as an earlier commit on this file.
	gitOut(t, repo, "commit", "-q", "--amend", "-m", "add p, reworded again")
	secondRewrite := gitOut(t, repo, "rev-parse", "HEAD")
	if err := GitCommit(repo, secondRewrite); err != nil {
		t.Fatal(err)
	}
	db := openDB(t)
	wantCommit(t, db, "the second rewrite", secondRewrite, 4)
	wantCommit(t, db, "the first rewrite", firstRewrite, 0)
}

func testRewriteAfterPartialRetirement(t *testing.T, home string) {
	repo, _ := repoWithRemote(t, home, "partial")
	path := filepath.Join(repo, "m.txt")
	// Two sessions write to one file: the first creates it, the second adds
	// three lines at its end.
	first := numbered(5, "first session %d")
	more := numbered(3, "second session %d")
	agentWrites(t, "partial-a", repo, "m.txt", first)
	writeFile(t, path, first+more)
	deliver(t, "claude-code", repo, claudeEdit("partial-b", "Edit", path,
		map[string]any{"old_string": "first session 5\n", "new_string": "first session 5\n" + more},
		[]map[string]any{hunk(5, " first session 5", "+second session 1", "+second session 2", "+second session 3")},
		map[string]any{"originalFile": first}))
	original := commitAll(t, repo, "add m")
	if err := GitCommit(repo, original); err != nil {
		t.Fatal(err)
	}
	db := openDB(t)
	wantCommit(t, db, "the commit with both sessions' lines", original, 8)

	// The amend takes the second session's lines out again.
	writeFile(t, path, first)
	gitOut(t, repo, "add", "-A")
	gitOut(t, repo, "commit", "-q", "--amend", "-m", "add m, shorter")
	firstRewrite := gitOut(t, repo, "rev-parse", "HEAD")
	if err := GitCommit(repo, firstRewrite); err != nil {
		t.Fatal(err)
	}
	wantCommit(t, db, "the first rewrite", firstRewrite, 5)
	// The original gave up the first session's lines and keeps the second's,
	// which no commit holds any more.
	wantCommit(t, db, "the original after the first rewrite", original, 3)

	gitOut(t, repo, "reflog", "expire", "--expire=now", "--all")
	gitOut(t, repo, "gc", "-q", "--prune=now")
	laterSecond()
	gitOut(t, repo, "commit", "-q", "--amend", "-m", "add m, shorter, reworded")
	secondRewrite := gitOut(t, repo, "rev-parse", "HEAD")
	if err := GitCommit(repo, secondRewrite); err != nil {
		t.Fatal(err)
	}
	wantCommit(t, db, "the second rewrite", secondRewrite, 5)
	wantCommit(t, db, "the first rewrite, replaced", firstRewrite, 0)
	wantCommit(t, db, "the original, still holding the dropped lines", original, 3)
}

func testCatchUpOnStop(t *testing.T, home string) {
	repo, _ := repoWithRemote(t, home, "stop")
	agentWrites(t, "stop-1", repo, "a.txt", numbered(5, "line %d"))
	// The engineer commits from an IDE: nothing records it.
	sha := commitAll(t, repo, "add a")
	db := openDB(t)
	wantNotRecorded(t, db, "the commit before the turn ended", sha)

	catchUpMu.Lock()
	lastCatchUp = map[string]time.Time{}
	catchUpMu.Unlock()
	deliver(t, "claude-code", repo, map[string]any{"hook_event_name": "Stop", "session_id": "stop-1", "cwd": repo})
	wantCommit(t, db, "the commit after the turn ended", sha, 5)
}

func testCatchUpSkipsFetched(t *testing.T, home string) {
	repo, remote := repoWithRemote(t, home, "team")
	mate := filepath.Join(home, "team-mate")
	gitOut(t, home, "clone", "-q", remote, mate)
	gitOut(t, mate, "config", "user.email", "mate@example.com")
	gitOut(t, mate, "config", "user.name", "Mate")
	writeFile(t, filepath.Join(mate, "theirs.txt"), "theirs\n")
	theirs := commitAll(t, mate, "theirs")
	gitOut(t, mate, "push", "-q", "origin", "HEAD:main")

	// Our own unpushed commit, then a pull that has to merge.
	writeFile(t, filepath.Join(repo, "ours.txt"), "ours\n")
	ours := commitAll(t, repo, "ours")
	gitOut(t, repo, "pull", "-q", "--no-rebase", "--no-edit", "origin", "main")
	merge := gitOut(t, repo, "rev-parse", "HEAD")
	if merge == ours {
		t.Fatal("test setup: the pull did not create a merge commit")
	}

	if n := sweep(t, repo); n != 2 {
		t.Errorf("recorded %d commits, want our commit and the merge", n)
	}
	db := openDB(t)
	wantCommit(t, db, "our commit", ours, 0)
	wantCommit(t, db, "the merge commit", merge, 0)
	wantNotRecorded(t, db, "the teammate's commit", theirs)
}

func testCatchUpSkipsPreInstall(t *testing.T, home string) {
	repo, _ := repoWithRemote(t, home, "old")
	writeFile(t, filepath.Join(repo, "old.txt"), "old\n")
	gitOut(t, repo, "add", "-A")
	cmd := exec.Command("git", "commit", "-q", "-m", "from before posthook")
	cmd.Dir = repo
	// Two days ago: recent enough to be looked at, older than the store.
	before := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	cmd.Env = append(os.Environ(), "GIT_COMMITTER_DATE="+before, "GIT_AUTHOR_DATE="+before)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, out)
	}
	old := gitOut(t, repo, "rev-parse", "HEAD")
	writeFile(t, filepath.Join(repo, "new.txt"), "new\n")
	recent := commitAll(t, repo, "after posthook")

	sweep(t, repo)
	db := openDB(t)
	wantNotRecorded(t, db, "the commit from before the install", old)
	wantCommit(t, db, "the commit from after it", recent, 0)
}

func testCatchUpRebase(t *testing.T, home string) {
	repo, _ := repoWithRemote(t, home, "rebase")
	gitOut(t, repo, "checkout", "-q", "-b", "feature")
	agentWrites(t, "rebase-1", repo, "f.txt", numbered(5, "f %d"))
	original := commitAll(t, repo, "add f")
	if err := GitCommit(repo, original); err != nil { // recorded at commit time
		t.Fatal(err)
	}
	db := openDB(t)
	wantCommit(t, db, "the commit as first made", original, 5)

	// main moves on, and the branch is rebased onto it.
	gitOut(t, repo, "checkout", "-q", "main")
	writeFile(t, filepath.Join(repo, "other.txt"), "other\n")
	commitAll(t, repo, "main moves")
	gitOut(t, repo, "checkout", "-q", "feature")
	gitOut(t, repo, "rebase", "-q", "main")
	rebased := gitOut(t, repo, "rev-parse", "HEAD")
	if rebased == original {
		t.Fatal("test setup: the rebase did not rewrite the commit")
	}

	sweep(t, repo)
	wantCommit(t, db, "the rebased commit", rebased, 5)
	wantCommit(t, db, "the commit it replaced", original, 0)
}

func testCatchUpAmend(t *testing.T, home string) {
	repo, _ := repoWithRemote(t, home, "amend")
	agentWrites(t, "amend-1", repo, "g.txt", numbered(4, "g %d"))
	original := commitAll(t, repo, "add g")
	if err := GitCommit(repo, original); err != nil {
		t.Fatal(err)
	}
	// The engineer adds a line by hand and amends.
	writeFile(t, filepath.Join(repo, "g.txt"), numbered(4, "g %d")+"by hand\n")
	gitOut(t, repo, "add", "-A")
	gitOut(t, repo, "commit", "-q", "--amend", "-m", "add g, with a note")
	amended := gitOut(t, repo, "rev-parse", "HEAD")

	sweep(t, repo)
	db := openDB(t)
	wantCommit(t, db, "the amended commit", amended, 4)
	wantCommit(t, db, "the commit it replaced", original, 0)
}

func testCatchUpCherryPick(t *testing.T, home string) {
	repo, _ := repoWithRemote(t, home, "pick")
	gitOut(t, repo, "checkout", "-q", "-b", "feature")
	agentWrites(t, "pick-1", repo, "h.txt", numbered(3, "h %d"))
	original := commitAll(t, repo, "add h")
	if err := GitCommit(repo, original); err != nil {
		t.Fatal(err)
	}
	gitOut(t, repo, "checkout", "-q", "main")
	gitOut(t, repo, "cherry-pick", original)
	picked := gitOut(t, repo, "rev-parse", "HEAD")

	sweep(t, repo)
	db := openDB(t)
	wantCommit(t, db, "the cherry-pick", picked, 3)
	wantCommit(t, db, "the original, still on its branch", original, 3)
}

func testCatchUpSquash(t *testing.T, home string) {
	repo, _ := repoWithRemote(t, home, "squash")
	gitOut(t, repo, "checkout", "-q", "-b", "feature")
	agentWrites(t, "squash-1", repo, "one.txt", numbered(3, "one %d"))
	first := commitAll(t, repo, "one")
	if err := GitCommit(repo, first); err != nil {
		t.Fatal(err)
	}
	agentWrites(t, "squash-1", repo, "two.txt", numbered(2, "two %d"))
	second := commitAll(t, repo, "two")
	if err := GitCommit(repo, second); err != nil {
		t.Fatal(err)
	}
	// Both commits become one before the push.
	gitOut(t, repo, "reset", "-q", "--soft", "main")
	squashed := commitAll(t, repo, "one and two")

	sweep(t, repo)
	db := openDB(t)
	wantCommit(t, db, "the squashed commit", squashed, 5)
	wantCommit(t, db, "the first commit it replaced", first, 0)
	wantCommit(t, db, "the second commit it replaced", second, 0)
}

func testCatchUpPushJob(t *testing.T, home string) {
	repo, remote := repoWithRemote(t, home, "push")
	agentWrites(t, "push-1", repo, "p.txt", numbered(6, "p %d"))
	sha := commitAll(t, repo, "add p")

	// What the shadow does around `git push`: list first, queue after.
	listed := nonEmptyLines(gitOut(t, repo, UnpushedCommitsArgs()...))
	if len(listed) != 1 || listed[0] != sha {
		t.Fatalf("unpushed before the push: %v, want [%s]", listed, sha)
	}
	gitOut(t, repo, "push", "-q", "origin", "main")
	if err := spool.WriteCommitJob(spool.CommitJob{RepoRoot: repo, SHAs: listed, NotesRemote: "origin",
		ReceivedAt: time.Now().UTC().Format(time.RFC3339Nano)}); err != nil {
		t.Fatal(err)
	}

	// Once pushed, a look at the repository no longer finds it...
	if n := sweep(t, repo); n != 0 {
		t.Errorf("a sweep after the push recorded %d commits; the commit is on the remote now", n)
	}
	db := openDB(t)
	wantNotRecorded(t, db, "the pushed commit before its job ran", sha)
	// ...which is why the list was taken before the push.
	handled, err := spool.DrainCommitJobs(ProcessCommitJob)
	if err != nil || handled != 1 {
		t.Fatalf("drained %d job(s), err %v", handled, err)
	}
	wantCommit(t, db, "the pushed commit after its job ran", sha, 6)
	if left, _ := spool.PendingCommitJobs(); left != 0 {
		t.Errorf("%d job(s) left in the spool", left)
	}
	// The shadow sent its notes before this commit had one. The job wrote
	// the note and sent it after them, to the remote the push went to.
	if !hasNote(remote, sha) {
		t.Errorf("the remote has no note for the pushed commit")
	}
}

func testAmendInTheSameSecond(t *testing.T, home string) {
	repo, _ := repoWithRemote(t, home, "quick")
	agentWrites(t, "quick-1", repo, "k.txt", numbered(4, "k %d"))
	// Both commits are made through the shadow, which records each as it is
	// made, and both carry the same time to the second.
	now := time.Now()
	gitOut(t, repo, "add", "-A")
	gitAt(t, repo, now, "commit", "-q", "-m", "add k")
	original := gitOut(t, repo, "rev-parse", "HEAD")
	if err := GitCommit(repo, original); err != nil {
		t.Fatal(err)
	}
	gitAt(t, repo, now, "commit", "-q", "--amend", "-m", "add k, reworded")
	amended := gitOut(t, repo, "rev-parse", "HEAD")
	if amended == original {
		t.Fatal("test setup: the amend did not rewrite the commit")
	}
	if a, b := gitOut(t, repo, "log", "-1", "--format=%ct", original), gitOut(t, repo, "log", "-1", "--format=%ct", amended); a != b {
		t.Fatalf("test setup: commit times differ (%s, %s)", a, b)
	}
	if err := GitCommit(repo, amended); err != nil {
		t.Fatal(err)
	}

	db := openDB(t)
	wantCommit(t, db, "the amended commit", amended, 4)
	wantCommit(t, db, "the commit it replaced in the same second", original, 0)
}

func testAmendThatDropsAFile(t *testing.T, home string) {
	repo, _ := repoWithRemote(t, home, "drop")
	agentWrites(t, "drop-1", repo, "kept.txt", numbered(3, "kept %d"))
	agentWrites(t, "drop-1", repo, "dropped.txt", numbered(2, "dropped %d"))
	original := commitAll(t, repo, "add two files")
	if err := GitCommit(repo, original); err != nil {
		t.Fatal(err)
	}
	// The amend takes one of the files out of the commit again.
	gitOut(t, repo, "rm", "-q", "--cached", "dropped.txt")
	gitOut(t, repo, "commit", "-q", "--amend", "-m", "add one file")
	amended := gitOut(t, repo, "rev-parse", "HEAD")
	if err := GitCommit(repo, amended); err != nil {
		t.Fatal(err)
	}

	db := openDB(t)
	wantCommit(t, db, "the amended commit", amended, 3)
	// Only what the replacement holds is given up. The other file's lines
	// are in no commit on a branch, and stay where they were recorded.
	wantCommit(t, db, "the commit it replaced", original, 2)
}

func testCatchUpDeletedBranch(t *testing.T, home string) {
	repo, _ := repoWithRemote(t, home, "gone")
	gitOut(t, repo, "checkout", "-q", "-b", "feature")
	agentWrites(t, "gone-1", repo, "c.txt", numbered(3, "from the agent %d"))
	onBranch := commitAll(t, repo, "add c on the branch")
	if err := GitCommit(repo, onBranch); err != nil {
		t.Fatal(err)
	}
	db := openDB(t)
	wantCommit(t, db, "the commit on the branch", onBranch, 3)

	// The branch goes away: merged by squash on the server and deleted, or
	// abandoned. No branch holds its commit any more.
	gitOut(t, repo, "checkout", "-q", "main")
	gitOut(t, repo, "branch", "-q", "-D", "feature")
	laterSecond()
	// Later someone writes the same file by hand on main, on the same line
	// numbers. This commit does not carry the other's work.
	writeFile(t, filepath.Join(repo, "c.txt"), numbered(3, "typed by hand %d"))
	byHand := commitAll(t, repo, "add c by hand")

	sweep(t, repo)
	wantCommit(t, db, "the later commit, typed by hand", byHand, 0)
	wantCommit(t, db, "the commit on the deleted branch", onBranch, 3)
}

func testCatchUpSameLinesInAnotherFile(t *testing.T, home string) {
	repo, _ := repoWithRemote(t, home, "elsewhere")
	gitOut(t, repo, "checkout", "-q", "-b", "feature")
	agentWrites(t, "elsewhere-1", repo, "a.txt", numbered(3, "from the agent %d"))
	onBranch := commitAll(t, repo, "add a on the branch")
	if err := GitCommit(repo, onBranch); err != nil {
		t.Fatal(err)
	}
	gitOut(t, repo, "checkout", "-q", "main")
	gitOut(t, repo, "branch", "-q", "-D", "feature")
	laterSecond()
	// A later commit, typed by hand, writes other lines to the same file on
	// the same line numbers, and happens to add the agent's lines to a
	// different file. The two commits share a file, but not what they add
	// to it.
	writeFile(t, filepath.Join(repo, "a.txt"), numbered(3, "typed by hand %d"))
	writeFile(t, filepath.Join(repo, "b.txt"), numbered(3, "from the agent %d"))
	byHand := commitAll(t, repo, "add a and b by hand")

	sweep(t, repo)
	db := openDB(t)
	wantCommit(t, db, "the later commit, typed by hand", byHand, 0)
	wantCommit(t, db, "the commit on the deleted branch", onBranch, 3)
}

func testCatchUpRevertOfRevert(t *testing.T, home string) {
	repo, remote := repoWithRemote(t, home, "again")
	agentWrites(t, "again-1", repo, "r.txt", numbered(4, "r %d"))
	first := commitAll(t, repo, "add r")
	if err := GitCommit(repo, first); err != nil {
		t.Fatal(err)
	}
	gitOut(t, repo, "push", "-q", "origin", "main")

	// A teammate reverts it. Their commit arrives by a pull and is never
	// recorded here, so the store does not know the commit after it descends
	// from the first.
	mate := filepath.Join(home, "again-mate")
	gitOut(t, home, "clone", "-q", remote, mate)
	gitOut(t, mate, "config", "user.email", "mate@example.com")
	gitOut(t, mate, "config", "user.name", "Mate")
	gitOut(t, mate, "revert", "--no-edit", first)
	gitOut(t, mate, "push", "-q", "origin", "HEAD:main")
	gitOut(t, repo, "pull", "-q", "--ff-only", "origin", "main")
	laterSecond()
	// The lines are put back, by a git posthook does not see.
	gitOut(t, repo, "revert", "--no-edit", "HEAD")
	again := gitOut(t, repo, "rev-parse", "HEAD")

	sweep(t, repo)
	db := openDB(t)
	// The same lines, added twice in one history. The first commit is an
	// ancestor, not an original this one replaces: it keeps its lines and
	// they are not counted a second time.
	wantCommit(t, db, "the commit that first added the lines", first, 4)
	wantCommit(t, db, "the commit that adds them again", again, 0)
}

func testCatchUpRemovesStaleNote(t *testing.T, home string) {
	repo, _ := repoWithRemote(t, home, "note")
	agentWrites(t, "note-1", repo, "n.txt", numbered(5, "n %d"))
	// The commit with the agent's lines is made by a git posthook does not
	// see...
	first := commitAll(t, repo, "add n")
	laterSecond()
	// ...and the next one, a line typed by hand, by one it does.
	writeFile(t, filepath.Join(repo, "n.txt"), numbered(5, "n %d")+"by hand\n")
	second := commitAll(t, repo, "one more line")
	if err := GitCommit(repo, second); err != nil {
		t.Fatal(err)
	}
	// Recorded alone, the second commit is the first one posthook knows on
	// this file. It adds one line, typed by hand, so it is credited nothing
	// and gets no note.
	if hasNote(repo, second) {
		t.Fatal("a commit that is credited nothing has a note")
	}
	// An earlier release wrote one here all the same, from the ranges of the
	// edits made before the commit, and it claims the agent's lines.
	stale := `{"v":1,"commit":"` + second + `","files":{"n.txt":[{"lines":"1-5","agent":"claude-code","session":"note-1","ts":"2026-11-01T10:00:00Z"}]}}`
	cmd := exec.Command("git", "notes", "--ref="+paths.NotesRef, "add", "-f", "-F", "-", second)
	cmd.Dir = repo
	cmd.Stdin = strings.NewReader(stale)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git notes add: %v: %s", err, out)
	}

	sweep(t, repo)
	db := openDB(t)
	wantCommit(t, db, "the commit with the agent's lines", first, 5)
	wantCommit(t, db, "the commit after it", second, 0)
	if !hasNote(repo, first) {
		t.Errorf("the commit with the agent's lines has no note")
	}
	if hasNote(repo, second) {
		t.Errorf("the later commit still has the note that claims the earlier commit's ranges")
	}
}

func testCatchUpForcePush(t *testing.T, home string) {
	repo, _ := repoWithRemote(t, home, "force")
	gitOut(t, repo, "checkout", "-q", "-b", "feature")
	agentWrites(t, "force-1", repo, "q.txt", numbered(4, "q %d"))
	original := commitAll(t, repo, "add q")
	if err := GitCommit(repo, original); err != nil {
		t.Fatal(err)
	}
	gitOut(t, repo, "push", "-q", "-u", "origin", "feature")

	gitOut(t, repo, "checkout", "-q", "main")
	writeFile(t, filepath.Join(repo, "other.txt"), "other\n")
	commitAll(t, repo, "main moves")
	gitOut(t, repo, "checkout", "-q", "feature")
	gitOut(t, repo, "rebase", "-q", "main")
	rebased := gitOut(t, repo, "rev-parse", "HEAD")

	// Before the force-push the remote still holds the original, so it is
	// still on a branch and keeps its lines.
	sweep(t, repo)
	db := openDB(t)
	wantCommit(t, db, "the rebased commit", rebased, 4)
	wantCommit(t, db, "the original while the remote still has it", original, 4)

	listed := nonEmptyLines(gitOut(t, repo, UnpushedCommitsArgs()...))
	gitOut(t, repo, "push", "-q", "--force", "origin", "feature")
	if _, err := RecordMissingCommits(repo, listed); err != nil {
		t.Fatal(err)
	}
	wantCommit(t, db, "the rebased commit after the force-push", rebased, 4)
	wantCommit(t, db, "the original once no branch holds it", original, 0)
}

func testCatchUpThrottle(t *testing.T, home string) {
	repo, _ := repoWithRemote(t, home, "throttle")
	writeFile(t, filepath.Join(repo, "one.txt"), "one\n")
	first := commitAll(t, repo, "one")
	if n := sweep(t, repo); n != 1 {
		t.Fatalf("first look recorded %d, want 1", n)
	}
	writeFile(t, filepath.Join(repo, "two.txt"), "two\n")
	second := commitAll(t, repo, "two")
	if n, err := CatchUpRepo(repo); err != nil || n != 0 {
		t.Errorf("a second look within the minute recorded %d (err %v)", n, err)
	}
	db := openDB(t)
	wantCommit(t, db, "the first commit", first, 0)
	wantNotRecorded(t, db, "the second commit, inside the minute", second)
	if n := sweep(t, repo); n != 1 {
		t.Errorf("the next look recorded %d, want 1", n)
	}
}
