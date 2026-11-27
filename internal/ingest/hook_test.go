package ingest

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bilanc/posthook/internal/spool"
	"github.com/bilanc/posthook/internal/store"
)

// The hook command for a prompt submit runs before the prompt reaches the
// model, and Claude Code adds whatever such a hook prints to the model's
// context. So the hook path must write nothing to standard output, and it
// must not fail: all it does is put the payload in the spool.
func TestThePromptHookPrintsNothing(t *testing.T) {
	isolateIngest(t)
	payload := `{"hook_event_name":"UserPromptSubmit","session_id":"hook-1","cwd":"/tmp","prompt":"a prompt"}`

	stdin, in, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	out, stdout, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	realIn, realOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = stdin, stdout
	t.Cleanup(func() { os.Stdin, os.Stdout = realIn, realOut })
	if _, err := in.WriteString(payload); err != nil {
		t.Fatal(err)
	}
	in.Close()

	hookErr := SpoolAgentEvent("claude-code")
	stdout.Close()
	os.Stdin, os.Stdout = realIn, realOut
	printed, _ := io.ReadAll(out)

	if hookErr != nil {
		t.Errorf("the hook failed: %v", hookErr)
	}
	if len(printed) != 0 {
		t.Errorf("the hook printed %q; a prompt hook's output goes into the model's context", printed)
	}
	if n, err := spool.Pending(); err != nil || n != 1 {
		t.Errorf("%d payload(s) in the spool (err %v), want 1", n, err)
	}
}

// feedStdin runs fn with payload on standard input, as a hook command gets it.
func feedStdin(t *testing.T, payload string, fn func()) {
	t.Helper()
	stdin, in, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	realIn := os.Stdin
	os.Stdin = stdin
	defer func() { os.Stdin = realIn; stdin.Close() }()
	go func() { _, _ = in.WriteString(payload); in.Close() }()
	fn()
}

// POSTHOOK_DISABLE=1 is for agents run by scripts and bots: their events are
// read (so the agent never sees a broken pipe) and thrown away, and the hook
// still prints nothing and succeeds. Only the session's id is remembered.
func TestADisabledAgentEventIsNotSpooled(t *testing.T) {
	isolateIngest(t)
	t.Setenv("POSTHOOK_DISABLE", "1")
	feedStdin(t, `{"hook_event_name":"UserPromptSubmit","session_id":"bot-1","cwd":"/tmp","prompt":"an email"}`, func() {
		if err := SpoolAgentEvent("claude-code"); err != nil {
			t.Errorf("the hook failed: %v", err)
		}
	})
	if n, err := spool.Pending(); err != nil || n != 0 {
		t.Errorf("%d payload(s) in the spool (err %v), want 0", n, err)
	}
	if !sessionDisabled("claude-code", "bot-1") {
		t.Error("the session was not remembered as unrecorded")
	}
	entries, _ := os.ReadDir(disabledSessionsDir())
	for _, e := range entries {
		if data, _ := os.ReadFile(filepath.Join(disabledSessionsDir(), e.Name())); len(data) != 0 {
			t.Errorf("a marker holds %q; it must hold nothing", data)
		}
	}
}

func TestAgentEventsDisabledOnlyForOne(t *testing.T) {
	for value, want := range map[string]bool{"1": true, "": false, "0": false, "true": false} {
		t.Setenv("POSTHOOK_DISABLE", value)
		if got := AgentEventsDisabled(); got != want {
			t.Errorf("POSTHOOK_DISABLE=%q: disabled %v, want %v", value, got, want)
		}
	}
}

// A session that ran with POSTHOOK_DISABLE=1 and is resumed without it is
// still not recorded: its first recorded Stop would otherwise import the
// whole transcript, the unrecorded run's prompts included.
func TestAResumedDisabledSessionIsNotRecorded(t *testing.T) {
	_ = store.Close()
	home := isolateIngest(t)
	t.Cleanup(func() { _ = store.Close() })
	const session = "pi-s1"
	t.Setenv("POSTHOOK_DISABLE", "1")
	feedStdin(t, `{"hook_event_name":"UserPromptSubmit","session_id":"`+session+`","cwd":"`+home+`"}`, func() { DropAgentEvent(piAgentSlug) })
	t.Setenv("POSTHOOK_DISABLE", "")

	transcriptPath := filepath.Join(home, ".pi", "agent", "sessions", "s.jsonl")
	writeFile(t, transcriptPath, piSessionFixture)
	deliver(t, piAgentSlug, home, piEvent("UserPromptSubmit", session, home, nil))
	deliver(t, piAgentSlug, home, piEvent("Stop", session, home, map[string]any{"transcript_path": transcriptPath}))

	db := openDB(t)
	var sessions, prompts, events int
	db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE id = ?`, session).Scan(&sessions)
	db.QueryRow(`SELECT COUNT(*) FROM session_prompts WHERE session_id = ?`, session).Scan(&prompts)
	db.QueryRow(`SELECT COUNT(*) FROM events WHERE session_id = ?`, session).Scan(&events)
	if sessions+prompts+events != 0 {
		t.Errorf("resumed session recorded: %d session(s), %d prompt(s), %d event(s)", sessions, prompts, events)
	}

	// Another session is recorded as usual.
	deliver(t, piAgentSlug, home, piEvent("Stop", "pi-other", home, map[string]any{"transcript_path": transcriptPath}))
	db.QueryRow(`SELECT COUNT(*) FROM session_prompts WHERE session_id = ?`, "pi-other").Scan(&prompts)
	if prompts != 1 {
		t.Errorf("a session that never ran disabled has %d prompt(s), want 1", prompts)
	}
}

// Markers go once they are older than the window, and are kept while their
// session runs.
func TestPruneDisabledSessions(t *testing.T) {
	isolateIngest(t)
	t.Setenv("POSTHOOK_DISABLE", "1")
	for _, id := range []string{"old", "new"} {
		feedStdin(t, `{"hook_event_name":"UserPromptSubmit","session_id":"`+id+`"}`, func() { DropAgentEvent("claude-code") })
	}
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(disabledSessionMarker("claude-code", "old"), past, past); err != nil {
		t.Fatal(err)
	}
	if n := PruneDisabledSessions(24 * time.Hour); n != 1 {
		t.Errorf("pruned %d, want 1", n)
	}
	if sessionDisabled("claude-code", "old") || !sessionDisabled("claude-code", "new") {
		t.Error("pruned the wrong marker")
	}
}
