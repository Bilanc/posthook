package ingest

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestNormalizePiPayload(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cases := []struct {
		name      string
		toolName  string
		input     any
		wantTool  string
		wantInput map[string]any
	}{
		{
			name:     "edit with one replacement becomes Edit",
			toolName: "edit",
			input:    map[string]any{"path": "src/seat.go", "edits": []any{map[string]any{"oldText": "a\n", "newText": "b\nc\n"}}},
			wantTool: "Edit",
			wantInput: map[string]any{
				"file_path": "src/seat.go", "old_string": "a\n", "new_string": "b\nc\n",
			},
		},
		{
			name:     "edit with several replacements becomes MultiEdit",
			toolName: "edit",
			input: map[string]any{"path": "/abs/seat.go", "edits": []any{
				map[string]any{"oldText": "one", "newText": "1"},
				map[string]any{"oldText": "two", "newText": "2"},
			}},
			wantTool: "MultiEdit",
			wantInput: map[string]any{
				"file_path": "/abs/seat.go",
				"edits": []any{
					map[string]any{"old_string": "one", "new_string": "1"},
					map[string]any{"old_string": "two", "new_string": "2"},
				},
			},
		},
		{
			name:      "legacy top-level oldText/newText edit",
			toolName:  "edit",
			input:     map[string]any{"path": "a.txt", "oldText": "x", "newText": "y"},
			wantTool:  "Edit",
			wantInput: map[string]any{"file_path": "a.txt", "old_string": "x", "new_string": "y"},
		},
		{
			name:      "write becomes Write",
			toolName:  "write",
			input:     map[string]any{"path": "new.go", "content": "package x\n"},
			wantTool:  "Write",
			wantInput: map[string]any{"file_path": "new.go", "content": "package x\n"},
		},
		{
			name:      "home and @ prefixes resolve the way Pi resolves them",
			toolName:  "write",
			input:     map[string]any{"path": "@~/notes.md", "content": "hi\n"},
			wantTool:  "Write",
			wantInput: map[string]any{"file_path": filepath.Join(home, "notes.md"), "content": "hi\n"},
		},
		{
			name:      "read becomes Read",
			toolName:  "read",
			input:     map[string]any{"path": "a.go", "offset": float64(10), "limit": float64(5)},
			wantTool:  "Read",
			wantInput: map[string]any{"file_path": "a.go", "offset": float64(10), "limit": float64(5)},
		},
		{
			name:      "bash becomes Bash",
			toolName:  "bash",
			input:     map[string]any{"command": "go test ./...", "timeout": float64(60)},
			wantTool:  "Bash",
			wantInput: map[string]any{"command": "go test ./...", "timeout": float64(60)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizePiPayload(map[string]any{
				"hook_event_name": "PostToolUse", "tool_name": tc.toolName, "tool_input": tc.input,
			})
			if got["tool_name"] != tc.wantTool {
				t.Errorf("tool_name = %v, want %s", got["tool_name"], tc.wantTool)
			}
			if got["pi_tool_name"] != tc.toolName {
				t.Errorf("pi_tool_name = %v, want %s", got["pi_tool_name"], tc.toolName)
			}
			if !reflect.DeepEqual(got["tool_input"], tc.wantInput) {
				t.Errorf("tool_input = %#v\nwant %#v", got["tool_input"], tc.wantInput)
			}
		})
	}
}

// Tools Pi has that posthook has no canonical name for (grep, find, ls,
// extension tools) are stored as Pi sent them, and an edit whose arguments
// are not the shape Pi documents is left alone rather than guessed at.
func TestNormalizePiPayloadLeavesOtherToolsAlone(t *testing.T) {
	for _, payload := range []map[string]any{
		{"tool_name": "ls", "tool_input": map[string]any{"path": "src"}},
		{"tool_name": "subagent", "tool_input": map[string]any{"task": "x"}},
		{"tool_name": "edit", "tool_input": map[string]any{"path": "a.go"}},
		{"tool_name": "edit", "tool_input": "not an object"},
		{"hook_event_name": "Stop"},
	} {
		before := map[string]any{}
		for k, v := range payload {
			before[k] = v
		}
		got := normalizePiPayload(payload)
		if !reflect.DeepEqual(got, before) {
			t.Errorf("normalizePiPayload(%v) = %v, want it unchanged", before, got)
		}
	}
}
