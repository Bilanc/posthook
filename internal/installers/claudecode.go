package installers

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bilanc/posthook/internal/paths"
)

const claudeAgentSlug = "claude-code"

var claudeHookTypes = []string{"PreToolUse", "PostToolUse", "SessionStart", "Stop"}

// DetectClaudeCode reports whether ~/.claude/ exists.
func DetectClaudeCode() bool {
	dir := filepath.Dir(paths.ClaudeSettingsPath())
	_, err := os.Stat(dir)
	return err == nil
}

// InstallClaudeCodeHooks merges posthook entries into ~/.claude/settings.json.
// The catch-all matcher "*" carries our hook for each event type; pre-existing
// hooks under other matchers are untouched.
func InstallClaudeCodeHooks(binaryPath string) (Result, error) {
	path := paths.ClaudeSettingsPath()
	before, err := ReadJSONOrEmpty(path)
	if err != nil {
		return Result{}, err
	}
	beforeJSON, _ := json.Marshal(before)
	after := cloneJSON(before)
	desiredCmd := PosthookCommandFor(binaryPath, claudeAgentSlug)

	hooksObj := getOrMakeMap(after, "hooks")
	for _, hookType := range claudeHookTypes {
		blocks := getOrMakeSlice(hooksObj, hookType)

		// Strip our command from every non-catch-all block (handles upgrades).
		for i, b := range blocks {
			bm, ok := b.(map[string]any)
			if !ok {
				continue
			}
			matcher, _ := bm["matcher"].(string)
			if matcher == "*" {
				continue
			}
			inner, _ := bm["hooks"].([]any)
			filtered := inner[:0]
			for _, h := range inner {
				hm, ok := h.(map[string]any)
				if !ok {
					filtered = append(filtered, h)
					continue
				}
				cmd, _ := hm["command"].(string)
				if !IsPosthookCommand(cmd, claudeAgentSlug) {
					filtered = append(filtered, h)
				}
			}
			bm["hooks"] = filtered
			blocks[i] = bm
		}

		// Find or create the catch-all block.
		var catchAll map[string]any
		catchAllIdx := -1
		for i, b := range blocks {
			bm, ok := b.(map[string]any)
			if !ok {
				continue
			}
			if matcher, _ := bm["matcher"].(string); matcher == "*" {
				catchAll = bm
				catchAllIdx = i
				break
			}
		}
		if catchAll == nil {
			catchAll = map[string]any{"matcher": "*", "hooks": []any{}}
			blocks = append(blocks, catchAll)
			catchAllIdx = len(blocks) - 1
		}
		inner, _ := catchAll["hooks"].([]any)
		if inner == nil {
			inner = []any{}
		}

		// Ensure exactly one posthook command in the catch-all.
		existingIdx := -1
		for i, h := range inner {
			hm, ok := h.(map[string]any)
			if !ok {
				continue
			}
			cmd, _ := hm["command"].(string)
			if IsPosthookCommand(cmd, claudeAgentSlug) {
				existingIdx = i
				break
			}
		}
		if existingIdx == -1 {
			inner = append(inner, map[string]any{"type": "command", "command": desiredCmd})
		} else {
			existing := inner[existingIdx].(map[string]any)
			if existing["command"] != desiredCmd {
				inner[existingIdx] = map[string]any{"type": "command", "command": desiredCmd}
			}
			// Deduplicate any further copies.
			deduped := make([]any, 0, len(inner))
			for i, h := range inner {
				if i == existingIdx {
					deduped = append(deduped, h)
					continue
				}
				hm, ok := h.(map[string]any)
				if !ok {
					deduped = append(deduped, h)
					continue
				}
				cmd, _ := hm["command"].(string)
				if IsPosthookCommand(cmd, claudeAgentSlug) {
					continue
				}
				deduped = append(deduped, h)
			}
			inner = deduped
		}
		catchAll["hooks"] = inner
		blocks[catchAllIdx] = catchAll
		hooksObj[hookType] = blocks
	}

	afterJSON, _ := json.Marshal(after)
	changed := string(beforeJSON) != string(afterJSON)
	if changed {
		if err := WriteJSONAtomic(path, after); err != nil {
			return Result{}, err
		}
	}
	msg := fmt.Sprintf("Claude Code: already up to date")
	if changed {
		msg = fmt.Sprintf("Claude Code: hooks installed in %s", path)
	}
	return Result{Changed: changed, Path: path, Message: msg}, nil
}

func getOrMakeMap(parent map[string]any, key string) map[string]any {
	if existing, ok := parent[key].(map[string]any); ok {
		return existing
	}
	m := map[string]any{}
	parent[key] = m
	return m
}

func getOrMakeSlice(parent map[string]any, key string) []any {
	if existing, ok := parent[key].([]any); ok {
		return existing
	}
	s := []any{}
	parent[key] = s
	return s
}

// cloneJSON does a deep clone via the JSON round-trip so we can compare
// before/after for "did we change anything" without aliasing issues.
func cloneJSON(v map[string]any) map[string]any {
	data, _ := json.Marshal(v)
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	if out == nil {
		return map[string]any{}
	}
	return out
}
