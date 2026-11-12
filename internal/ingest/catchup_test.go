package ingest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// laterSecond waits until the clock's second has changed: a commit's time
// has one-second resolution, and "the commit before this one" needs the two
// to differ, as they always do outside a test.
func laterSecond() { time.Sleep(1100 * time.Millisecond) }

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
	t.Run("a push job records commits that are on the remote by the time it runs", func(t *testing.T) { testCatchUpPushJob(t, home) })
	t.Run("a force-pushed rebase retires the original it replaced on the remote", func(t *testing.T) { testCatchUpForcePush(t, home) })
	t.Run("one repository is looked at once a minute on Stop", func(t *testing.T) { testCatchUpThrottle(t, home) })
	t.Run("a commit is recorded from the worktree it was made in, and only once", func(t *testing.T) { testCatchUpWorktrees(t, home) })
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
	laterSecond()
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
	laterSecond()
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
	laterSecond()
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
	laterSecond()
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
	repo, _ := repoWithRemote(t, home, "push")
	agentWrites(t, "push-1", repo, "p.txt", numbered(6, "p %d"))
	sha := commitAll(t, repo, "add p")

	// What the shadow does around `git push`: list first, queue after.
	listed := nonEmptyLines(gitOut(t, repo, UnpushedCommitsArgs()...))
	if len(listed) != 1 || listed[0] != sha {
		t.Fatalf("unpushed before the push: %v, want [%s]", listed, sha)
	}
	gitOut(t, repo, "push", "-q", "origin", "main")
	if err := spool.WriteCommitJob(spool.CommitJob{RepoRoot: repo, SHAs: listed, ReceivedAt: time.Now().UTC().Format(time.RFC3339Nano)}); err != nil {
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
	laterSecond()
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
