package installers

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bilanc/posthook/internal/paths"
)

const cursorAgentSlug = "cursor"

var cursorHookTypes = []string{
	"preToolUse", "postToolUse", "beforeSubmitPrompt", "afterFileEdit",
}

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
