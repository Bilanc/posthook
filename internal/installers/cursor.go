package installers

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bilanc/posthook/internal/paths"
)

const cursorAgentSlug = "cursor"

// postToolUse + afterFileEdit carry the edits we attribute; beforeSubmitPrompt
// marks session/prompt boundaries. preToolUse fired on every tool call with no
// data we keep, so it's dropped and stripped on install.
var cursorHookTypes = []string{
	"postToolUse", "beforeSubmitPrompt", "afterFileEdit",
}

var cursorDeprecatedHookTypes = []string{"preToolUse"}

func DetectCursor() bool {
	dir := filepath.Dir(paths.CursorHooksPath())
	_, err := os.Stat(dir)
	return err == nil
}

// InstallCursorHooks merges posthook entries into ~/.cursor/hooks.json.
// Cursor's schema is flatter than Claude's: events map directly to an array
// of {command} entries with no matcher layer.
func InstallCursorHooks(binaryPath string) (Result, error) {
	path := paths.CursorHooksPath()
	before, err := ReadJSONOrEmpty(path)
	if err != nil {
		return Result{}, err
	}
	beforeJSON, _ := json.Marshal(before)
	after := cloneJSON(before)
	desiredCmd := PosthookCommandFor(binaryPath, cursorAgentSlug)

	if _, ok := after["version"]; !ok {
		after["version"] = 1
	}

	hooksObj := getOrMakeMap(after, "hooks")
	for _, hookType := range cursorHookTypes {
		arr, _ := hooksObj[hookType].([]any)
		if arr == nil {
			arr = []any{}
		}

		idx := -1
		for i, h := range arr {
			hm, ok := h.(map[string]any)
			if !ok {
				continue
			}
			cmd, _ := hm["command"].(string)
			if IsPosthookCommand(cmd, cursorAgentSlug) {
				idx = i
				break
			}
		}
		if idx == -1 {
			arr = append(arr, map[string]any{"command": desiredCmd})
		} else {
			existing := arr[idx].(map[string]any)
			if existing["command"] != desiredCmd {
				arr[idx] = map[string]any{"command": desiredCmd}
			}
		}

		// Deduplicate any additional copies of our command.
		seen := false
		deduped := make([]any, 0, len(arr))
		for _, h := range arr {
			hm, ok := h.(map[string]any)
			if ok {
				cmd, _ := hm["command"].(string)
				if IsPosthookCommand(cmd, cursorAgentSlug) {
					if seen {
						continue
					}
					seen = true
				}
			}
			deduped = append(deduped, h)
		}
		hooksObj[hookType] = deduped
	}

	// Retire event types we no longer register: drop our command (and any
	// hook-type left empty), keeping user hooks intact.
	for _, hookType := range cursorDeprecatedHookTypes {
		stripCursorHook(hooksObj, hookType)
	}

	afterJSON, _ := json.Marshal(after)
	changed := string(beforeJSON) != string(afterJSON)
	if changed {
		if err := WriteJSONAtomic(path, after); err != nil {
			return Result{}, err
		}
	}
	msg := "Cursor: already up to date"
	if changed {
		msg = fmt.Sprintf("Cursor: hooks installed in %s", path)
	}
	return Result{Changed: changed, Path: path, Message: msg}, nil
}

// stripCursorHook removes posthook's command from one hook type, dropping the
// type if that leaves it empty. User entries stay, and a type we removed
// nothing from is not touched. No-op when the type isn't present.
func stripCursorHook(hooksObj map[string]any, hookType string) {
	arr, ok := hooksObj[hookType].([]any)
	if !ok {
		return
	}
	kept := make([]any, 0, len(arr))
	for _, h := range arr {
		if hm, ok := h.(map[string]any); ok {
			if cmd, _ := hm["command"].(string); IsPosthookCommand(cmd, cursorAgentSlug) {
				continue
			}
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

// UninstallCursorHooks removes posthook's command from every hook type in
// ~/.cursor/hooks.json, dropping types left empty. If nothing but the schema
// "version" field remains afterwards the file is deleted: that is exactly the
// file posthook creates on a machine that had none, and Cursor's default
// state is no hooks.json at all. A file carrying anything else is rewritten
// without our entries. A missing file is a no-op.
func UninstallCursorHooks() (Result, error) {
	path := paths.CursorHooksPath()
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return Result{Path: path, Message: "Cursor: no hooks file, nothing to remove"}, nil
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
			stripCursorHook(hooksObj, hookType)
		}
		if hadTypes && len(hooksObj) == 0 {
			delete(after, "hooks")
		}
	}

	afterJSON, _ := json.Marshal(after)
	if string(beforeJSON) == string(afterJSON) {
		return Result{Path: path, Message: "Cursor: no posthook hooks found"}, nil
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
		return Result{Changed: true, Path: path, Message: fmt.Sprintf("Cursor: hooks removed; deleted %s (nothing else was in it)", path)}, nil
	}
	if err := WriteJSONAtomic(path, after); err != nil {
		return Result{}, err
	}
	return Result{Changed: true, Path: path, Message: fmt.Sprintf("Cursor: hooks removed from %s", path)}, nil
}
