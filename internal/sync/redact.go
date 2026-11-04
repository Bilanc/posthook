package sync

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/bilanc/posthook/internal/store"
)

// Metadata-only sync (config.SyncModeMetadataOnly) is an allowlist, not a
// denylist: every outgoing row is rebuilt from the keys named here and
// anything else is dropped. A denylist would silently widen the moment an
// agent adds a hook field (Claude Code's last_assistant_message arrived that
// way) or an MCP tool carries free text under a new tool_input key.
//
// The local SQLite store is untouched — the local dashboard, blame and the
// session timeline keep working on the laptop. Only what leaves the machine
// changes. `posthook sync --preview` prints the exact batch after this pass.
//
// Three tables carry free text and are rewritten:
//
//   - events.payload: the raw hook stdin. Rebuilt from payloadKeys (+ a
//     filtered tool_input). tool_response / tool_output (shell output, file
//     contents), prompt, last_assistant_message, command, old/new_string,
//     content and edits are gone. The newline count of the dropped edit text
//     is kept on the row as lines_added so AI-line scoring upstream still
//     works without the text.
//   - session_prompts.prompt_text: blanked. prompt_chars keeps the length so
//     turn counts, turn timing and prompt size survive.
//   - commits.message: nulled. It is free text typed by a human; the sha,
//     author, timestamps and line counts stay.
//
// Everything else (ids, timestamps, enums, counts, emails, repo/file paths)
// passes through unchanged. Paths are metadata but do reveal directory names
// and the machine username; the README says so.
//
// The allowlist is on keys AND value shapes: an allowlisted key only survives
// with a scalar value (string, number, bool, null), or a list of strings for
// the two list-shaped keys. An agent that nests an object under a kept key —
// {"model": {"prompt": …}} — gets that value dropped, not forwarded.

// payloadKeys are the top-level hook-payload keys that survive. Identifiers,
// enums, numbers and paths only — nothing an engineer or an agent typed.
var payloadKeys = []string{
	// event identity
	"hook_event_name", "event", "type",
	"session_id", "sessionId", "conversation_id", "generation_id",
	"prompt_id", "turn_id", "tool_use_id",
	// agent / model
	"agent_type", "agent_id", "model", "model_id", "cursor_version",
	"permission_mode", "effort", "source", "stop_hook_active", "mcp_server",
	// tool identity + timing
	"tool_name", "duration_ms", "duration",
	// locations
	"cwd", "transcript_path", "transcriptPath", "workspace_roots",
	"scratchpad_dir", "file_path", "file_paths",
	// identity
	"user_email",
}

// toolInputKeys are the only tool_input fields that survive. file_path and
// friends feed per-file attribution; the rest are harmless scalars.
var toolInputKeys = []string{
	"file_path", "file_paths", "notebook_path", "cwd", "timeout", "replace_all",
}

// listKeys are the allowlisted keys whose value is a list of strings rather
// than a scalar (Cursor's workspace roots, multi-file tool calls).
var listKeys = map[string]bool{"file_paths": true, "workspace_roots": true}

// redactedMarker is stamped on every rewritten payload so a reader of the
// landed data can tell "captured but withheld" from "agent sent nothing".
const redactedMarker = "posthook_redacted"

var (
	payloadKeySet   = toSet(payloadKeys)
	toolInputKeySet = toSet(toolInputKeys)
)

func toSet(keys []string) map[string]bool {
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		m[k] = true
	}
	return m
}

// redactRow applies the metadata-only allowlist to one outgoing row of
// table. Rows of tables that carry no free text are returned as-is.
func redactRow(table string, row map[string]any) map[string]any {
	switch table {
	case "events":
		redactEventPayload(row)
	case "session_prompts":
		redactSessionPrompt(row)
	case "commits":
		if _, ok := row["message"]; ok {
			row["message"] = nil
		}
	}
	return row
}

// redactEventPayload rewrites row["payload"] in place. A NULL payload (hook
// misfire rows) stays NULL. A payload that is not a JSON object — the `raw`
// fallback ingest stores for agents that send non-JSON — is replaced by a
// marker object, since there is no structure to allowlist.
func redactEventPayload(row map[string]any) {
	raw, ok := row["payload"].(string)
	if !ok || raw == "" {
		return
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil || payload == nil {
		row["payload"] = `{"` + redactedMarker + `":true,"raw_dropped":true}`
		return
	}

	out := make(map[string]any, len(payloadKeys)+2)
	for k, v := range payload {
		if payloadKeySet[k] && allowedValue(k, v) {
			out[k] = v
		}
	}
	newLines := 0
	if ti, ok := payload["tool_input"].(map[string]any); ok {
		kept := map[string]any{}
		for k, v := range ti {
			if toolInputKeySet[k] && allowedValue(k, v) {
				kept[k] = v
			}
		}
		out["tool_input"] = kept
		newLines += newlineCount(ti["new_string"]) + newlineCount(ti["content"]) + editsNewlines(ti["edits"])
	}
	// Cursor afterFileEdit carries the edit at the top level: edits[] for a
	// multi-edit, new_string for a single edit, content for a whole-file
	// write (the same three shapes ingest's lineRangeToolForEvent recognises).
	newLines += editsNewlines(payload["edits"]) + newlineCount(payload["new_string"]) + newlineCount(payload["content"])
	out[redactedMarker] = true

	if newLines > 0 && isNull(row["lines_added"]) {
		row["lines_added"] = int64(newLines)
	}
	b, err := json.Marshal(out)
	if err != nil {
		row["payload"] = `{"` + redactedMarker + `":true,"raw_dropped":true}`
		return
	}
	row["payload"] = string(b)
}

// redactSessionPrompt blanks the prompt and records its length. The row
// itself still syncs so the turn it anchors (timing, tool calls) survives.
func redactSessionPrompt(row map[string]any) {
	text, _ := row["prompt_text"].(string)
	row["prompt_text"] = ""
	row["prompt_chars"] = int64(utf8.RuneCountInString(text))
	row["redacted"] = true
}

// newlineCount mirrors the server's fallback AI-line score for an edit —
// LENGTH(text) - LENGTH(REPLACE(text, '\n', ”)) — so a redacted row scores
// the same lines a full-mode row would.
func newlineCount(v any) int {
	s, ok := v.(string)
	if !ok {
		return 0
	}
	return strings.Count(s, "\n")
}

func editsNewlines(v any) int {
	edits, ok := v.([]any)
	if !ok {
		return 0
	}
	n := 0
	for _, e := range edits {
		if m, ok := e.(map[string]any); ok {
			n += newlineCount(m["new_string"])
		}
	}
	return n
}

func isNull(v any) bool {
	return v == nil
}

// allowedValue reports whether v is a shape an allowlisted key may carry out
// of the machine: a JSON scalar, or — for listKeys only — a list of strings.
// Objects and any other list are dropped even under an allowlisted key, so
// the allowlist cannot be tunnelled through.
func allowedValue(key string, v any) bool {
	switch x := v.(type) {
	case nil, string, float64, bool:
		return true
	case []any:
		if !listKeys[key] {
			return false
		}
		for _, e := range x {
			if _, ok := e.(string); !ok {
				return false
			}
		}
		return true
	}
	return false
}

// RedactTables applies the allowlist to a whole batch, table by table. Used
// by Flush and Preview; exported so tests outside the package can pin the
// wire shape.
func RedactTables(tables map[string][]map[string]any) {
	for _, table := range store.SyncableTables {
		for _, row := range tables[table] {
			redactRow(table, row)
		}
	}
}
