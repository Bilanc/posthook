package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// The hook command as an agent runs it: `posthook ingest --agent <slug>` with
// the payload on stdin. For a prompt-submit hook Claude Code adds whatever
// the command prints to the model's context and treats exit code 2 as "block
// this prompt", so the command must print nothing and exit 0, whatever it is
// fed. This builds the real binary: the in-process tests cannot cover the
// command wiring or the worker it starts.
func TestHookCommandPrintsNothingAndExitsZero(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := filepath.Join(t.TempDir(), "posthook")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v: %s", err, out)
	}
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".posthook"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The hook starts a background worker under this scratch home. Stop it
	// when the test is done.
	t.Cleanup(func() {
		data, err := os.ReadFile(filepath.Join(home, ".posthook", "worker.lock"))
		if err != nil {
			return
		}
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 1 {
			_ = syscall.Kill(pid, syscall.SIGTERM)
		}
	})

	payloads := map[string]string{
		"a prompt":       `{"hook_event_name":"UserPromptSubmit","session_id":"hook-cmd","cwd":"` + home + `","prompt":"a prompt"}`,
		"a tool call":    `{"hook_event_name":"PostToolUse","session_id":"hook-cmd","cwd":"` + home + `","tool_name":"Bash","tool_input":{"command":"ls"}}`,
		"not JSON":       `this is not JSON`,
		"nothing at all": ``,
	}
	for name, payload := range payloads {
		cmd := exec.Command(bin, "ingest", "--agent", "claude-code")
		cmd.Env = append(os.Environ(), "HOME="+home, "POSTHOOK_DEBUG=")
		cmd.Stdin = strings.NewReader(payload)
		var stdout bytes.Buffer
		cmd.Stdout = &stdout
		err := cmd.Run()
		if err != nil {
			t.Errorf("%s: the hook command failed: %v", name, err)
		}
		if stdout.Len() != 0 {
			t.Errorf("%s: the hook command printed %q", name, stdout.String())
		}
	}
}
