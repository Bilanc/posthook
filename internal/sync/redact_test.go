package sync

import (
	"encoding/json"
	"strings"
	"testing"
)

// Every string an engineer or an agent could have typed, in every place a
// hook payload can carry it. If any of these survives a metadata-only flush
// the mode is broken — this is the test to hand a security reviewer.
var contentStrings = []string{
	"SECRET_PROMPT_TEXT",
	"SECRET_ASSISTANT_REPLY",
	"SECRET_BASH_COMMAND",
	"SECRET_BASH_DESCRIPTION",
	"SECRET_STDOUT",
	"SECRET_STDERR",
	"SECRET_OLD_STRING",
	"SECRET_NEW_STRING",
	"SECRET_WRITE_CONTENT",
	"SECRET_ORIGINAL_FILE",
	"SECRET_READ_FILE_BODY",
	"SECRET_PATCH_HUNK",
	"SECRET_MCP_SQL",
	"SECRET_MCP_PROMPT",
	"SECRET_MULTIEDIT_NEW",
	"SECRET_CURSOR_EDIT_NEW",
	"SECRET_CURSOR_TOOL_OUTPUT",
	"SECRET_BACKGROUND_TASK",
	"SECRET_UNKNOWN_TOP_LEVEL_KEY",
	"SECRET_RAW_NON_JSON",
	"SECRET_COMMIT_MESSAGE",
	"SECRET_CURSOR_FLAT_NEW",
	"SECRET_CURSOR_FLAT_CONTENT",
	"SECRET_NESTED_UNDER_MODEL",
	"SECRET_NESTED_IN_LIST",
	"SECRET_NESTED_IN_NUMBER_KEY",
	"SECRET_NESTED_TOOL_INPUT",
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func fixtureTables(t *testing.T) map[string][]map[string]any {
	t.Helper()
	postToolUseEdit := map[string]any{
		"hook_event_name": "PostToolUse", "session_id": "s1", "cwd": "/repo",
		"tool_name": "Edit", "tool_use_id": "tu1", "transcript_path": "/t.jsonl",
		"agent_type": "main", "permission_mode": "auto", "effort": "high", "duration_ms": 12,
		"tool_input": map[string]any{
			"file_path": "/repo/a.go", "old_string": "SECRET_OLD_STRING",
			"new_string": "SECRET_NEW_STRING\nline2\nline3", "replace_all": false,
		},
		"tool_response": map[string]any{
			"filePath": "/repo/a.go", "oldString": "SECRET_OLD_STRING", "newString": "SECRET_NEW_STRING",
			"originalFile": "SECRET_ORIGINAL_FILE", "structuredPatch": []any{"SECRET_PATCH_HUNK"},
		},
		"some_future_key": "SECRET_UNKNOWN_TOP_LEVEL_KEY",
	}
	postToolUseBash := map[string]any{
		"hook_event_name": "PostToolUse", "session_id": "s1", "tool_name": "Bash",
		"tool_input":    map[string]any{"command": "SECRET_BASH_COMMAND", "description": "SECRET_BASH_DESCRIPTION", "timeout": 1000},
		"tool_response": map[string]any{"stdout": "SECRET_STDOUT", "stderr": "SECRET_STDERR", "interrupted": false},
	}
	postToolUseRead := map[string]any{
		"hook_event_name": "PostToolUse", "session_id": "s1", "tool_name": "Read",
		"tool_input":    map[string]any{"file_path": "/repo/b.go"},
		"tool_response": map[string]any{"type": "text", "file": map[string]any{"filePath": "/repo/b.go", "content": "SECRET_READ_FILE_BODY"}},
	}
	postToolUseWrite := map[string]any{
		"hook_event_name": "PostToolUse", "session_id": "s1", "tool_name": "Write",
		"tool_input": map[string]any{"file_path": "/repo/c.go", "content": "SECRET_WRITE_CONTENT\n\n"},
	}
	postToolUseMulti := map[string]any{
		"hook_event_name": "PostToolUse", "session_id": "s1", "tool_name": "MultiEdit",
		"tool_input": map[string]any{"file_path": "/repo/d.go", "edits": []any{
			map[string]any{"old_string": "x", "new_string": "SECRET_MULTIEDIT_NEW\n"},
			map[string]any{"old_string": "y", "new_string": "z\n\n"},
		}},
	}
	postToolUseMCP := map[string]any{
		"hook_event_name": "PostToolUse", "session_id": "s1", "tool_name": "mcp__bilanc__query_marts", "mcp_server": "bilanc",
		"tool_input":    map[string]any{"sql": "SECRET_MCP_SQL", "prompt": "SECRET_MCP_PROMPT"},
		"tool_response": []any{map[string]any{"type": "text", "text": "SECRET_STDOUT"}},
	}
	stop := map[string]any{
		"hook_event_name": "Stop", "session_id": "s1", "stop_hook_active": false, "prompt_id": "p1",
		"last_assistant_message": "SECRET_ASSISTANT_REPLY",
		"background_tasks":       []any{map[string]any{"description": "SECRET_BACKGROUND_TASK"}},
	}
	userPrompt := map[string]any{
		"hook_event_name": "UserPromptSubmit", "session_id": "s1", "prompt": "SECRET_PROMPT_TEXT",
	}
	cursorAfterFileEdit := map[string]any{
		"hook_event_name": "afterFileEdit", "session_id": "c1", "conversation_id": "c1", "generation_id": "g1",
		"cursor_version": "1.7.0", "model": "gpt-5", "user_email": "eng@example.com", "workspace_roots": []any{"/ws"},
		"file_path": "/ws/e.ts",
		"edits":     []any{map[string]any{"old_string": "a", "new_string": "SECRET_CURSOR_EDIT_NEW\nmore"}},
	}
	// Cursor's single-edit and whole-file-write shapes put the text at the top
	// level rather than under edits[] (ingest's lineRangeToolForEvent).
	cursorFlatEdit := map[string]any{
		"hook_event_name": "afterFileEdit", "conversation_id": "c1", "generation_id": "g1", "cursor_version": "1.7.0",
		"file_path": "/ws/f.ts", "old_string": "a", "new_string": "SECRET_CURSOR_FLAT_NEW\nline2\nline3",
	}
	cursorFlatWrite := map[string]any{
		"hook_event_name": "afterFileEdit", "conversation_id": "c1", "generation_id": "g1", "cursor_version": "1.7.0",
		"file_path": "/ws/g.ts", "content": "SECRET_CURSOR_FLAT_CONTENT\n",
	}
	// Content smuggled under allowlisted keys: objects and non-string lists
	// must be dropped even though the key itself is kept.
	nestedUnderAllowed := map[string]any{
		"hook_event_name": "PostToolUse", "session_id": "s1", "tool_name": "Read",
		"model":           map[string]any{"prompt": "SECRET_NESTED_UNDER_MODEL"},
		"file_paths":      []any{"/repo/ok.go", map[string]any{"text": "SECRET_NESTED_IN_LIST"}},
		"workspace_roots": []any{"/ws"},
		"duration_ms":     []any{"SECRET_NESTED_IN_NUMBER_KEY"},
		"tool_input":      map[string]any{"file_path": map[string]any{"text": "SECRET_NESTED_TOOL_INPUT"}, "timeout": 5},
	}
	cursorPostToolUse := map[string]any{
		"hook_event_name": "PostToolUse", "session_id": "c1", "cursor_version": "1.7.0", "generation_id": "g1",
		"tool_name": "Shell", "tool_input": map[string]any{"command": "SECRET_BASH_COMMAND", "cwd": "/ws"},
		"tool_output": "SECRET_CURSOR_TOOL_OUTPUT", "duration": 1.5,
	}

	ev := func(id string, payload any, linesAdded any) map[string]any {
		var p any
		if s, ok := payload.(string); ok {
			p = s
		} else if payload != nil {
			p = mustJSON(t, payload)
		}
		return map[string]any{
			"id": id, "org_id": "local", "session_id": "s1", "ts": "2026-11-03T10:00:00Z",
			"event_type": "PostToolUse", "agent_slug": "claude-code", "cwd": "/repo",
			"file_path": "/repo/a.go", "repo_id": "r1", "rel_file_path": "a.go",
			"lines_added": linesAdded, "lines_removed": nil, "payload": p,
		}
	}
	return map[string][]map[string]any{
		"repositories": {{"id": "r1", "org_id": "local", "remote_url": "git@github.com:x/y.git", "name": "y", "root_path": "/repo", "created_at": "2026-11-03T09:00:00Z"}},
		"sessions": {{"id": "s1", "org_id": "local", "agent_slug": "claude-code", "model_slug": "claude-fable-5-1", "repo_id": "r1",
			"branch": "main", "cwd": "/repo", "started_at": "2026-11-03T09:00:00Z", "ended_at": nil,
			"engineer_email": "eng@example.com", "engineer_name": "Eng", "input_tokens": int64(10), "output_tokens": int64(20),
			"cache_read_tokens": nil, "cache_creation_tokens": nil}},
		"events": {
			ev("e-edit", postToolUseEdit, nil),
			ev("e-bash", postToolUseBash, nil),
			ev("e-read", postToolUseRead, nil),
			ev("e-write", postToolUseWrite, nil),
			ev("e-multi", postToolUseMulti, nil),
			ev("e-mcp", postToolUseMCP, nil),
			ev("e-stop", stop, nil),
			ev("e-prompt", userPrompt, nil),
			ev("e-cursor-afe", cursorAfterFileEdit, nil),
			ev("e-cursor-ptu", cursorPostToolUse, nil),
			ev("e-cursor-edit", cursorFlatEdit, nil),
			ev("e-cursor-write", cursorFlatWrite, nil),
			ev("e-nested", nestedUnderAllowed, nil),
			ev("e-raw", "SECRET_RAW_NON_JSON", nil),
			ev("e-null", nil, nil),
			ev("e-patch", map[string]any{"hook_event_name": "PostToolUse", "tool_name": "apply_patch", "tool_input": map[string]any{"content": "SECRET_PATCH_HUNK\n\n\n"}}, int64(7)),
		},
		"commits": {{"id": "c1", "org_id": "local", "repo_id": "r1", "sha": "abc", "parent_sha": "def", "author_email": "eng@example.com",
			"author_name": "Eng", "committed_at": "2026-11-03T10:30:00Z", "branch": "main", "message": "SECRET_COMMIT_MESSAGE",
			"lines_added": int64(5), "lines_removed": int64(1), "files_changed": int64(1)}},
		"commit_files":         {{"commit_id": "c1", "file_path": "a.go", "lines_added": int64(5), "lines_removed": int64(1)}},
		"commit_sessions":      {{"commit_id": "c1", "session_id": "s1", "agent_slug": "claude-code", "model_slug": nil, "first_event_ts": "x", "last_event_ts": "y", "event_count": int64(3), "files_touched": int64(1), "lines_attributed": int64(4), "attribution_source": "event_line_ranges", "confidence": "line_range_next_file_commit"}},
		"commit_session_files": {{"commit_id": "c1", "session_id": "s1", "file_path": "a.go", "first_event_ts": "x", "last_event_ts": "y", "event_count": int64(3), "lines_attributed": int64(4)}},
		"event_line_ranges":    {{"id": "lr1", "event_id": "e-edit", "file_path": "/repo/a.go", "rel_file_path": "a.go", "blob_sha_after": "sha", "start_line": int64(1), "end_line": int64(3), "new_text_lines": int64(3), "created_at": "z"}},
		"session_prompts": {
			{"id": "p1", "org_id": "local", "session_id": "s1", "ts": "2026-11-03T09:01:00Z", "seq": int64(1), "agent_slug": "claude-code", "source": "transcript", "prompt_text": "SECRET_PROMPT_TEXT héllo", "created_at": "z"},
		},
	}
}

func TestRedactTables_NoContentSurvives(t *testing.T) {
	tables := fixtureTables(t)
	RedactTables(tables)
	wire := mustJSON(t, tables)
	for _, s := range contentStrings {
		if strings.Contains(wire, s) {
			t.Errorf("content %q survived metadata-only redaction", s)
		}
	}
	if strings.Contains(wire, "tool_response") || strings.Contains(wire, "tool_output") ||
		strings.Contains(wire, "last_assistant_message") || strings.Contains(wire, "\"prompt\":") {
		t.Errorf("content-bearing key survived: %s", wire)
	}
}

func TestRedactTables_MetadataSurvives(t *testing.T) {
	tables := fixtureTables(t)
	RedactTables(tables)

	payloadOf := func(id string) map[string]any {
		t.Helper()
		for _, row := range tables["events"] {
			if row["id"] == id {
				var m map[string]any
				if err := json.Unmarshal([]byte(row["payload"].(string)), &m); err != nil {
					t.Fatalf("%s payload not JSON: %v", id, err)
				}
				return m
			}
		}
		t.Fatalf("no event %s", id)
		return nil
	}
	rowOf := func(id string) map[string]any {
		for _, row := range tables["events"] {
			if row["id"] == id {
				return row
			}
		}
		return nil
	}

	edit := payloadOf("e-edit")
	for _, k := range []string{"hook_event_name", "session_id", "tool_name", "tool_use_id", "cwd", "transcript_path", "agent_type", "permission_mode", "effort", "duration_ms"} {
		if _, ok := edit[k]; !ok {
			t.Errorf("e-edit lost allowlisted key %s", k)
		}
	}
	if edit[redactedMarker] != true {
		t.Errorf("e-edit missing %s marker", redactedMarker)
	}
	ti := edit["tool_input"].(map[string]any)
	if ti["file_path"] != "/repo/a.go" || ti["replace_all"] != false {
		t.Errorf("tool_input allowlisted keys wrong: %v", ti)
	}
	if _, ok := ti["new_string"]; ok {
		t.Errorf("tool_input.new_string survived")
	}

	// Newline counts of the dropped text land on lines_added, mirroring the
	// server's fallback score, but never overwrite a value already there.
	cases := map[string]int64{"e-edit": 2, "e-write": 2, "e-multi": 3, "e-cursor-afe": 1, "e-cursor-edit": 2, "e-cursor-write": 1, "e-patch": 7}
	for id, want := range cases {
		if got := rowOf(id)["lines_added"]; got != want {
			t.Errorf("%s lines_added = %v, want %d", id, got, want)
		}
	}
	for _, id := range []string{"e-bash", "e-read", "e-stop", "e-prompt"} {
		if got := rowOf(id)["lines_added"]; got != nil {
			t.Errorf("%s lines_added = %v, want nil", id, got)
		}
	}

	// Cursor's phantom-session detection upstream keys on these two.
	afe := payloadOf("e-cursor-afe")
	if afe["cursor_version"] != "1.7.0" || afe["generation_id"] != "g1" || afe["file_path"] != "/ws/e.ts" {
		t.Errorf("cursor markers lost: %v", afe)
	}
	if _, ok := afe["edits"]; ok {
		t.Errorf("top-level edits survived")
	}

	// Allowlisted keys only carry scalars (or string lists for the two list
	// keys); anything else under them is dropped, not forwarded.
	nested := payloadOf("e-nested")
	for _, k := range []string{"model", "file_paths", "duration_ms"} {
		if v, ok := nested[k]; ok {
			t.Errorf("non-scalar value survived under allowlisted key %s: %v", k, v)
		}
	}
	if roots, ok := nested["workspace_roots"].([]any); !ok || len(roots) != 1 || roots[0] != "/ws" {
		t.Errorf("string list dropped from workspace_roots: %v", nested["workspace_roots"])
	}
	nti := nested["tool_input"].(map[string]any)
	if _, ok := nti["file_path"]; ok || nti["timeout"] != float64(5) {
		t.Errorf("tool_input value shapes not enforced: %v", nti)
	}

	raw := payloadOf("e-raw")
	if raw["raw_dropped"] != true || raw[redactedMarker] != true {
		t.Errorf("non-JSON payload not replaced by marker: %v", raw)
	}
	if rowOf("e-null")["payload"] != nil {
		t.Errorf("NULL payload should stay NULL")
	}

	p := tables["session_prompts"][0]
	if p["prompt_text"] != "" || p["redacted"] != true || p["prompt_chars"] != int64(24) {
		t.Errorf("session_prompts not redacted as expected: %v", p)
	}
	if p["seq"] != int64(1) || p["ts"] != "2026-11-03T09:01:00Z" || p["id"] != "p1" {
		t.Errorf("session_prompts metadata lost: %v", p)
	}

	c := tables["commits"][0]
	if c["message"] != nil || c["sha"] != "abc" || c["lines_added"] != int64(5) {
		t.Errorf("commit not redacted as expected: %v", c)
	}
}

func TestRedactTables_OtherTablesByteIdentical(t *testing.T) {
	before := fixtureTables(t)
	after := fixtureTables(t)
	RedactTables(after)
	for _, table := range []string{"repositories", "sessions", "commit_files", "commit_sessions", "commit_session_files", "event_line_ranges"} {
		if a, b := mustJSON(t, before[table]), mustJSON(t, after[table]); a != b {
			t.Errorf("%s changed under redaction:\n%s\n%s", table, a, b)
		}
	}
}

// Pins the allowlists. Adding a key here is a deliberate, reviewed widening
// of what leaves the machine in metadata-only mode.
func TestAllowlistsPinned(t *testing.T) {
	wantPayload := "agent_id,agent_type,conversation_id,cursor_version,cwd,duration,duration_ms,effort,event,file_path,file_paths,generation_id,hook_event_name,mcp_server,model,model_id,permission_mode,prompt_id,scratchpad_dir,sessionId,session_id,source,stop_hook_active,tool_name,tool_use_id,transcriptPath,transcript_path,turn_id,type,user_email,workspace_roots"
	wantToolInput := "cwd,file_path,file_paths,notebook_path,replace_all,timeout"
	if got := sortedKeys(payloadKeySet); got != wantPayload {
		t.Errorf("payload allowlist changed:\n got %s\nwant %s", got, wantPayload)
	}
	if got := sortedKeys(toolInputKeySet); got != wantToolInput {
		t.Errorf("tool_input allowlist changed:\n got %s\nwant %s", got, wantToolInput)
	}
}

func sortedKeys(m map[string]bool) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// insertion sort: tiny lists, no extra import
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return strings.Join(keys, ",")
}

func TestRedactTables_MeasuredLineColumnsPassThrough(t *testing.T) {
	// The counts ingest measures, and their per-file split, are metadata:
	// numbers and file paths, like the columns beside them. Metadata-only
	// mode must leave them alone while it strips the payload.
	fileLines := `[{"rel_file_path":"a.go","file_path":"/repo/a.go","lines_generated":1,"lines_reworked":0},` +
		`{"rel_file_path":"b.go","file_path":"/repo/b.go","lines_generated":1,"lines_reworked":1}]`
	tables := map[string][]map[string]any{"events": {{
		"id": "e1", "event_type": "PostToolUse",
		"payload":         `{"tool_name":"apply_patch","tool_input":{"command":"*** Begin Patch\n+TOP_SECRET_LINE"}}`,
		"lines_generated": int64(2), "lines_reworked": int64(1), "file_lines": fileLines,
	}}}
	RedactTables(tables)
	row := tables["events"][0]
	if row["lines_generated"] != int64(2) || row["lines_reworked"] != int64(1) || row["file_lines"] != fileLines {
		t.Errorf("measured columns changed: generated=%v reworked=%v file_lines=%v", row["lines_generated"], row["lines_reworked"], row["file_lines"])
	}
	if payload, _ := row["payload"].(string); strings.Contains(payload, "TOP_SECRET_LINE") {
		t.Errorf("payload content survived: %s", payload)
	}
}
