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

func TestPiPatchHunks(t *testing.T) {
	cases := []struct {
		name  string
		patch string
		want  []any
		ok    bool
	}{
		{
			name:  "one hunk",
			patch: "--- f\n+++ f\n@@ -1,3 +1,5 @@\n package seat\n \n-func Remove() {}\n+func Remove() {\n+\tprorate()\n+}\n",
			want: []any{map[string]any{"oldStart": float64(1), "newStart": float64(1), "lines": []any{
				" package seat", " ", "-func Remove() {}", "+func Remove() {", "+\tprorate()", "+}",
			}}},
			ok: true,
		},
		{
			name:  "two hunks, a removed line that looks like a header",
			patch: "--- f\n+++ f\n@@ -2 +2 @@\n-b\n+B\n@@ -11,2 +11,3 @@\n---k\n+K1\n+K2\n l\n",
			want: []any{
				map[string]any{"oldStart": float64(2), "newStart": float64(2), "lines": []any{"-b", "+B"}},
				map[string]any{"oldStart": float64(11), "newStart": float64(11), "lines": []any{"---k", "+K1", "+K2", " l"}},
			},
			ok: true,
		},
		{
			name:  "no newline at end of file",
			patch: "--- f\n+++ f\n@@ -1,2 +1,2 @@\n x\n-y\n\\ No newline at end of file\n+z\n\\ No newline at end of file\n",
			want: []any{map[string]any{"oldStart": float64(1), "newStart": float64(1), "lines": []any{
				" x", "-y", "\\ No newline at end of file", "+z", "\\ No newline at end of file",
			}}},
			ok: true,
		},
		{name: "no hunks", patch: "--- f\n+++ f\n", ok: false},
		{name: "a line outside any hunk", patch: "--- f\n+++ f\n x\n", ok: false},
		{name: "an unmarked line in a hunk", patch: "@@ -1 +1 @@\nplain\n", ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := piPatchHunks(tc.patch)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (hunks %#v)", ok, tc.ok, got)
			}
			if tc.ok && !reflect.DeepEqual(got, tc.want) {
				t.Errorf("hunks = %#v\nwant %#v", got, tc.want)
			}
		})
	}
}

// An edit's patch becomes Claude Code's structuredPatch, the shape ingest
// already counts exactly; a patch it cannot read is dropped and the edit
// falls back to diffing its strings.
func TestNormalizePiPayloadEditPatch(t *testing.T) {
	edit := func(resp map[string]any) map[string]any {
		return normalizePiPayload(map[string]any{
			"tool_name":     "edit",
			"tool_input":    map[string]any{"path": "f", "edits": []any{map[string]any{"oldText": "a", "newText": "b"}}},
			"tool_response": resp,
		})
	}
	got := edit(map[string]any{"is_error": false, "patch": "@@ -1 +1 @@\n-a\n+b\n"})
	resp := got["tool_response"].(map[string]any)
	if _, has := resp["patch"]; has {
		t.Error("patch text kept alongside the structured hunks")
	}
	if hunks, ok := resp["structuredPatch"].([]any); !ok || len(hunks) != 1 {
		t.Errorf("structuredPatch = %#v", resp["structuredPatch"])
	}
	if resp["is_error"] != false {
		t.Errorf("is_error lost: %#v", resp)
	}

	got = edit(map[string]any{"is_error": false, "patch": "not a patch"})
	resp = got["tool_response"].(map[string]any)
	if _, has := resp["structuredPatch"]; has {
		t.Error("unreadable patch turned into hunks")
	}
	if _, has := resp["patch"]; has {
		t.Error("unreadable patch kept")
	}
}

func TestAddBOMToFirstLine(t *testing.T) {
	const bom = "\uFEFF"
	hunks := func(oldStart, newStart float64, lines ...any) []any {
		return []any{map[string]any{"oldStart": oldStart, "newStart": newStart, "lines": lines}}
	}
	for _, tc := range []struct {
		name     string
		in, want []any
	}{
		{"changed first line", hunks(1, 1, "-a", "+A", " b"), hunks(1, 1, "-"+bom+"a", "+"+bom+"A", " b")},
		{"context first line", hunks(1, 1, " a", "+x"), hunks(1, 1, " "+bom+"a", "+x")},
		{"line inserted above line 1", hunks(1, 1, "+x", " a"), hunks(1, 1, "+"+bom+"x", " "+bom+"a")},
		{"hunk below line 1", hunks(5, 5, " e", "-f", "+F"), hunks(5, 5, " e", "-f", "+F")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addBOMToFirstLine(tc.in)
			if !reflect.DeepEqual(tc.in, tc.want) {
				t.Errorf("got %#v\nwant %#v", tc.in, tc.want)
			}
		})
	}
}
