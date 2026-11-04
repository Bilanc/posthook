package installers

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bilanc/posthook/internal/paths"
)

const copilotAgentSlug = "github-copilot"

// copilotHookTypes are registered under their PascalCase (Claude-compatible)
// names on purpose: Copilot then delivers snake_case payloads carrying
// hook_event_name, session_id, cwd, tool_name, tool_input and, on Stop,
// transcript_path — the shape ingest already parses for Claude Code.
// PostToolUse carries the file edits we attribute; Stop (Copilot's agentStop,
// end of every turn) carries the transcript path that backfills model, span,
// token totals and typed prompts. The same file is read by Copilot CLI and by
// VS Code's Copilot session target.
var copilotHookTypes = []string{"PostToolUse", "Stop"}

// copilotHookTimeoutSec matches Copilot's default; ingest only spools stdin so
// it returns in milliseconds, the cap just stops a wedged hook from stalling
// the agent.
const copilotHookTimeoutSec = 30

// DetectCopilot reports whether GitHub Copilot has been used on this machine:
// Copilot CLI creates ~/.copilot on first launch, and VS Code's Copilot Chat
// creates globalStorage/github.copilot-chat under the VS Code user directory.
// A bare VS Code install without Copilot is deliberately not enough — writing
// ~/.copilot/hooks there would create a directory nothing reads.
func DetectCopilot() bool {
	if _, err := os.Stat(paths.CopilotDir()); err == nil {
		return true
	}
	for _, dir := range paths.VSCodeUserDirs() {
		if _, err := os.Stat(filepath.Join(dir, "globalStorage", "github.copilot-chat")); err == nil {
			return true
		}
	}
	return false
}

// InstallCopilotHooks writes posthook's entries into ~/.copilot/hooks/posthook.json.
// Copilot loads every *.json in that directory and runs all matching entries,
// so posthook owns this one file outright rather than merging into a file the
// user wrote. Anything else found in it (a user who edited it by hand) is
// preserved; our own command is kept to exactly one entry per registered
// event and removed from events we no longer register.
func InstallCopilotHooks(binaryPath string) (Result, error) {
	path := paths.CopilotHooksPath()
	before, err := ReadJSONOrEmpty(path)
	if err != nil {
		return Result{}, err
	}
	beforeJSON, _ := json.Marshal(before)
	after := cloneJSON(before)
	desiredCmd := PosthookCommandFor(binaryPath, copilotAgentSlug)

	// Copilot rejects the whole file on a bad or missing version.
	if _, ok := after["version"]; !ok {
		after["version"] = 1
	}

	hooksObj := getOrMakeMap(after, "hooks")
	for _, hookType := range copilotHookTypes {
		arr, _ := hooksObj[hookType].([]any)
		kept := make([]any, 0, len(arr)+1)
		placed := false
		for _, h := range arr {
			if hm, ok := h.(map[string]any); ok && isPosthookCopilotEntry(hm) {
				if placed {
					continue // duplicate copy of our command
				}
				placed = true
				kept = append(kept, copilotHookEntry(desiredCmd)) // refresh in place (binary may have moved)
				continue
			}
			kept = append(kept, h)
		}
		if !placed {
			kept = append(kept, copilotHookEntry(desiredCmd))
		}
		hooksObj[hookType] = kept
	}

	// Retire our command from any event this release no longer registers so
	// upgrades stop firing redundant hooks. User entries under those events stay.
	for hookType := range hooksObj {
		if !containsString(copilotHookTypes, hookType) {
			stripCopilotHook(hooksObj, hookType)
		}
	}

	afterJSON, _ := json.Marshal(after)
	changed := string(beforeJSON) != string(afterJSON)
	if changed {
		if err := WriteJSONAtomic(path, after); err != nil {
			return Result{}, err
		}
	}
	msg := "GitHub Copilot: already up to date"
	if changed {
		msg = fmt.Sprintf("GitHub Copilot: hooks installed in %s", path)
	}
	return Result{Changed: changed, Path: path, Message: msg}, nil
}

// copilotHookEntry is one command hook. "command" is Copilot's cross-platform
// field: it is copied to both bash and powershell when those are absent.
func copilotHookEntry(cmd string) map[string]any {
	return map[string]any{
		"type":       "command",
		"command":    cmd,
		"timeoutSec": copilotHookTimeoutSec,
	}
}

// isPosthookCopilotEntry recognises our entry whichever command field a
// previous release (or a hand edit) put the command under.
func isPosthookCopilotEntry(hm map[string]any) bool {
	for _, key := range []string{"command", "bash", "powershell"} {
		if cmd, _ := hm[key].(string); IsPosthookCommand(cmd, copilotAgentSlug) {
			return true
		}
	}
	return false
}

// stripCopilotHook removes posthook's entries from one event, dropping the
// event if that leaves it empty. User entries stay, and an event we removed
// nothing from is not touched. No-op when the event isn't present.
func stripCopilotHook(hooksObj map[string]any, hookType string) {
	arr, ok := hooksObj[hookType].([]any)
	if !ok {
		return
	}
	kept := make([]any, 0, len(arr))
	for _, h := range arr {
		if hm, ok := h.(map[string]any); ok && isPosthookCopilotEntry(hm) {
			continue
		}
		kept = append(kept, h)
	}
	switch {
	case len(kept) == len(arr):
		return
	case len(kept) == 0:
		delete(hooksObj, hookType)
	default:
		hooksObj[hookType] = kept
	}
}

// UninstallCopilotHooks removes posthook's entries from every event in
// ~/.copilot/hooks/posthook.json, dropping events left empty. If nothing but
// the schema "version" field remains the file is deleted: that is exactly the
// file posthook creates, and Copilot's default is no such file. A file still
// carrying someone else's entries is rewritten without ours. A missing file
// is a no-op.
func UninstallCopilotHooks() (Result, error) {
	path := paths.CopilotHooksPath()
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return Result{Path: path, Message: "GitHub Copilot: no hooks file, nothing to remove"}, nil
	}
	before, err := ReadJSONOrEmpty(path)
	if err != nil {
		return Result{}, err
	}
	beforeJSON, _ := json.Marshal(before)
	after := cloneJSON(before)

	if hooksObj, ok := after["hooks"].(map[string]any); ok {
		hadTypes := len(hooksObj) > 0
		for hookType := range hooksObj {
			stripCopilotHook(hooksObj, hookType)
		}
		if hadTypes && len(hooksObj) == 0 {
			delete(after, "hooks")
		}
	}

	afterJSON, _ := json.Marshal(after)
	if string(beforeJSON) == string(afterJSON) {
		return Result{Path: path, Message: "GitHub Copilot: no posthook hooks found"}, nil
	}

	onlyVersion := true
	for k := range after {
		if k != "version" {
			onlyVersion = false
			break
		}
	}
	if onlyVersion {
		if err := os.Remove(path); err != nil {
			return Result{}, err
		}
		return Result{Changed: true, Path: path, Message: fmt.Sprintf("GitHub Copilot: hooks removed; deleted %s (nothing else was in it)", path)}, nil
	}
	if err := WriteJSONAtomic(path, after); err != nil {
		return Result{}, err
	}
	return Result{Changed: true, Path: path, Message: fmt.Sprintf("GitHub Copilot: hooks removed from %s", path)}, nil
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
