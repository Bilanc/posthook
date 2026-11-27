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

// A marked session resumed without the variable is dropped by the hook,
// before its prompt reaches the spool.
func TestAResumedDisabledSessionIsDroppedBeforeTheSpool(t *testing.T) {
	isolateIngest(t)
	t.Setenv("POSTHOOK_DISABLE", "1")
	feedStdin(t, `{"hook_event_name":"UserPromptSubmit","session_id":"bot-2"}`, func() { DropAgentEvent("claude-code") })
	t.Setenv("POSTHOOK_DISABLE", "")
	feedStdin(t, `{"hook_event_name":"UserPromptSubmit","session_id":"bot-2","prompt":"a resumed prompt"}`, func() {
		if err := SpoolAgentEvent("claude-code"); err != nil {
			t.Errorf("the hook failed: %v", err)
		}
	})
	if n, err := spool.Pending(); err != nil || n != 0 {
		t.Errorf("%d payload(s) in the spool (err %v), want 0", n, err)
	}

	// Other sessions are spooled as usual.
	feedStdin(t, `{"hook_event_name":"UserPromptSubmit","session_id":"someone"}`, func() { _ = SpoolAgentEvent("claude-code") })
	if n, _ := spool.Pending(); n != 1 {
		t.Errorf("%d payload(s) in the spool for an unmarked session, want 1", n)
	}
}

// However long ago it ran, a disabled session stays unrecorded: its
// transcript can outlive any retention window, and resuming it would import
// the unrecorded prompts.
func TestAnOldDisabledSessionStaysUnrecorded(t *testing.T) {
	_ = store.Close()
	home := isolateIngest(t)
	t.Cleanup(func() { _ = store.Close() })
	t.Setenv("POSTHOOK_RETENTION_DAYS", "1")
	const session = "pi-old"
	transcriptPath := filepath.Join(home, ".pi", "agent", "sessions", "old.jsonl")
	writeFile(t, transcriptPath, piSessionFixture)
	t.Setenv("POSTHOOK_DISABLE", "1")
	feedStdin(t, `{"hook_event_name":"UserPromptSubmit","session_id":"`+session+`"}`, func() { DropAgentEvent(piAgentSlug) })
	t.Setenv("POSTHOOK_DISABLE", "")
	marker := disabledSessionMarker(piAgentSlug, session)
	past := time.Now().Add(-400 * 24 * time.Hour)
	if err := os.Chtimes(marker, past, past); err != nil {
		t.Fatal(err)
	}

	// Resumed long after, through the hook and through the worker.
	stop := `{"hook_event_name":"Stop","session_id":"` + session + `","cwd":"` + home + `","transcript_path":"` + transcriptPath + `"}`
	feedStdin(t, stop, func() { _ = SpoolAgentEvent(piAgentSlug) })
	if n, _ := spool.Pending(); n != 0 {
		t.Errorf("%d payload(s) spooled for the resumed session, want 0", n)
	}
	deliver(t, piAgentSlug, home, piEvent("Stop", session, home, map[string]any{"transcript_path": transcriptPath}))
	db := openDB(t)
	var prompts, sessions int
	db.QueryRow(`SELECT COUNT(*) FROM session_prompts WHERE session_id = ?`, session).Scan(&prompts)
	db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE id = ?`, session).Scan(&sessions)
	if prompts+sessions != 0 {
		t.Errorf("resumed old session recorded: %d session(s), %d prompt(s)", sessions, prompts)
	}
	if info, err := os.Stat(marker); err != nil || info.Size() != 0 || info.Mode().Perm() != 0o600 {
		t.Errorf("marker should be an empty owner-only file: %v %v", info, err)
	}
}

// Nothing is left for the worker by an event that is not recorded, except at
// a turn's end in a repository.
func TestHandleAgentHookWakesOnlyWhenThereIsWork(t *testing.T) {
	isolateIngest(t)
	hook := func(payload string) bool {
		var wake bool
		feedStdin(t, payload, func() { wake, _ = HandleAgentHook("claude-code") })
		return wake
	}
	t.Setenv("POSTHOOK_DISABLE", "1")
	if hook(`{"hook_event_name":"UserPromptSubmit","session_id":"w"}`) {
		t.Error("a disabled prompt woke the worker")
	}
	t.Setenv("POSTHOOK_DISABLE", "")
	if hook(`{"hook_event_name":"UserPromptSubmit","session_id":"w","prompt":"resumed"}`) {
		t.Error("a resumed disabled session's prompt woke the worker")
	}
	if !hook(`{"hook_event_name":"UserPromptSubmit","session_id":"someone"}`) {
		t.Error("a recorded event did not wake the worker")
	}
}
