package ingest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const patchText = "*** Begin Patch\n*** Update File: docs/posthook/mdm-deployment.mdx\n@@\n <Note>\n-  old line\n+  new line\n </Note>\n*** End Patch\n"

func parse(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestNormalizeCopilotPayloadCLIApplyPatchClaudeMapped(t *testing.T) {
	// PascalCase registration: Copilot CLI reports apply_patch as "Edit" and
	// passes the patch text as the whole tool_input.
	raw, _ := json.Marshal(map[string]any{
		"hook_event_name": "PostToolUse", "session_id": "60a0ed8e", "cwd": "/repo",
		"tool_name": "Edit", "tool_input": patchText,
		"tool_result": map[string]any{"result_type": "success", "text_result_for_llm": "Modified 1 file(s)"},
	})
	p := normalizeCopilotPayload(parse(t, string(raw)))
	if p["tool_name"] != "apply_patch" {
		t.Fatalf("tool_name = %v", p["tool_name"])
	}
	files := applyPatchFilesForPayload(p)
	if len(files) != 1 || files[0].FilePath != "docs/posthook/mdm-deployment.mdx" || files[0].LinesAdded != 1 || files[0].LinesRemoved != 1 {
		t.Fatalf("apply patch files = %+v", files)
	}
	if extractFilePath(p) != "" {
		t.Error("apply_patch carries no tool_input.file_path; the first patched file is picked up separately")
	}
}

func TestNormalizeCopilotPayloadVSCodeApplyPatch(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"hook_event_name": "PostToolUse", "session_id": "5d2cab69", "cwd": "/repo",
		"tool_name":  "apply_patch",
		"tool_input": map[string]any{"input": patchText, "explanation": "Document the uninstall flow", "toolUses": []any{}},
	})
	p := normalizeCopilotPayload(parse(t, string(raw)))
	if p["tool_name"] != "apply_patch" {
		t.Fatalf("tool_name = %v", p["tool_name"])
	}
	if files := applyPatchFilesForPayload(p); len(files) != 1 {
		t.Fatalf("apply patch files = %+v", files)
	}
}

func TestNormalizeCopilotPayloadCamelCaseAndJSONString(t *testing.T) {
	// camelCase registration: no event name, camelCase fields, and toolArgs
	// as a JSON-encoded string (the shape Copilot's own docs test with).
	p := normalizeCopilotPayload(parse(t, `{"sessionId":"abc","timestamp":1704614400000,"cwd":"/repo","toolName":"create","toolArgs":"{\"path\":\"src/new.go\",\"file_text\":\"package x\\n\"}","toolResult":{"resultType":"success","textResultForLlm":"ok"}}`))
	if p["hook_event_name"] != "PostToolUse" {
		t.Errorf("hook_event_name = %v", p["hook_event_name"])
	}
	if p["session_id"] != "abc" {
		t.Errorf("session_id = %v", p["session_id"])
	}
	if p["tool_name"] != "Write" {
		t.Errorf("tool_name = %v", p["tool_name"])
	}
	ti, _ := p["tool_input"].(map[string]any)
	if ti["file_path"] != "src/new.go" || ti["content"] != "package x\n" {
		t.Errorf("tool_input = %v", ti)
	}
	if extractFilePath(p) != "src/new.go" {
		t.Errorf("extractFilePath = %q", extractFilePath(p))
	}
	if lineRangeToolForEvent(copilotAgentSlug, "PostToolUse", p) != "Write" {
		t.Error("Write should be line-range attributed")
	}
}

func TestNormalizeCopilotPayloadToolShapes(t *testing.T) {
	cases := []struct {
		name      string
		toolName  string
		toolInput any
		wantName  string
		wantInput map[string]any
	}{
		{"cli edit", "edit", map[string]any{"path": "a.go", "old_str": "x", "new_str": "y"},
			"Edit", map[string]any{"file_path": "a.go", "old_string": "x", "new_string": "y"}},
		{"cli str_replace_editor create", "str_replace_editor", map[string]any{"command": "create", "path": "a.go", "file_text": "body"},
			"Write", map[string]any{"file_path": "a.go", "content": "body"}},
		{"cli create claude-mapped", "Write", map[string]any{"path": "a.go", "file_text": "body"},
			"Write", map[string]any{"file_path": "a.go", "content": "body"}},
		{"vscode replace_string_in_file", "replace_string_in_file", map[string]any{"filePath": "/w/a.go", "oldString": "x", "newString": "y"},
			"Edit", map[string]any{"file_path": "/w/a.go", "old_string": "x", "new_string": "y"}},
		{"vscode create_file", "create_file", map[string]any{"filePath": "/w/a.go", "content": "body"},
			"Write", map[string]any{"file_path": "/w/a.go", "content": "body"}},
		{"vscode multi replace one file", "multi_replace_string_in_file",
			map[string]any{"replacements": []any{
				map[string]any{"filePath": "/w/a.go", "oldString": "x", "newString": "y"},
				map[string]any{"filePath": "/w/a.go", "oldString": "p", "newString": "q"},
			}},
			"MultiEdit", map[string]any{"file_path": "/w/a.go", "edits": []any{
				map[string]any{"old_string": "x", "new_string": "y"},
				map[string]any{"old_string": "p", "new_string": "q"},
			}}},
		{"claude-native edit passes through", "Edit", map[string]any{"file_path": "a.go", "old_string": "x", "new_string": "y"},
			"Edit", map[string]any{"file_path": "a.go", "old_string": "x", "new_string": "y"}},
		{"cli bash claude-mapped", "Bash", map[string]any{"command": "ls", "description": "list"},
			"Bash", map[string]any{"command": "ls", "description": "list"}},
		{"vscode run_in_terminal", "run_in_terminal", map[string]any{"command": "go test ./...", "explanation": "x", "mode": "sync"},
			"Bash", map[string]any{"command": "go test ./...", "explanation": "x", "mode": "sync"}},
		{"view keeps name, exposes path", "view", map[string]any{"path": "a.go", "view_range": []any{1.0, 2.0}},
			"view", map[string]any{"path": "a.go", "view_range": []any{1.0, 2.0}, "file_path": "a.go"}},
		{"vscode read_file keeps name, exposes path", "read_file", map[string]any{"filePath": "/w/a.go", "startLine": 1.0, "endLine": 200.0},
			"read_file", map[string]any{"filePath": "/w/a.go", "startLine": 1.0, "endLine": 200.0, "file_path": "/w/a.go"}},
		{"vscode list_dir is a browse, no file_path", "list_dir", map[string]any{"path": "/w/docs"},
			"list_dir", map[string]any{"path": "/w/docs"}},
		{"cli glob is a browse, no file_path", "glob", map[string]any{"pattern": "**/*.md", "paths": []any{"/w"}},
			"glob", map[string]any{"pattern": "**/*.md", "paths": []any{"/w"}}},
		{"vscode memory tool is a browse, no file_path", "memory", map[string]any{"command": "view", "path": "/memories/x.md"},
			"memory", map[string]any{"command": "view", "path": "/memories/x.md"}},
		{"insert edit keeps name, exposes path", "insert_edit_into_file", map[string]any{"filePath": "/w/a.go", "code": "// ...existing code...", "explanation": "e"},
			"insert_edit_into_file", map[string]any{"filePath": "/w/a.go", "code": "// ...existing code...", "explanation": "e", "file_path": "/w/a.go"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := map[string]any{"hook_event_name": "PostToolUse", "tool_name": c.toolName, "tool_input": c.toolInput}
			p = normalizeCopilotPayload(p)
			if p["tool_name"] != c.wantName {
				t.Errorf("tool_name = %v, want %v", p["tool_name"], c.wantName)
			}
			if !reflect.DeepEqual(p["tool_input"], c.wantInput) {
				t.Errorf("tool_input = %#v, want %#v", p["tool_input"], c.wantInput)
			}
		})
	}
}

func TestNormalizeCopilotPayloadLeavesNonToolEventsAlone(t *testing.T) {
	p := normalizeCopilotPayload(parse(t, `{"hook_event_name":"Stop","session_id":"s","cwd":"/r","transcript_path":"/x/events.jsonl","stop_reason":"end_turn","stop_hook_active":false}`))
	if _, ok := p["tool_name"]; ok {
		t.Error("Stop payload should not gain a tool_name")
	}
	if p["transcript_path"] != "/x/events.jsonl" {
		t.Error("transcript_path must survive")
	}
	if p["hook_event_name"] != "Stop" {
		t.Error("explicit event name must survive")
	}
}

func TestCopilotMultiFileReplacementsBecomeApplyPatch(t *testing.T) {
	p := normalizeCopilotPayload(map[string]any{"hook_event_name": "PostToolUse", "tool_name": "multi_replace_string_in_file",
		"tool_input": map[string]any{"replacements": []any{
			map[string]any{"filePath": "/w/a.go", "oldString": "x", "newString": "y\nz"},
			map[string]any{"filePath": "/w/b.go", "oldString": "p\nq", "newString": "r"},
			map[string]any{"filePath": "/w/a.go", "oldString": "m", "newString": "n"},
		}}})
	if p["tool_name"] != "apply_patch" || p["copilot_tool_name"] != "multi_replace_string_in_file" {
		t.Fatalf("tool_name = %v (from %v)", p["tool_name"], p["copilot_tool_name"])
	}
	files := applyPatchFilesForPayload(p)
	if len(files) != 2 {
		t.Fatalf("files = %+v", files)
	}
	if files[0].FilePath != "/w/a.go" || len(files[0].Edits) != 2 || files[0].LinesAdded != 3 || files[0].LinesRemoved != 2 {
		t.Errorf("a.go = %+v", files[0])
	}
	if files[0].Edits[0].NewString != "y\nz" || files[0].Edits[1].NewString != "n" {
		t.Errorf("a.go edits = %+v", files[0].Edits)
	}
	if files[1].FilePath != "/w/b.go" || len(files[1].Edits) != 1 || files[1].LinesAdded != 1 || files[1].LinesRemoved != 2 {
		t.Errorf("b.go = %+v", files[1])
	}
}

func TestNormalizeCopilotPayloadRecordsOriginalToolName(t *testing.T) {
	p := normalizeCopilotPayload(map[string]any{"tool_name": "create", "tool_input": map[string]any{"path": "a.go", "file_text": "x"}})
	if p["tool_name"] != "Write" || p["copilot_tool_name"] != "create" {
		t.Errorf("got %v / %v", p["tool_name"], p["copilot_tool_name"])
	}
	p = normalizeCopilotPayload(map[string]any{"tool_name": "Edit", "tool_input": map[string]any{"file_path": "a.go", "old_string": "x", "new_string": "y"}})
	if _, ok := p["copilot_tool_name"]; ok {
		t.Error("an already-canonical name should not be recorded as renamed")
	}
}

func TestIsCopilotPayload(t *testing.T) {
	yes := []string{
		`{"transcript_path":"/Users/x/.copilot/session-state/60a0ed8e/events.jsonl"}`,
		`{"transcript_path":"/opt/copilot-home/session-state/60a0ed8e/events.jsonl"}`,
		`{"transcript_path":"/Users/x/Library/Application Support/Code/User/workspaceStorage/abc/GitHub.copilot-chat/transcripts/5d2cab69.jsonl"}`,
		`{"transcriptPath":"C:\\Users\\x\\AppData\\Roaming\\Code\\User\\workspaceStorage\\abc\\GitHub.copilot-chat\\transcripts\\5d.jsonl"}`,
		`{"sessionId":"s","toolName":"bash","toolArgs":{"command":"ls"}}`,
		`{"tool_name":"Edit","tool_result":{"result_type":"success","text_result_for_llm":"ok"}}`,
	}
	no := []string{
		`{"transcript_path":"/Users/x/.claude/projects/-Users-x-repo/abc.jsonl","tool_name":"Edit","tool_response":{"filePath":"a.go"}}`,
		`{"transcript_path":"/Users/x/.codex/sessions/2026/11/03/rollout-abc.jsonl"}`,
		`{"hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{"command":"ls"},"cursor_version":"1.2"}`,
		`{}`,
	}
	for _, raw := range yes {
		if !isCopilotPayload(parse(t, raw)) {
			t.Errorf("expected Copilot payload: %s", raw)
		}
	}
	for _, raw := range no {
		if isCopilotPayload(parse(t, raw)) {
			t.Errorf("unexpected Copilot payload: %s", raw)
		}
	}
}

func TestNormalizeEventTypeCopilot(t *testing.T) {
	cases := map[string]string{
		"postToolUse": "PostToolUse", "preToolUse": "PreToolUse",
		"userPromptSubmitted": "UserPromptSubmit", "UserPromptSubmit": "UserPromptSubmit",
		"agentStop": "Stop", "Stop": "Stop",
		"sessionEnd": "SessionEnd", "sessionStart": "SessionStart",
		"afterFileEdit": "afterFileEdit",
	}
	for in, want := range cases {
		if got := normalizeEventType(in); got != want {
			t.Errorf("normalizeEventType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCopilotTranscriptPathDiscovery(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("COPILOT_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	// The supplied path wins when it exists.
	given := filepath.Join(home, "given.jsonl")
	if err := os.WriteFile(given, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := copilotTranscriptPath(given, "sid"); got != given {
		t.Errorf("existing given path should win, got %q", got)
	}
	if got := copilotTranscriptPath(filepath.Join(home, "missing.jsonl"), "nope"); got != "" {
		t.Errorf("nothing on disk should yield empty, got %q", got)
	}

	// Copilot CLI session state.
	cli := filepath.Join(home, ".copilot", "session-state", "60a0ed8e", "events.jsonl")
	if err := os.MkdirAll(filepath.Dir(cli), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cli, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := copilotTranscriptPath("", "60a0ed8e"); got != cli {
		t.Errorf("CLI discovery = %q, want %q", got, cli)
	}
	if got := copilotTranscriptPath("", "../60a0ed8e"); got != "" {
		t.Errorf("path-like session ids must not be used for discovery, got %q", got)
	}

	// VS Code workspace storage (the platform-specific user dir is derived
	// from the same HOME / XDG_CONFIG_HOME overrides).
	for _, dir := range vscodeUserDirsForTest() {
		vs := filepath.Join(dir, "workspaceStorage", "b21feb07", "GitHub.copilot-chat", "transcripts", "5d2cab69.jsonl")
		if err := os.MkdirAll(filepath.Dir(vs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(vs, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := copilotTranscriptPath("", "5d2cab69"); got != vs {
			t.Errorf("VS Code discovery = %q, want %q", got, vs)
		}
		break
	}
}
