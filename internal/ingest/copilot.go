package ingest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/bilanc/posthook/internal/paths"
)

const copilotAgentSlug = "github-copilot"

// GitHub Copilot hooks (Copilot CLI and VS Code's Copilot session target share
// one runtime and one hook file) can deliver two payload formats, selected by
// how the event was registered: PascalCase names give snake_case fields with
// hook_event_name, camelCase names give camelCase fields and no event name.
// Observed from VS Code 1.144 under PascalCase registration: cwd,
// hook_event_name, session_id, timestamp, tool_name, tool_input,
// tool_response, tool_use_id and transcript_path on every PostToolUse (the
// docs promise transcript_path only on Stop), tool names left as VS Code
// spells them (list_dir, read_file, apply_patch). Tool names and arguments
// vary more: the CLI's apply_patch passes the patch text as the entire
// argument, VS Code wraps it as {input}, the CLI's create/edit tools use
// {path, file_text} / {path, old_str, new_str}, VS Code's native tools use
// {filePath, content} / {filePath, oldString, newString}, and the CLI's
// PascalCase mode is documented to map names Claude-style (apply_patch ->
// Edit, create -> Write, bash -> Bash) without touching the arguments. Rather
// than trust the name, normalizeCopilotPayload reads the argument shape and
// rewrites the payload into the canonical tool_name/tool_input pairs the rest
// of ingest already handles for Claude Code and Codex.

// copilotFieldAliases maps camelCase payload fields onto the snake_case names
// ingest reads.
var copilotFieldAliases = map[string]string{
	"hookEventName":  "hook_event_name",
	"sessionId":      "session_id",
	"transcriptPath": "transcript_path",
	"toolName":       "tool_name",
	"toolArgs":       "tool_input",
	"toolResult":     "tool_result",
}

// normalizeCopilotPayload rewrites a Copilot hook payload in place into the
// Claude-shaped form ingest understands and returns it.
func normalizeCopilotPayload(payload map[string]any) map[string]any {
	for from, to := range copilotFieldAliases {
		if _, exists := payload[to]; exists {
			continue
		}
		if v, ok := payload[from]; ok {
			payload[to] = v
		}
	}
	if _, ok := payload["hook_event_name"]; !ok {
		if ev := inferCopilotEvent(payload); ev != "" {
			payload["hook_event_name"] = ev
		}
	}

	// Copilot parses tool arguments from JSON where it can, but a JSON-encoded
	// string can still arrive (its own docs test hooks that way).
	if s, ok := payload["tool_input"].(string); ok {
		if trimmed := strings.TrimSpace(s); strings.HasPrefix(trimmed, "{") {
			var m map[string]any
			if json.Unmarshal([]byte(trimmed), &m) == nil {
				payload["tool_input"] = m
			}
		}
	}

	toolName, _ := payload["tool_name"].(string)
	if name, input, ok := canonicalCopilotTool(toolName, payload["tool_input"]); ok {
		if name != toolName {
			payload["copilot_tool_name"] = toolName
		}
		payload["tool_name"] = name
		payload["tool_input"] = input
	}
	return payload
}

// inferCopilotEvent names the event for a camelCase payload, which carries no
// event-name field, from the fields present.
func inferCopilotEvent(payload map[string]any) string {
	switch {
	case payload["tool_result"] != nil:
		return "PostToolUse"
	case payload["tool_name"] != nil:
		return "PreToolUse"
	case payload["prompt"] != nil:
		return "UserPromptSubmit"
	case payload["stop_reason"] != nil || payload["stopReason"] != nil:
		return "Stop"
	}
	return ""
}

func isApplyPatchText(s string) bool {
	return strings.HasPrefix(strings.TrimSpace(s), "*** Begin Patch")
}

// canonicalCopilotTool maps a Copilot tool call onto the canonical
// (tool_name, tool_input) pair by argument shape. ok is false when the call
// is left untouched.
func canonicalCopilotTool(toolName string, raw any) (name string, input any, ok bool) {
	switch v := raw.(type) {
	case string:
		// Copilot CLI apply_patch: the patch text is the whole argument. Hand
		// it to the Codex apply_patch path, which expects {command: <patch>}.
		if isApplyPatchText(v) {
			return "apply_patch", map[string]any{"command": v}, true
		}
		return "", nil, false
	case map[string]any:
		return canonicalCopilotToolMap(toolName, v)
	}
	return "", nil, false
}

func canonicalCopilotToolMap(toolName string, ti map[string]any) (string, any, bool) {
	// VS Code apply_patch: {input: <patch>, explanation, toolUses}.
	if s, ok := ti["input"].(string); ok && isApplyPatchText(s) {
		return "apply_patch", map[string]any{"command": s}, true
	}

	// Shell: the CLI's bash/powershell (Bash/PowerShell once Claude-mapped)
	// and VS Code's run_in_terminal all carry {command}.
	switch strings.ToLower(toolName) {
	case "bash", "powershell", "run_in_terminal", "shell":
		if _, ok := ti["command"].(string); ok {
			return "Bash", ti, true
		}
		return "", nil, false
	}

	// VS Code multi_replace_string_in_file: {replacements: [{filePath, oldString, newString}]}.
	// One file becomes a MultiEdit; several files become a synthetic
	// apply_patch so the stored event carries lines_added/lines_removed and
	// every downstream reader (metrics, dashboard, backfills) treats it like
	// Codex's and Copilot's own multi-file patches.
	if reps, ok := ti["replacements"].([]any); ok {
		files := groupCopilotReplacements(reps)
		if len(files) == 0 {
			return "", nil, false
		}
		if len(files) == 1 {
			return "MultiEdit", map[string]any{"file_path": files[0]["file_path"], "edits": files[0]["edits"]}, true
		}
		return "apply_patch", map[string]any{"command": replacementsAsPatch(files)}, true
	}

	filePath, hasPath := firstString(ti, "file_path", "path", "filePath")
	if !hasPath || isCopilotBrowseTool(toolName) {
		return "", nil, false
	}
	oldStr, hasOld := firstString(ti, "old_string", "old_str", "oldString")
	newStr, hasNew := firstString(ti, "new_string", "new_str", "newString")
	content, hasContent := firstString(ti, "content", "file_text")
	switch {
	case ti["edits"] != nil:
		// Claude-native MultiEdit shape, already canonical.
		out := cloneMap(ti)
		out["file_path"] = filePath
		return "MultiEdit", out, true
	case hasOld && hasNew:
		return "Edit", map[string]any{"file_path": filePath, "old_string": oldStr, "new_string": newStr}, true
	case hasContent && !hasOld:
		return "Write", map[string]any{"file_path": filePath, "content": content}, true
	}
	// A path-bearing tool we attribute no lines for (view/Read, str_replace_editor
	// insert, insert_edit_into_file …): keep its name, expose the path so the
	// event still records the file touched.
	out := cloneMap(ti)
	out["file_path"] = filePath
	return toolName, out, true
}

// copilotBrowseToolHints mark tools whose path argument is a search root or
// a store, not a file the agent read or wrote: list_dir, glob, grep_search,
// file_search, memory … Their events keep no file_path, matching how Claude
// Code's Glob/Grep/LS events are recorded.
var copilotBrowseToolHints = []string{"list", "dir", "glob", "search", "grep", "find", "memory", "fetch", "web"}

func isCopilotBrowseTool(toolName string) bool {
	lower := strings.ToLower(toolName)
	for _, hint := range copilotBrowseToolHints {
		if strings.Contains(lower, hint) {
			return true
		}
	}
	return false
}

// groupCopilotReplacements groups {filePath, oldString, newString} items by
// file, preserving first-appearance order, as {file_path, edits: [{old_string,
// new_string}]} maps.
func groupCopilotReplacements(reps []any) []map[string]any {
	var files []map[string]any
	index := map[string]int{}
	// Group as []map for building, but hand out []any-typed children so the
	// result reads back like decoded JSON (ingest only type-asserts []any).
	for _, r := range reps {
		rm, ok := r.(map[string]any)
		if !ok {
			continue
		}
		fp, ok := firstString(rm, "filePath", "file_path", "path")
		if !ok {
			continue
		}
		oldStr, _ := firstString(rm, "oldString", "old_string", "old_str")
		newStr, _ := firstString(rm, "newString", "new_string", "new_str")
		edit := map[string]any{"old_string": oldStr, "new_string": newStr}
		if i, seen := index[fp]; seen {
			files[i]["edits"] = append(files[i]["edits"].([]any), edit)
			continue
		}
		index[fp] = len(files)
		files = append(files, map[string]any{"file_path": fp, "edits": []any{edit}})
	}
	return files
}

// replacementsAsPatch renders grouped replacements as apply_patch text: one
// "*** Update File:" section per file, each replacement as a hunk of "-"
// old lines followed by "+" new lines, which is what lineranges.ParseApplyPatch
// reads back.
func replacementsAsPatch(files []map[string]any) string {
	var b strings.Builder
	b.WriteString("*** Begin Patch\n")
	for _, f := range files {
		fp, _ := f["file_path"].(string)
		edits, _ := f["edits"].([]any)
		b.WriteString("*** Update File: " + fp + "\n")
		for _, e := range edits {
			em, _ := e.(map[string]any)
			oldStr, _ := em["old_string"].(string)
			newStr, _ := em["new_string"].(string)
			b.WriteString("@@\n")
			for _, line := range patchLines(oldStr) {
				b.WriteString("-" + line + "\n")
			}
			for _, line := range patchLines(newStr) {
				b.WriteString("+" + line + "\n")
			}
		}
	}
	b.WriteString("*** End Patch\n")
	return b.String()
}

func patchLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// copilotTranscriptPath returns the transcript for a Copilot session: the
// path the Stop hook supplied when it exists, otherwise the CLI's
// session-state file or VS Code's per-workspace transcript, found by session
// id. Copilot only documents transcript_path on agentStop, and VS Code's
// Copilot target may hand us a path we cannot read.
func copilotTranscriptPath(given, sessionID string) string {
	if given != "" {
		if _, err := os.Stat(given); err == nil {
			return given
		}
	}
	if sessionID == "" || strings.ContainsAny(sessionID, `/\`) || strings.Contains(sessionID, "..") {
		return ""
	}
	cli := filepath.Join(paths.CopilotSessionStateDir(), sessionID, "events.jsonl")
	if _, err := os.Stat(cli); err == nil {
		return cli
	}
	for _, dir := range paths.VSCodeUserDirs() {
		pattern := filepath.Join(dir, "workspaceStorage", "*", "GitHub.copilot-chat", "transcripts", sessionID+".jsonl")
		if matches, _ := filepath.Glob(pattern); len(matches) > 0 {
			return matches[0]
		}
	}
	return ""
}

// isCopilotPayload reports whether a hook payload originated from GitHub
// Copilot, regardless of which hook registration delivered it. Copilot also
// runs Claude-format hooks from a repository's .claude/settings.json, and VS
// Code's legacy Local harness can run ~/.claude/settings.json hooks, so a
// Copilot event can arrive under --agent claude-code: the transcript path
// (CLI session-state or VS Code's GitHub.copilot-chat storage) and the
// camelCase / tool_result field names only Copilot uses give it away.
func isCopilotPayload(payload map[string]any) bool {
	tp := strings.ToLower(strings.ReplaceAll(pickString(payload, "transcript_path", "transcriptPath"), `\`, "/"))
	if strings.Contains(tp, "github.copilot-chat/") {
		return true
	}
	if strings.Contains(tp, "/session-state/") && strings.HasSuffix(tp, "/events.jsonl") {
		return true
	}
	if _, ok := payload["toolArgs"]; ok {
		return true
	}
	if tr, ok := payload["tool_result"].(map[string]any); ok {
		if _, ok := tr["text_result_for_llm"]; ok {
			return true
		}
	}
	return false
}

func firstString(m map[string]any, keys ...string) (string, bool) {
	for _, k := range keys {
		if v, ok := m[k].(string); ok {
			return v, true
		}
	}
	return "", false
}

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	return out
}
