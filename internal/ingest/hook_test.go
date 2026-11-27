package ingest

import (
	"io"
	"os"
	"testing"

	"github.com/bilanc/posthook/internal/spool"
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

// POSTHOOK_DISABLE=1 is for agents run by scripts and bots: their events are
// read (so the agent never sees a broken pipe) and thrown away, and the hook
// still prints nothing and succeeds.
func TestADisabledAgentEventIsNotSpooled(t *testing.T) {
	isolateIngest(t)
	t.Setenv("POSTHOOK_DISABLE", "1")
	stdin, in, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	realIn := os.Stdin
	os.Stdin = stdin
	t.Cleanup(func() { os.Stdin = realIn })
	if _, err := in.WriteString(`{"hook_event_name":"UserPromptSubmit","session_id":"bot-1","cwd":"/tmp","prompt":"an email"}`); err != nil {
		t.Fatal(err)
	}
	in.Close()

	if err := SpoolAgentEvent("claude-code"); err != nil {
		t.Errorf("the hook failed: %v", err)
	}
	if n, err := spool.Pending(); err != nil || n != 0 {
		t.Errorf("%d payload(s) in the spool (err %v), want 0", n, err)
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
