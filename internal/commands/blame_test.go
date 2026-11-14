package commands

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/bilanc/posthook/internal/ingest"
	"github.com/bilanc/posthook/internal/paths"
	"github.com/bilanc/posthook/internal/spool"
	"github.com/bilanc/posthook/internal/store"
)

// These tests run `posthook blame`'s line matching against real git
// repositories: an agent edits, an engineer changes things by hand, a commit
// is recorded, and each line of the file must come out as the agent's or
// not.

func blameHome(t *testing.T) string {
	t.Helper()
	_ = store.Close()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".gitconfig"))
	t.Setenv("POSTHOOK_BYPASS", "1")
	if err := os.MkdirAll(filepath.Join(home, ".posthook"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return home
}

func blameGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func blameWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// blameRepo makes a repository with a bare remote and one pushed commit.
func blameRepo(t *testing.T, home, name string) (repo, remote string) {
	t.Helper()
	remote = filepath.Join(home, name+".git")
	blameGit(t, home, "init", "-q", "--bare", "--initial-branch=main", remote)
	repo = filepath.Join(home, name)
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	blameGit(t, repo, "init", "-q", "--initial-branch=main")
	blameGit(t, repo, "config", "user.email", "dev@example.com")
	blameGit(t, repo, "config", "user.name", "Dev")
	blameGit(t, repo, "remote", "add", "origin", remote)
	blameWrite(t, filepath.Join(repo, "README.md"), "# "+name+"\n")
	blameGit(t, repo, "add", "-A")
	blameGit(t, repo, "commit", "-q", "-m", "init")
	blameGit(t, repo, "push", "-q", "-u", "origin", "main")
	return repo, remote
}

// agentWrite has a Claude Code session create a file, as a Write of a new
// file reports it.
func agentWrite(t *testing.T, session, repo, rel, content string) {
	t.Helper()
	path := filepath.Join(repo, rel)
	blameWrite(t, path, content)
	raw, err := json.Marshal(map[string]any{
		"hook_event_name": "PostToolUse", "session_id": session, "tool_name": "Write",
		"tool_input":    map[string]any{"file_path": path, "content": content},
		"tool_response": map[string]any{"type": "create", "filePath": path, "structuredPatch": []any{}, "originalFile": nil},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ingest.ProcessAgentEnvelope(spool.Envelope{
		Agent: "claude-code", Cwd: repo, ReceivedAt: time.Now().UTC().Format(time.RFC3339Nano), Payload: raw,
	}); err != nil {
		t.Fatal(err)
	}
}

func commitRecorded(t *testing.T, repo, message string) string {
	t.Helper()
	blameGit(t, repo, "add", "-A")
	blameGit(t, repo, "commit", "-q", "-m", message)
	sha := blameGit(t, repo, "rev-parse", "HEAD")
	if err := ingest.GitCommit(repo, sha); err != nil {
		t.Fatal(err)
	}
	return sha
}

// aiLines runs the blame matching for a file and returns the numbers of the
// lines it says an agent wrote, with the session of each.
func aiLines(t *testing.T, repo, rel string) map[int]string {
	t.Helper()
	root, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "blame", "--porcelain", rel)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git blame: %v", err)
	}
	matches, err := lookupRanges(root, rel, parsePorcelain(string(out)))
	if err != nil {
		t.Fatal(err)
	}
	got := map[int]string{}
	for line, m := range matches {
		got[line] = m.sessionID.String
	}
	return got
}

func wantAILines(t *testing.T, what string, got map[int]string, session string, want ...int) {
	t.Helper()
	var lines []int
	for line, s := range got {
		if s != session {
			t.Errorf("%s: line %d is credited to session %q, want %q", what, line, s, session)
		}
		lines = append(lines, line)
	}
	sort.Ints(lines)
	if len(lines) != len(want) {
		t.Errorf("%s: agent lines %v, want %v", what, lines, want)
		return
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("%s: agent lines %v, want %v", what, lines, want)
			return
		}
	}
}

func readNote(t *testing.T, dir, sha string) noteBody {
	t.Helper()
	cmd := exec.Command("git", "notes", "--ref="+paths.NotesRef, "show", sha)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("no note for %s: %v", sha[:7], err)
	}
	var note noteBody
	if err := json.Unmarshal(out, &note); err != nil {
		t.Fatalf("note is not JSON: %v: %s", err, out)
	}
	return note
}

func TestBlameAfterAHandEdit(t *testing.T) {
	home := blameHome(t)
	repo, _ := blameRepo(t, home, "blame-hand")
	agentWrite(t, "blame-s1", repo, "f.txt", "one\ntwo\nthree\nfour\nfive\n")
	// The engineer adds a line at the top and rewrites the agent's third
	// line before committing.
	blameWrite(t, filepath.Join(repo, "f.txt"), "by hand\none\ntwo\nTHREE, by hand\nfour\nfive\n")
	sha := commitRecorded(t, repo, "add f")

	// The agent's lines are 2, 3, 5 and 6 of the committed file. Its edit
	// recorded them at 1 to 5, which would have marked the hand-written top
	// line and the rewritten one.
	wantAILines(t, "blame", aiLines(t, repo, "f.txt"), "blame-s1", 2, 3, 5, 6)

	// The note says the same, in the commit's line numbers.
	note := readNote(t, repo, sha)
	if note.V != 2 {
		t.Errorf("note version %d, want 2", note.V)
	}
	var ranges []string
	for _, e := range note.Files["f.txt"] {
		ranges = append(ranges, e.Lines)
		if e.Session != "blame-s1" || e.Agent != "claude-code" || e.TS == "" {
			t.Errorf("note entry %+v", e)
		}
	}
	if strings.Join(ranges, ",") != "2-3,5-6" {
		t.Errorf("note ranges %v, want 2-3 and 5-6", ranges)
	}
}

func TestBlameOnATeammatesClone(t *testing.T) {
	home := blameHome(t)
	repo, remote := blameRepo(t, home, "blame-team")
	agentWrite(t, "blame-s2", repo, "f.txt", "one\ntwo\nthree\n")
	blameWrite(t, filepath.Join(repo, "f.txt"), "by hand\none\ntwo\nthree\n")
	sha := commitRecorded(t, repo, "add f")
	blameGit(t, repo, "push", "-q", "origin", "main")
	blameGit(t, repo, "push", "-q", "origin", paths.NotesRef+":"+paths.NotesRef)

	// A teammate's machine: its own posthook database, which has never seen
	// the session or the commit. All it has is the note.
	_ = store.Close()
	mate := t.TempDir()
	t.Setenv("HOME", mate)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(mate, ".config"))
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(mate, ".gitconfig"))
	if err := os.MkdirAll(filepath.Join(mate, ".posthook"), 0o755); err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(mate, "clone")
	blameGit(t, mate, "clone", "-q", remote, clone)
	blameGit(t, clone, "fetch", "-q", "origin", paths.NotesRef+":"+paths.NotesRef)
	if blameGit(t, clone, "rev-parse", "HEAD") != sha {
		t.Fatal("test setup: the clone is not at the commit")
	}
	wantAILines(t, "blame from the note", aiLines(t, clone, "f.txt"), "blame-s2", 2, 3, 4)
}

func TestBlameReadsANoteFromBeforeExactLines(t *testing.T) {
	home := blameHome(t)
	repo, _ := blameRepo(t, home, "blame-v1")
	blameWrite(t, filepath.Join(repo, "f.txt"), "one\ntwo\nthree\nfour\n")
	blameGit(t, repo, "add", "-A")
	blameGit(t, repo, "commit", "-q", "-m", "add f")
	sha := blameGit(t, repo, "rev-parse", "HEAD")
	// A note as earlier releases wrote it: version 1, the ranges of the
	// edits. The commit itself is not in this database.
	v1 := `{"v":1,"commit":"` + sha + `","files":{"f.txt":[{"lines":"2-3","agent":"codex","session":"old-session","model":"gpt-x","ts":"2026-10-01T10:00:00Z"}]}}`
	cmd := exec.Command("git", "notes", "--ref="+paths.NotesRef, "add", "-f", "-F", "-", sha)
	cmd.Dir = repo
	cmd.Stdin = strings.NewReader(v1)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git notes add: %v: %s", err, out)
	}
	wantAILines(t, "blame from a v1 note", aiLines(t, repo, "f.txt"), "old-session", 2, 3)
}

func TestBlameOfUncommittedLines(t *testing.T) {
	home := blameHome(t)
	repo, _ := blameRepo(t, home, "blame-pending")
	agentWrite(t, "blame-s3", repo, "f.txt", "one\ntwo\nthree\n")
	// Tracked but not committed: the engineer has changed the second line
	// and added a fourth since the agent wrote the file.
	blameWrite(t, filepath.Join(repo, "f.txt"), "one\nTWO, by hand\nthree\nfour, by hand\n")
	blameGit(t, repo, "add", "-N", "f.txt")
	wantAILines(t, "blame of a file not yet committed", aiLines(t, repo, "f.txt"), "blame-s3", 1, 3)
}

func TestBlameOfACommitCreditedNothing(t *testing.T) {
	home := blameHome(t)
	repo, _ := blameRepo(t, home, "blame-none")
	agentWrite(t, "blame-s4", repo, "f.txt", "one\ntwo\n")
	// The engineer replaces everything the agent wrote before committing.
	blameWrite(t, filepath.Join(repo, "f.txt"), "all\nby hand\n")
	commitRecorded(t, repo, "add f by hand")
	if got := aiLines(t, repo, "f.txt"); len(got) != 0 {
		t.Errorf("lines %v are shown as the agent's; the commit holds none of its lines", got)
	}
}
