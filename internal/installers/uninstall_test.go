package installers

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bilanc/posthook/internal/paths"

	"github.com/pelletier/go-toml/v2"
)

const testBin = "/opt/bin/posthook"

// isolate points HOME (and git's global config) at a scratch dir so the
// installers never touch the developer's real agent configs.
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".gitconfig"))
	t.Setenv("POSTHOOK_BIN", "")
	return home
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(readFile(t, path)), &m); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return m
}

func readTOML(t *testing.T, path string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := toml.Unmarshal([]byte(readFile(t, path)), &m); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return m
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "POSTHOOK_BYPASS=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// --- Claude Code -------------------------------------------------------------

func TestUninstallClaudeCodeHooksKeepsUserHooks(t *testing.T) {
	isolate(t)
	path := paths.ClaudeSettingsPath()
	mustWrite(t, path, `{
	  "model": "opus",
	  "hooks": {
	    "PostToolUse": [{"matcher": "Edit", "hooks": [{"type": "command", "command": "echo user"}]}],
	    "SessionStart": [{"matcher": "*", "hooks": [{"type": "command", "command": "/old/posthook ingest --agent claude-code"}]}]
	  }
	}`)
	if _, err := InstallClaudeCodeHooks(testBin); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readFile(t, path), paths.PosthookMarker) {
		t.Fatal("install should have written our hook")
	}

	res, err := UninstallClaudeCodeHooks()
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Fatal("expected uninstall to change the file")
	}
	if strings.Contains(readFile(t, path), paths.PosthookMarker) {
		t.Fatalf("posthook command still present:\n%s", readFile(t, path))
	}
	after := readJSON(t, path)
	if after["model"] != "opus" {
		t.Fatalf("unrelated setting lost: %v", after)
	}
	hooks := after["hooks"].(map[string]any)
	ptu, _ := hooks["PostToolUse"].([]any)
	if len(ptu) != 1 || ptu[0].(map[string]any)["matcher"] != "Edit" {
		t.Fatalf("user PostToolUse hook not preserved: %v", hooks["PostToolUse"])
	}
	for _, gone := range []string{"Stop", "SessionStart"} {
		if _, ok := hooks[gone]; ok {
			t.Fatalf("%s should be dropped once only our hook was in it: %v", gone, hooks[gone])
		}
	}

	res, err = UninstallClaudeCodeHooks()
	if err != nil || res.Changed {
		t.Fatalf("second uninstall should be a no-op: changed=%v err=%v", res.Changed, err)
	}
}

func TestUninstallClaudeCodeHooksDropsHooksKeyWhenOnlyOurs(t *testing.T) {
	isolate(t)
	path := paths.ClaudeSettingsPath()
	mustWrite(t, path, `{"model": "opus"}`)
	if _, err := InstallClaudeCodeHooks(testBin); err != nil {
		t.Fatal(err)
	}
	if _, err := UninstallClaudeCodeHooks(); err != nil {
		t.Fatal(err)
	}
	after := readJSON(t, path)
	if _, ok := after["hooks"]; ok {
		t.Fatalf("hooks key should be gone: %v", after)
	}
	if after["model"] != "opus" {
		t.Fatalf("unrelated setting lost: %v", after)
	}
}

func TestUninstallClaudeCodeHooksMissingFile(t *testing.T) {
	isolate(t)
	res, err := UninstallClaudeCodeHooks()
	if err != nil || res.Changed {
		t.Fatalf("missing file should be a no-op: changed=%v err=%v", res.Changed, err)
	}
	if _, err := os.Stat(paths.ClaudeSettingsPath()); !os.IsNotExist(err) {
		t.Fatal("uninstall must not create the settings file")
	}
}

// --- Cursor ------------------------------------------------------------------

func TestUninstallCursorHooksDeletesTheFileItCreated(t *testing.T) {
	isolate(t)
	path := paths.CursorHooksPath()
	if _, err := InstallCursorHooks(testBin); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("install should have created hooks.json")
	}
	res, err := UninstallCursorHooks()
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Fatal("expected a change")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("hooks.json should be deleted when nothing but version remains:\n%s", readFile(t, path))
	}
}

func TestUninstallCursorHooksKeepsUserHooks(t *testing.T) {
	isolate(t)
	path := paths.CursorHooksPath()
	mustWrite(t, path, `{"version": 1, "hooks": {"postToolUse": [{"command": "echo user"}], "preToolUse": [{"command": "/old/posthook ingest --agent cursor"}]}}`)
	if _, err := InstallCursorHooks(testBin); err != nil {
		t.Fatal(err)
	}
	if _, err := UninstallCursorHooks(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(readFile(t, path), paths.PosthookMarker) {
		t.Fatalf("posthook command still present:\n%s", readFile(t, path))
	}
	after := readJSON(t, path)
	if after["version"] != float64(1) {
		t.Fatalf("version lost: %v", after)
	}
	hooks := after["hooks"].(map[string]any)
	if ptu, _ := hooks["postToolUse"].([]any); len(ptu) != 1 || ptu[0].(map[string]any)["command"] != "echo user" {
		t.Fatalf("user hook not preserved: %v", hooks)
	}
	for _, gone := range []string{"beforeSubmitPrompt", "afterFileEdit", "preToolUse"} {
		if _, ok := hooks[gone]; ok {
			t.Fatalf("%s should be dropped: %v", gone, hooks[gone])
		}
	}
}

// --- Codex -------------------------------------------------------------------

func TestUninstallCodexHooksRemovesEverythingItAdded(t *testing.T) {
	isolate(t)
	path := paths.CodexConfigPath()
	mustWrite(t, path, "model = \"o3\"\n")
	if _, err := InstallCodexHooks(testBin); err != nil {
		t.Fatal(err)
	}
	before := readTOML(t, path)
	if _, ok := before["hooks"]; !ok {
		t.Fatal("install should have written hooks")
	}

	res, err := UninstallCodexHooks()
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Fatal("expected a change")
	}
	after := readTOML(t, path)
	for _, key := range []string{"hooks", "features", "sandbox_workspace_write"} {
		if v, ok := after[key]; ok {
			t.Fatalf("%s should be gone, got %v", key, v)
		}
	}
	if after["model"] != "o3" {
		t.Fatalf("unrelated setting lost: %v", after)
	}
}

func TestUninstallCodexHooksKeepsUserHooksTrustAndFeatures(t *testing.T) {
	isolate(t)
	path := paths.CodexConfigPath()
	userState := path + ":post_tool_use:0:0"
	initial := map[string]any{
		"features":                map[string]any{"hooks": true, "other": true},
		"sandbox_workspace_write": map[string]any{"writable_roots": []any{"/tmp/x"}},
		"hooks": map[string]any{
			"PostToolUse": []any{
				map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "echo user"}}},
			},
			"state": map[string]any{
				userState: map[string]any{"enabled": true, "trusted_hash": "sha256:user"},
			},
		},
	}
	buf, err := toml.Marshal(initial)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, path, string(buf))
	if _, err := InstallCodexHooks(testBin); err != nil {
		t.Fatal(err)
	}
	// Sanity: install added a trust entry for our handler next to the user's.
	if state := readTOML(t, path)["hooks"].(map[string]any)["state"].(map[string]any); len(state) < 2 {
		t.Fatalf("install should have added trust entries: %v", state)
	}

	if _, err := UninstallCodexHooks(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(readFile(t, path), paths.PosthookMarker) {
		t.Fatalf("posthook command still present:\n%s", readFile(t, path))
	}
	after := readTOML(t, path)
	hooks := after["hooks"].(map[string]any)
	ptu := hooks["PostToolUse"].([]any)
	inner := ptu[0].(map[string]any)["hooks"].([]any)
	if len(ptu) != 1 || len(inner) != 1 || inner[0].(map[string]any)["command"] != "echo user" {
		t.Fatalf("user hook not preserved: %v", hooks["PostToolUse"])
	}
	if _, ok := hooks["Stop"]; ok {
		t.Fatalf("Stop should be dropped: %v", hooks["Stop"])
	}
	state := hooks["state"].(map[string]any)
	if len(state) != 1 {
		t.Fatalf("only the user's trust entry should remain: %v", state)
	}
	if _, ok := state[userState]; !ok {
		t.Fatalf("user's trust entry lost: %v", state)
	}
	features := after["features"].(map[string]any)
	if features["hooks"] != true || features["other"] != true {
		t.Fatalf("features must stay while user hooks remain: %v", features)
	}
	roots := after["sandbox_workspace_write"].(map[string]any)["writable_roots"].([]any)
	if len(roots) != 1 || roots[0] != "/tmp/x" {
		t.Fatalf("user writable root lost: %v", roots)
	}
}

// --- Git hooks / template ----------------------------------------------------

func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q", dir)
	return dir
}

func TestRemoveRepoHookRemovesAnyPosthookVersion(t *testing.T) {
	isolate(t)
	for _, marker := range []string{"# posthook v1", "# posthook v2", hookMarker} {
		repo := initRepo(t)
		hook := filepath.Join(repo, ".git", "hooks", "post-commit")
		mustWrite(t, hook, "#!/bin/sh\n"+marker+"\n/old/posthook ingest || true\n")
		removed, err := RemoveRepoHook(repo)
		if err != nil {
			t.Fatal(err)
		}
		if !removed {
			t.Fatalf("%q: hook should be recognised as ours", marker)
		}
		if _, err := os.Stat(hook); !os.IsNotExist(err) {
			t.Fatalf("%q: hook still exists", marker)
		}
	}
}

func TestRemoveRepoHookLeavesForeignHookAndNonRepos(t *testing.T) {
	isolate(t)
	repo := initRepo(t)
	hook := filepath.Join(repo, ".git", "hooks", "post-commit")
	mustWrite(t, hook, "#!/bin/sh\necho mine\n")
	removed, err := RemoveRepoHook(repo)
	if err != nil || removed {
		t.Fatalf("foreign hook must be left alone: removed=%v err=%v", removed, err)
	}
	if _, err := os.Stat(hook); err != nil {
		t.Fatal("foreign hook was deleted")
	}

	removed, err = RemoveRepoHook(t.TempDir())
	if err != nil || removed {
		t.Fatalf("non-repo must be a no-op: removed=%v err=%v", removed, err)
	}
}

func TestInstallRepoHookUpgradesAnOlderPosthookHook(t *testing.T) {
	isolate(t)
	repo := initRepo(t)
	hook := filepath.Join(repo, ".git", "hooks", "post-commit")
	mustWrite(t, hook, "#!/bin/sh\n# posthook v1\n/old/posthook ingest || true\n")
	if _, err := InstallRepoHook(repo, testBin); err != nil {
		t.Fatalf("an older posthook hook must be overwritable, got: %v", err)
	}
	if got := readFile(t, hook); !strings.Contains(got, hookMarker) || !strings.Contains(got, testBin) {
		t.Fatalf("hook not upgraded:\n%s", got)
	}
}

func TestUninstallGlobalGitTemplate(t *testing.T) {
	isolate(t)
	res, err := UninstallGlobalGitTemplate()
	if err != nil || res.Changed {
		t.Fatalf("unset templateDir should be a no-op: %v %v", res, err)
	}

	git(t, t.TempDir(), "config", "--global", "init.templateDir", "/somewhere/else")
	res, err = UninstallGlobalGitTemplate()
	if err != nil || res.Changed {
		t.Fatalf("someone else's templateDir must be left alone: %v %v", res, err)
	}
	if got := git(t, t.TempDir(), "config", "--global", "--get", "init.templateDir"); got != "/somewhere/else" {
		t.Fatalf("templateDir changed to %q", got)
	}

	if _, err := InstallGlobalGitTemplate(testBin); err != nil {
		t.Fatal(err)
	}
	if !GlobalGitTemplateActive() {
		t.Fatal("install should point templateDir at posthook's dir")
	}
	res, err = UninstallGlobalGitTemplate()
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed || GlobalGitTemplateActive() {
		t.Fatalf("templateDir should be unset: %v", res)
	}
	if out, err := exec.Command("git", "config", "--global", "--get", "init.templateDir").Output(); err == nil {
		t.Fatalf("templateDir still set: %q", out)
	}
}
