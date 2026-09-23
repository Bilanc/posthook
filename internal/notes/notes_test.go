package notes

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bilanc/posthook/internal/paths"
)

// git runs real git in dir and fails the test on error.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"POSTHOOK_BYPASS=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// setup returns a bare remote and two clones of it, with one commit on main.
func setup(t *testing.T) (remote, a, b string) {
	t.Helper()
	root := t.TempDir()
	remote = filepath.Join(root, "remote.git")
	a = filepath.Join(root, "a")
	b = filepath.Join(root, "b")
	git(t, root, "init", "-q", "--bare", "-b", "main", remote)
	git(t, root, "clone", "-q", remote, a)
	git(t, a, "commit", "-q", "--allow-empty", "-m", "one")
	git(t, a, "push", "-q", "origin", "HEAD:main")
	git(t, root, "clone", "-q", remote, b)
	return remote, a, b
}

func addNote(t *testing.T, dir, rev, body string) {
	t.Helper()
	git(t, dir, "notes", "--ref="+paths.NotesRef, "add", "-f", "-m", body, rev)
}

func showNote(t *testing.T, dir, rev string) string {
	t.Helper()
	cmd := exec.Command("git", "notes", "--ref="+paths.NotesRef, "show", rev)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "POSTHOOK_BYPASS=1")
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}

func TestPushThenFetchCarriesNotesToASecondClone(t *testing.T) {
	_, a, b := setup(t)
	sha := git(t, a, "rev-parse", "HEAD")
	addNote(t, a, "HEAD", `{"from":"a"}`)

	if err := Push(a, "origin"); err != nil {
		t.Fatalf("push: %v", err)
	}
	if err := Fetch(b, "origin"); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got := showNote(t, b, sha); got != `{"from":"a"}` {
		t.Fatalf("clone b should see a's note, got %q", got)
	}
}

func TestPushWithNoLocalNotesIsANoop(t *testing.T) {
	_, a, _ := setup(t)
	if err := Push(a, "origin"); err != nil {
		t.Fatalf("push with no notes should not error: %v", err)
	}
	if RefExists(a, paths.NotesRef) {
		t.Fatal("push must not create a notes ref")
	}
}

func TestFetchWhenRemoteHasNoNotesReturnsErrorButLeavesRepoClean(t *testing.T) {
	_, _, b := setup(t)
	if err := Fetch(b, "origin"); err == nil {
		t.Fatal("expected an error when the remote has no notes ref")
	}
	if RefExists(b, paths.NotesRef) || RefExists(b, TrackingRef) {
		t.Fatal("no refs should have been created")
	}
}

func TestDivergedNotesMergeAndBothSidesPushFastForward(t *testing.T) {
	_, a, b := setup(t)
	shaOne := git(t, a, "rev-parse", "HEAD")
	addNote(t, a, "HEAD", `{"from":"a"}`)
	if err := Push(a, "origin"); err != nil {
		t.Fatalf("push a: %v", err)
	}

	// b commits and notes its own commit without ever fetching a's notes.
	git(t, b, "pull", "-q", "origin", "main")
	git(t, b, "commit", "-q", "--allow-empty", "-m", "two")
	shaTwo := git(t, b, "rev-parse", "HEAD")
	addNote(t, b, "HEAD", `{"from":"b"}`)
	git(t, b, "push", "-q", "origin", "HEAD:main")

	if err := Push(b, "origin"); err != nil {
		t.Fatalf("push b (diverged): %v", err)
	}
	if got := showNote(t, b, shaOne); got != `{"from":"a"}` {
		t.Fatalf("b should have merged a's note in before pushing, got %q", got)
	}

	// a now fetches and must see both.
	git(t, a, "pull", "-q", "origin", "main")
	if err := Fetch(a, "origin"); err != nil {
		t.Fatalf("fetch a: %v", err)
	}
	if got := showNote(t, a, shaTwo); got != `{"from":"b"}` {
		t.Fatalf("a should see b's note, got %q", got)
	}
	if got := showNote(t, a, shaOne); got != `{"from":"a"}` {
		t.Fatalf("a's own note must survive, got %q", got)
	}
}

func TestSameCommitConflictKeepsLocalNoteAndLeavesNoMergeState(t *testing.T) {
	_, a, b := setup(t)
	sha := git(t, a, "rev-parse", "HEAD")
	addNote(t, a, "HEAD", `{"from":"a"}`)
	if err := Push(a, "origin"); err != nil {
		t.Fatalf("push a: %v", err)
	}
	addNote(t, b, "HEAD", `{"from":"b"}`)
	if err := Push(b, "origin"); err != nil {
		t.Fatalf("push b: %v", err)
	}
	if got := showNote(t, b, sha); got != `{"from":"b"}` {
		t.Fatalf("local note should win a same-commit conflict, got %q", got)
	}
	if _, err := os.Stat(filepath.Join(b, ".git", "NOTES_MERGE_WORKTREE")); err == nil {
		t.Fatal("a notes merge worktree was left behind")
	}
}

func TestEnsureRefspecAddsTrackingFetchAndRemovesLegacy(t *testing.T) {
	_, a, _ := setup(t)
	git(t, a, "config", "--add", "remote.origin.fetch", legacyRefspec)
	git(t, a, "config", "--add", "remote.origin.push", legacyRefspec)

	if !EnsureRefspec(a, "origin") {
		t.Fatal("expected config to change on first run")
	}
	if EnsureRefspec(a, "origin") {
		t.Fatal("second run must be a no-op")
	}
	fetch := strings.Split(git(t, a, "config", "--get-all", "remote.origin.fetch"), "\n")
	if !containsLine(fetch, FetchRefspec) {
		t.Fatalf("tracking fetch refspec missing: %q", fetch)
	}
	if containsLine(fetch, legacyRefspec) {
		t.Fatalf("legacy fetch refspec still present: %q", fetch)
	}
	cmd := exec.Command("git", "config", "--get-all", "remote.origin.push")
	cmd.Dir = a
	if out, _ := cmd.Output(); strings.TrimSpace(string(out)) != "" {
		t.Fatalf("legacy push refspec still present: %q", out)
	}

	// With the refspec in place, a plain `git fetch` brings notes down into
	// the tracking ref and MergeTracking makes them visible.
	sha := git(t, a, "rev-parse", "HEAD")
	git(t, a, "notes", "--ref="+paths.NotesRef, "add", "-m", `{"x":1}`, "HEAD")
	git(t, a, "push", "-q", "origin", paths.NotesRef+":"+paths.NotesRef)
	git(t, a, "update-ref", "-d", paths.NotesRef)
	git(t, a, "fetch", "-q", "origin")
	if !RefExists(a, TrackingRef) {
		t.Fatal("plain fetch should have populated the tracking ref")
	}
	if err := MergeTracking(a); err != nil {
		t.Fatalf("merge tracking: %v", err)
	}
	if got := showNote(t, a, sha); got != `{"x":1}` {
		t.Fatalf("note not visible after MergeTracking, got %q", got)
	}
}

func TestEnsureRefspecIsNoopWithoutRemote(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	if EnsureRefspec(dir, "origin") {
		t.Fatal("must not touch config when the remote does not exist")
	}
}

func containsLine(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}
