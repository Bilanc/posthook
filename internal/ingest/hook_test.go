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
