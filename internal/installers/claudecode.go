package installers

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bilanc/posthook/internal/paths"
)

const claudeAgentSlug = "claude-code"

// claudeHookTypes are the events posthook actually needs: PostToolUse carries
// the file edits we attribute, Stop carries the transcript (model + tokens).
// PreToolUse and SessionStart fired on every tool call / session with no data
// we keep, doubling hook spawns for nothing — they're now stripped on install.
var claudeHookTypes = []string{"PostToolUse", "Stop"}

// claudeDeprecatedHookTypes are events posthook used to register but no longer
// does. On (re)install we remove our command from these so upgraded users stop
// firing redundant hooks.
var claudeDeprecatedHookTypes = []string{"PreToolUse", "SessionStart"}

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

	// Remove our command from event types we no longer register, so upgraded
	// installs stop firing redundant hooks. User hooks under these types stay.
	for _, hookType := range claudeDeprecatedHookTypes {
		stripClaudeHook(hooksObj, hookType)
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

// stripClaudeHook removes posthook's command from every block under a hook
// type, dropping blocks left empty and the hook-type key if no blocks remain.
// No-op when the type isn't present. Used to retire deprecated event types.
func stripClaudeHook(hooksObj map[string]any, hookType string) {
	blocks, ok := hooksObj[hookType].([]any)
	if !ok {
		return
	}
	keptBlocks := make([]any, 0, len(blocks))
	for _, b := range blocks {
		bm, ok := b.(map[string]any)
		if !ok {
			keptBlocks = append(keptBlocks, b)
			continue
		}
		inner, _ := bm["hooks"].([]any)
		kept := make([]any, 0, len(inner))
		for _, h := range inner {
			hm, ok := h.(map[string]any)
			if !ok {
				kept = append(kept, h)
				continue
			}
			cmd, _ := hm["command"].(string)
			if !IsPosthookCommand(cmd, claudeAgentSlug) {
				kept = append(kept, h)
			}
		}
		// Drop a block that only ever held our (now-removed) command.
		if len(kept) == 0 {
			continue
		}
		bm["hooks"] = kept
		keptBlocks = append(keptBlocks, bm)
	}
	if len(keptBlocks) == 0 {
		delete(hooksObj, hookType)
		return
	}
	hooksObj[hookType] = keptBlocks
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
