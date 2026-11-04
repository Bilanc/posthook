package installers

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bilanc/posthook/internal/paths"
)

func copilotEntries(t *testing.T, m map[string]any, hookType string) []map[string]any {
	t.Helper()
	hooks, _ := m["hooks"].(map[string]any)
	arr, _ := hooks[hookType].([]any)
	var out []map[string]any
	for _, h := range arr {
		hm, ok := h.(map[string]any)
		if !ok {
			t.Fatalf("%s: non-object entry %v", hookType, h)
		}
		out = append(out, hm)
	}
	return out
}

func TestInstallCopilotHooksFreshMachine(t *testing.T) {
	isolate(t)
	t.Setenv("COPILOT_HOME", "")
	res, err := InstallCopilotHooks(testBin)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Fatal("expected first install to write the file")
	}
	got := readJSON(t, paths.CopilotHooksPath())
	if v, _ := got["version"].(float64); v != 1 {
		t.Errorf("version = %v, want 1", got["version"])
	}
	hooks, _ := got["hooks"].(map[string]any)
	if len(hooks) != len(copilotHookTypes) {
		t.Errorf("registered events = %v, want %v", hooks, copilotHookTypes)
	}
	for _, hookType := range copilotHookTypes {
		entries := copilotEntries(t, got, hookType)
		if len(entries) != 1 {
			t.Fatalf("%s: %d entries, want 1", hookType, len(entries))
		}
		e := entries[0]
		if e["type"] != "command" {
			t.Errorf("%s: type = %v", hookType, e["type"])
		}
		if e["command"] != testBin+" ingest --agent github-copilot" {
			t.Errorf("%s: command = %v", hookType, e["command"])
		}
		if ts, _ := e["timeoutSec"].(float64); ts != copilotHookTimeoutSec {
			t.Errorf("%s: timeoutSec = %v", hookType, e["timeoutSec"])
		}
	}

	again, err := InstallCopilotHooks(testBin)
	if err != nil {
		t.Fatal(err)
	}
	if again.Changed {
		t.Error("second install should be a no-op")
	}
}

func TestInstallCopilotHooksHonoursCopilotHome(t *testing.T) {
	home := isolate(t)
	custom := filepath.Join(home, "elsewhere")
	t.Setenv("COPILOT_HOME", custom)
	if _, err := InstallCopilotHooks(testBin); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(custom, "hooks", "posthook.json")); err != nil {
		t.Fatalf("expected hooks file under COPILOT_HOME: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".copilot")); !os.IsNotExist(err) {
		t.Error("~/.copilot should not be created when COPILOT_HOME points elsewhere")
	}
}

func TestInstallCopilotHooksPreservesUserEntriesAndDedups(t *testing.T) {
	isolate(t)
	t.Setenv("COPILOT_HOME", "")
	mustWrite(t, paths.CopilotHooksPath(), `{
  "version": 1,
  "disableAllHooks": false,
  "hooks": {
    "PostToolUse": [
      {"type": "command", "bash": "echo keep-me"},
      {"type": "command", "command": "/old/path/posthook ingest --agent github-copilot"},
      {"type": "command", "bash": "/old/path/posthook ingest --agent github-copilot"}
    ],
    "PreToolUse": [
      {"type": "command", "command": "/old/path/posthook ingest --agent github-copilot"},
      {"type": "command", "command": "echo user-pre"}
    ],
    "sessionStart": [
      {"type": "command", "command": "/old/path/posthook ingest --agent github-copilot"}
    ]
  }
}`)
	res, err := InstallCopilotHooks(testBin)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Fatal("expected a rewrite")
	}
	got := readJSON(t, paths.CopilotHooksPath())
	if got["disableAllHooks"] != false {
		t.Error("unrelated top-level keys must survive")
	}

	post := copilotEntries(t, got, "PostToolUse")
	if len(post) != 2 {
		t.Fatalf("PostToolUse entries = %d, want 2 (user + ours): %v", len(post), post)
	}
	if post[0]["bash"] != "echo keep-me" {
		t.Errorf("user entry must stay first: %v", post[0])
	}
	if post[1]["command"] != testBin+" ingest --agent github-copilot" {
		t.Errorf("our entry must be refreshed to the new binary path: %v", post[1])
	}

	// PreToolUse is no longer registered: our copy goes, the user's stays.
	pre := copilotEntries(t, got, "PreToolUse")
	if len(pre) != 1 || pre[0]["command"] != "echo user-pre" {
		t.Errorf("PreToolUse = %v, want only the user entry", pre)
	}
	// sessionStart held only our stale entry, so the event disappears.
	if _, ok := got["hooks"].(map[string]any)["sessionStart"]; ok {
		t.Error("sessionStart should be dropped once our stale entry is removed")
	}
	if len(copilotEntries(t, got, "Stop")) != 1 {
		t.Error("Stop should be registered")
	}
}

func TestUninstallCopilotHooks(t *testing.T) {
	isolate(t)
	t.Setenv("COPILOT_HOME", "")
	path := paths.CopilotHooksPath()

	// Missing file.
	res, err := UninstallCopilotHooks()
	if err != nil || res.Changed {
		t.Fatalf("missing file should be a no-op: %+v %v", res, err)
	}

	// File we created ourselves: deleted outright.
	if _, err := InstallCopilotHooks(testBin); err != nil {
		t.Fatal(err)
	}
	res, err = UninstallCopilotHooks()
	if err != nil || !res.Changed {
		t.Fatalf("uninstall after install: %+v %v", res, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("file posthook created should be deleted on uninstall")
	}

	// File carrying someone else's entry: rewritten without ours.
	mustWrite(t, path, `{"version":1,"hooks":{"PostToolUse":[{"type":"command","command":"echo keep"},{"type":"command","command":"/x/posthook ingest --agent github-copilot"}],"Stop":[{"type":"command","command":"/x/posthook ingest --agent github-copilot"}]}}`)
	res, err = UninstallCopilotHooks()
	if err != nil || !res.Changed {
		t.Fatalf("uninstall with user entry: %+v %v", res, err)
	}
	got := readJSON(t, path)
	post := copilotEntries(t, got, "PostToolUse")
	if len(post) != 1 || post[0]["command"] != "echo keep" {
		t.Errorf("PostToolUse = %v, want only the user entry", post)
	}
	if _, ok := got["hooks"].(map[string]any)["Stop"]; ok {
		t.Error("Stop held only our entry and should be dropped")
	}

	// Nothing of ours present: untouched.
	res, err = UninstallCopilotHooks()
	if err != nil || res.Changed {
		t.Fatalf("second uninstall should be a no-op: %+v %v", res, err)
	}
}

func TestDetectCopilot(t *testing.T) {
	home := isolate(t)
	t.Setenv("COPILOT_HOME", "")
	if DetectCopilot() {
		t.Fatal("fresh home should not detect Copilot")
	}
	if err := os.MkdirAll(filepath.Join(home, ".copilot"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !DetectCopilot() {
		t.Error("~/.copilot should detect Copilot CLI")
	}
}
