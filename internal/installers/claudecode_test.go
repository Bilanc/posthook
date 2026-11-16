package installers

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/bilanc/posthook/internal/paths"
)

// claudeCommands returns posthook's command count per hook type in the
// Claude Code settings file.
func claudeCommands(t *testing.T) map[string]int {
	t.Helper()
	var settings map[string]any
	data, err := os.ReadFile(paths.ClaudeSettingsPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	hooks, _ := settings["hooks"].(map[string]any)
	for hookType, blocks := range hooks {
		for _, b := range blocks.([]any) {
			inner, _ := b.(map[string]any)["hooks"].([]any)
			for _, h := range inner {
				if cmd, _ := h.(map[string]any)["command"].(string); IsPosthookCommand(cmd, claudeAgentSlug) {
					counts[hookType]++
				}
			}
		}
	}
	return counts
}

func TestClaudeCodeHooksIncludeThePromptHook(t *testing.T) {
	isolate(t)
	// An install from before the prompt hook: the two older hook types, a
	// retired one, and a hook of the user's own on prompt submit.
	mustWrite(t, paths.ClaudeSettingsPath(), `{
  "hooks": {
    "PostToolUse": [{"matcher": "*", "hooks": [{"type": "command", "command": "/old/posthook ingest --agent claude-code"}]}],
    "Stop": [{"matcher": "*", "hooks": [{"type": "command", "command": "/old/posthook ingest --agent claude-code"}]}],
    "PreToolUse": [{"matcher": "*", "hooks": [{"type": "command", "command": "/old/posthook ingest --agent claude-code"}]}],
    "UserPromptSubmit": [{"hooks": [{"type": "command", "command": "echo mine"}]}]
  }
}`)
	res, err := InstallClaudeCodeHooks(testBin)
	if err != nil || !res.Changed {
		t.Fatalf("install: changed=%v err=%v", res.Changed, err)
	}
	got := claudeCommands(t)
	// One command per event posthook needs: tool calls, the end of a turn,
	// and the start of one. Nothing on the retired event.
	want := map[string]int{"PostToolUse": 1, "Stop": 1, "UserPromptSubmit": 1}
	if len(got) != len(want) {
		t.Fatalf("posthook commands by hook type: %v, want %v", got, want)
	}
	for hookType, n := range want {
		if got[hookType] != n {
			t.Errorf("%s: %d posthook command(s), want %d", hookType, got[hookType], n)
		}
	}
	settings := readFile(t, paths.ClaudeSettingsPath())
	if !strings.Contains(settings, "echo mine") {
		t.Error("the user's own prompt hook was removed")
	}
	if strings.Contains(settings, "/old/posthook") {
		t.Error("a command with the old binary path is left")
	}

	// Installing again changes nothing.
	if res, err := InstallClaudeCodeHooks(testBin); err != nil || res.Changed {
		t.Errorf("second install: changed=%v err=%v", res.Changed, err)
	}

	// Uninstall takes the prompt hook out with the others and leaves the
	// user's.
	if _, err := UninstallClaudeCodeHooks(); err != nil {
		t.Fatal(err)
	}
	if left := claudeCommands(t); len(left) != 0 {
		t.Errorf("posthook commands left after uninstall: %v", left)
	}
	if !strings.Contains(readFile(t, paths.ClaudeSettingsPath()), "echo mine") {
		t.Error("uninstall removed the user's own prompt hook")
	}
}
