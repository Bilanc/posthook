package installers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bilanc/posthook/internal/atomicfs"
	"github.com/bilanc/posthook/internal/paths"

	"github.com/pelletier/go-toml/v2"
)

const codexAgentSlug = "codex"

// PostToolUse carries edits, Stop carries the transcript. PreToolUse fired on
// every tool call with no data we keep — dropped and stripped on install.
var codexHookEvents = []string{"PostToolUse", "Stop"}

var codexDeprecatedHookEvents = []string{"PreToolUse"}

func DetectCodex() bool {
	dir := filepath.Dir(paths.CodexConfigPath())
	_, err := os.Stat(dir)
	return err == nil
}

// InstallCodexHooks merges posthook entries into ~/.codex/config.toml. Codex
// requires (a) features.hooks = true, (b) ~/.posthook in sandbox_workspace_write
// writable_roots so our DB writes don't get sandboxed, and (c) a
// trust-state entry so it doesn't prompt the user on first fire.
func InstallCodexHooks(binaryPath string) (Result, error) {
	path := paths.CodexConfigPath()
	var config map[string]any
	if data, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(data)) != "" {
		if err := toml.Unmarshal(data, &config); err != nil {
			return Result{}, fmt.Errorf("parse %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) && err != nil {
		return Result{}, err
	}
	if config == nil {
		config = map[string]any{}
	}
	beforeJSON, _ := json.Marshal(config)
	desiredCmd := PosthookCommandFor(binaryPath, codexAgentSlug)

	// features.hooks = true, drop legacy codex_hooks flag.
	features := getOrMakeMap(config, "features")
	features["hooks"] = true
	delete(features, "codex_hooks")

	ensureWritableRoot(config)

	hooksTable := getOrMakeMap(config, "hooks")
	for _, event := range codexHookEvents {
		blocks, _ := hooksTable[event].([]any)
		if blocks == nil {
			blocks = []any{}
		}

		// Strip posthook command from non-catch-all blocks.
		for i, b := range blocks {
			bm, ok := b.(map[string]any)
			if !ok {
				continue
			}
			matcher, hasMatcher := bm["matcher"].(string)
			if hasMatcher && matcher != "*" {
				inner, _ := bm["hooks"].([]any)
				filtered := inner[:0]
				for _, h := range inner {
					hm, ok := h.(map[string]any)
					if !ok {
						filtered = append(filtered, h)
						continue
					}
					cmd, _ := hm["command"].(string)
					if !IsPosthookCommand(cmd, codexAgentSlug) {
						filtered = append(filtered, h)
					}
				}
				bm["hooks"] = filtered
				blocks[i] = bm
			}
		}

		// Find or create the catch-all block (matcher absent or "*").
		var catchAll map[string]any
		catchAllIdx := -1
		for i, b := range blocks {
			bm, ok := b.(map[string]any)
			if !ok {
				continue
			}
			matcher, hasMatcher := bm["matcher"].(string)
			if !hasMatcher || matcher == "*" {
				catchAll = bm
				catchAllIdx = i
				break
			}
		}
		if catchAll == nil {
			catchAll = map[string]any{"hooks": []any{}}
			blocks = append(blocks, catchAll)
			catchAllIdx = len(blocks) - 1
		}
		inner, _ := catchAll["hooks"].([]any)

		// Ensure exactly one posthook command.
		idx := -1
		for i, h := range inner {
			hm, ok := h.(map[string]any)
			if !ok {
				continue
			}
			cmd, _ := hm["command"].(string)
			if IsPosthookCommand(cmd, codexAgentSlug) {
				idx = i
				break
			}
		}
		if idx == -1 {
			inner = append(inner, map[string]any{"type": "command", "command": desiredCmd})
		} else {
			existing := inner[idx].(map[string]any)
			if existing["command"] != desiredCmd {
				inner[idx] = map[string]any{"type": "command", "command": desiredCmd}
			}
		}
		// Deduplicate.
		seen := false
		deduped := make([]any, 0, len(inner))
		for _, h := range inner {
			hm, ok := h.(map[string]any)
			if ok {
				cmd, _ := hm["command"].(string)
				if IsPosthookCommand(cmd, codexAgentSlug) {
					if seen {
						continue
					}
					seen = true
				}
			}
			deduped = append(deduped, h)
		}
		catchAll["hooks"] = deduped
		blocks[catchAllIdx] = catchAll

		// Trust-state entry for the catch-all block's posthook command.
		handlerIdx := -1
		for i, h := range deduped {
			hm, ok := h.(map[string]any)
			if !ok {
				continue
			}
			cmd, _ := hm["command"].(string)
			if IsPosthookCommand(cmd, codexAgentSlug) {
				handlerIdx = i
				break
			}
		}
		trustCodexHook(hooksTable, path, event, catchAllIdx, handlerIdx, desiredCmd)
		hooksTable[event] = blocks
	}

	// Retire event types we no longer register: strip our command (and any
	// now-empty blocks) plus their trust-state entries.
	for _, event := range codexDeprecatedHookEvents {
		stripCodexHook(hooksTable, path, event)
	}

	afterJSON, _ := json.Marshal(config)
	changed := string(beforeJSON) != string(afterJSON)
	if changed {
		buf, err := toml.Marshal(config)
		if err != nil {
			return Result{}, err
		}
		if err := atomicfs.Write(path, buf, 0o644); err != nil {
			return Result{}, err
		}
	}
	msg := "Codex CLI: already up to date"
	if changed {
		msg = fmt.Sprintf("Codex CLI: hooks installed in %s", path)
	}
	return Result{Changed: changed, Path: path, Message: msg}, nil
}

// removeCodexHookCommands filters posthook's command out of every block under
// an event, dropping blocks left empty and the event key if no blocks remain.
// Returns the command strings it removed (nil when the event is absent or
// held none of ours, in which case nothing is touched). User hooks under the
// same event stay.
func removeCodexHookCommands(hooksTable map[string]any, event string) []string {
	blocks, ok := hooksTable[event].([]any)
	if !ok {
		return nil
	}
	var removed []string
	kept := make([]any, 0, len(blocks))
	for _, b := range blocks {
		bm, ok := b.(map[string]any)
		if !ok {
			kept = append(kept, b)
			continue
		}
		inner, _ := bm["hooks"].([]any)
		keptInner := make([]any, 0, len(inner))
		for _, h := range inner {
			if hm, ok := h.(map[string]any); ok {
				if cmd, _ := hm["command"].(string); IsPosthookCommand(cmd, codexAgentSlug) {
					removed = append(removed, cmd)
					continue
				}
			}
			keptInner = append(keptInner, h)
		}
		if len(keptInner) != len(inner) {
			if len(keptInner) == 0 {
				continue
			}
			bm["hooks"] = keptInner
		}
		kept = append(kept, bm)
	}
	if len(removed) == 0 {
		return nil
	}
	if len(kept) == 0 {
		delete(hooksTable, event)
	} else {
		hooksTable[event] = kept
	}
	return removed
}

// stripCodexHook retires a deprecated event type: it removes our command
// (and any now-empty blocks) and clears every trust-state entry recorded for
// that event.
func stripCodexHook(hooksTable map[string]any, configPath, event string) {
	removeCodexHookCommands(hooksTable, event)
	if state, ok := hooksTable["state"].(map[string]any); ok {
		prefix := configPath + ":" + eventNameToSnakeCase(event) + ":"
		for k := range state {
			if strings.HasPrefix(k, prefix) {
				delete(state, k)
			}
		}
	}
}

// dropCodexTrustState removes the trust-state entries recorded for the given
// posthook commands under one event. Entries are matched by trusted hash
// (computed from the command exactly as trustCodexHook did), not by position,
// so the user's own entries under the same event survive. When the event has
// no hooks left at all its remaining entries are orphans and go too.
func dropCodexTrustState(hooksTable map[string]any, configPath, event string, commands []string) {
	state, ok := hooksTable["state"].(map[string]any)
	if !ok {
		return
	}
	eventName := eventNameToSnakeCase(event)
	ours := map[string]bool{}
	for _, cmd := range commands {
		ours[computeTrustHash(eventName, cmd)] = true
	}
	_, eventHasHooks := hooksTable[event]
	prefix := configPath + ":" + eventName + ":"
	for k, v := range state {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		entry, _ := v.(map[string]any)
		hash, _ := entry["trusted_hash"].(string)
		if ours[hash] || !eventHasHooks {
			delete(state, k)
		}
	}
}

// UninstallCodexHooks removes posthook from ~/.codex/config.toml: our command
// under every hook event (current and deprecated), the trust-state entries
// recorded for those commands, and ~/.posthook from the sandbox writable
// roots. features.hooks is dropped only when no hooks of any kind remain —
// posthook sets it, but so might the user, and it is harmless on its own.
// User hooks and their trust entries stay. A missing file is a no-op.
func UninstallCodexHooks() (Result, error) {
	path := paths.CodexConfigPath()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Result{Path: path, Message: "Codex CLI: no config file, nothing to remove"}, nil
	}
	if err != nil {
		return Result{}, err
	}
	var config map[string]any
	if strings.TrimSpace(string(data)) != "" {
		if err := toml.Unmarshal(data, &config); err != nil {
			return Result{}, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	if config == nil {
		config = map[string]any{}
	}
	beforeJSON, _ := json.Marshal(config)

	removedAny := false
	if hooksTable, ok := config["hooks"].(map[string]any); ok {
		for event := range hooksTable {
			if event == "state" {
				continue
			}
			if removed := removeCodexHookCommands(hooksTable, event); len(removed) > 0 {
				removedAny = true
				dropCodexTrustState(hooksTable, path, event, removed)
			}
		}
		if removedAny {
			if state, ok := hooksTable["state"].(map[string]any); ok && len(state) == 0 {
				delete(hooksTable, "state")
			}
			if len(hooksTable) == 0 {
				delete(config, "hooks")
			}
		}
	}
	if removeWritableRoot(config) {
		removedAny = true
	}
	// Only drop the feature flag once nothing is left that needs it.
	if _, hooksLeft := config["hooks"]; removedAny && !hooksLeft {
		if features, ok := config["features"].(map[string]any); ok {
			delete(features, "hooks")
			if len(features) == 0 {
				delete(config, "features")
			}
		}
	}

	afterJSON, _ := json.Marshal(config)
	changed := string(beforeJSON) != string(afterJSON)
	if changed {
		buf, err := toml.Marshal(config)
		if err != nil {
			return Result{}, err
		}
		if err := atomicfs.Write(path, buf, 0o644); err != nil {
			return Result{}, err
		}
	}
	msg := "Codex CLI: no posthook hooks found"
	if changed {
		msg = fmt.Sprintf("Codex CLI: hooks removed from %s", path)
	}
	return Result{Changed: changed, Path: path, Message: msg}, nil
}

// removeWritableRoot drops ~/.posthook from sandbox_workspace_write.writable_roots
// (the inverse of ensureWritableRoot), removing the list and the table when
// they are left empty. Returns true iff anything changed.
func removeWritableRoot(config map[string]any) bool {
	ww, ok := config["sandbox_workspace_write"].(map[string]any)
	if !ok {
		return false
	}
	roots, ok := ww["writable_roots"].([]any)
	if !ok {
		return false
	}
	kept := make([]any, 0, len(roots))
	for _, r := range roots {
		if s, ok := r.(string); ok && s == paths.PosthookDir() {
			continue
		}
		kept = append(kept, r)
	}
	if len(kept) == len(roots) {
		return false
	}
	if len(kept) == 0 {
		delete(ww, "writable_roots")
		if len(ww) == 0 {
			delete(config, "sandbox_workspace_write")
		}
	} else {
		ww["writable_roots"] = kept
	}
	return true
}

func trustCodexHook(
	hooksTable map[string]any,
	configPath, event string,
	groupIdx, handlerIdx int,
	command string,
) {
	if groupIdx < 0 || handlerIdx < 0 {
		return
	}
	state, _ := hooksTable["state"].(map[string]any)
	if state == nil {
		state = map[string]any{}
	}
	hooksTable["state"] = state

	eventName := eventNameToSnakeCase(event)
	stateKey := fmt.Sprintf("%s:%s:%d:%d", configPath, eventName, groupIdx, handlerIdx)
	state[stateKey] = map[string]any{
		"enabled":      true,
		"trusted_hash": computeTrustHash(eventName, command),
	}
}

func ensureWritableRoot(config map[string]any) {
	ww := getOrMakeMap(config, "sandbox_workspace_write")
	roots, _ := ww["writable_roots"].([]any)
	var asStrings []string
	for _, r := range roots {
		if s, ok := r.(string); ok {
			asStrings = append(asStrings, s)
		}
	}
	target := paths.PosthookDir()
	found := false
	for _, s := range asStrings {
		if s == target {
			found = true
			break
		}
	}
	if !found {
		asStrings = append(asStrings, target)
	}
	out := make([]any, len(asStrings))
	for i, s := range asStrings {
		out[i] = s
	}
	ww["writable_roots"] = out
}

func eventNameToSnakeCase(event string) string {
	switch event {
	case "PreToolUse":
		return "pre_tool_use"
	case "PostToolUse":
		return "post_tool_use"
	case "Stop":
		return "stop"
	}
	return strings.ToLower(event)
}

// computeTrustHash mirrors the codex CLI's trust-hash format: a canonical
// JSON-encoded identity object hashed with SHA-256, prefixed with "sha256:".
func computeTrustHash(eventName, command string) string {
	identity := map[string]any{
		"event_name": eventName,
		"hooks": []map[string]any{
			{
				"async":   false,
				"command": command,
				"timeout": 600,
				"type":    "command",
			},
		},
	}
	encoded := canonicalJSON(identity)
	sum := sha256.Sum256([]byte(encoded))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// canonicalJSON encodes value with sorted object keys at every level. Mirrors
// the canonicalization the TS version did via JSON.stringify of the
// already-sorted object.
func canonicalJSON(v any) string {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			kj, _ := json.Marshal(k)
			b.Write(kj)
			b.WriteByte(':')
			b.WriteString(canonicalJSON(t[k]))
		}
		b.WriteByte('}')
		return b.String()
	case []any:
		var b strings.Builder
		b.WriteByte('[')
		for i, item := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(canonicalJSON(item))
		}
		b.WriteByte(']')
		return b.String()
	case []map[string]any:
		items := make([]any, len(t))
		for i, m := range t {
			items[i] = m
		}
		return canonicalJSON(items)
	default:
		data, _ := json.Marshal(v)
		return string(data)
	}
}
