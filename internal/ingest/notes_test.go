package ingest

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bilanc/posthook/internal/paths"
	"github.com/bilanc/posthook/internal/store"
)

// The git note of a commit, as written by attribution.

type writtenNote struct {
	V     int                    `json:"v"`
	Files map[string][]noteEntry `json:"files"`
}

func noteOf(t *testing.T, repo, sha string) writtenNote {
	t.Helper()
	cmd := exec.Command("git", "notes", "--ref="+paths.NotesRef, "show", sha)
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("no note for %s: %v", sha[:7], err)
	}
	var note writtenNote
	if err := json.Unmarshal(out, &note); err != nil {
		t.Fatalf("note is not JSON: %v: %s", err, out)
	}
	return note
}

func rangesIn(note writtenNote, file string) string {
	var out []string
	for _, e := range note.Files[file] {
		out = append(out, e.Lines)
	}
	return strings.Join(out, ",")
}

func TestCommitNotes(t *testing.T) {
	_ = store.Close()
	home := isolateIngest(t)
	t.Cleanup(func() { _ = store.Close() })
	openDB(t)

	t.Run("a note lists the credited lines in the commit's line numbers", func(t *testing.T) {
		repo, path, content := seededRepo(t, home, "note-exact", "cfg.ini", 10)
		after := agentInserts(t, "note-exact-1", repo, path, content, 5, "a1 = new", "a2 = new", "a3 = new")
		// By hand: two lines on top, and the agent's middle line rewritten.
		byHand := "top1 = by hand\ntop2 = by hand\n" + strings.Replace(after, "a2 = new\n", "a2 = by hand\n", 1)
		writeFile(t, path, byHand)
		sha := commitAll(t, repo, "agent change, touched up")
		if err := GitCommit(repo, sha); err != nil {
			t.Fatal(err)
		}
		note := noteOf(t, repo, sha)
		// The edit was recorded at lines 6 to 8. In the commit the agent's
		// two surviving lines are 8 and 10.
		if note.V != noteV2 || rangesIn(note, "cfg.ini") != "8,10" {
			t.Errorf("note v%d ranges %q, want v2 and 8,10", note.V, rangesIn(note, "cfg.ini"))
		}
		for _, e := range note.Files["cfg.ini"] {
			if e.Session != "note-exact-1" || e.Agent != "claude-code" || e.TS == "" {
				t.Errorf("note entry %+v", e)
			}
		}
	})

	t.Run("a file matched by position keeps the older kind of note", func(t *testing.T) {
		repo, path, content := seededRepo(t, home, "note-position", "cfg.ini", 10)
		agentInserts(t, "note-position-1", repo, path, content, 5, "a1 = new", "a2 = new", "a3 = new")
		forgetAuthorship(t, path)
		sha := commitAll(t, repo, "agent change")
		if err := GitCommit(repo, sha); err != nil {
			t.Fatal(err)
		}
		note := noteOf(t, repo, sha)
		if note.V != noteV1 || rangesIn(note, "cfg.ini") != "6-8" {
			t.Errorf("note v%d ranges %q, want v1 and the edit's range 6-8", note.V, rangesIn(note, "cfg.ini"))
		}
	})

	t.Run("a rebased commit gets a note in its own line numbers", func(t *testing.T) {
		repo, _ := repoWithRemote(t, home, "note-rebase")
		path := filepath.Join(repo, "cfg.ini")
		content := numbered(10, "k%d = old")
		writeFile(t, path, content)
		commitAll(t, repo, "add cfg")
		gitOut(t, repo, "push", "-q", "origin", "main")
		gitOut(t, repo, "checkout", "-q", "-b", "feature")
		agentInserts(t, "note-rebase-1", repo, path, content, 8, "a1 = new", "a2 = new", "a3 = new")
		original := commitAll(t, repo, "agent change")
		if err := GitCommit(repo, original); err != nil {
			t.Fatal(err)
		}
		if got := rangesIn(noteOf(t, repo, original), "cfg.ini"); got != "9-11" {
			t.Fatalf("the original's note ranges %q, want 9-11", got)
		}
		gitOut(t, repo, "checkout", "-q", "main")
		writeFile(t, path, "top1 = by hand\ntop2 = by hand\n"+content)
		commitAll(t, repo, "main moves")
		gitOut(t, repo, "checkout", "-q", "feature")
		gitOut(t, repo, "rebase", "-q", "main")
		rebased := gitOut(t, repo, "rev-parse", "HEAD")

		sweep(t, repo)
		note := noteOf(t, repo, rebased)
		if note.V != noteV2 || rangesIn(note, "cfg.ini") != "11-13" {
			t.Errorf("the rebased commit's note v%d ranges %q, want v2 and 11-13", note.V, rangesIn(note, "cfg.ini"))
		}
	})

	t.Run("two sessions in one file are listed apart, in line order", func(t *testing.T) {
		repo, path, content := seededRepo(t, home, "note-two", "cfg.ini", 10)
		after := agentInserts(t, "note-two-a", repo, path, content, 5, "a1 = new", "a2 = new")
		agentInserts(t, "note-two-b", repo, path, after, 0, "top = new")
		sha := commitAll(t, repo, "two sessions")
		if err := GitCommit(repo, sha); err != nil {
			t.Fatal(err)
		}
		note := noteOf(t, repo, sha)
		entries := note.Files["cfg.ini"]
		if len(entries) != 2 || entries[0].Lines != "1" || entries[0].Session != "note-two-b" ||
			entries[1].Lines != "7-8" || entries[1].Session != "note-two-a" {
			t.Errorf("note entries %+v", entries)
		}
	})
}
