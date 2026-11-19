package ingest

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/bilanc/posthook/internal/paths"
)

const piAgentSlug = "pi"

// Pi (pi.dev) has no hook config file: posthook's Pi extension
// (installers/pi) subscribes to Pi's extension events and pipes one
// Claude-shaped payload per event into `posthook ingest --agent pi`:
//
//	before_agent_start (a user prompt)  -> UserPromptSubmit (no prompt text)
//	tool_result                         -> PostToolUse {tool_name, tool_input, tool_use_id}
//	agent_settled (Pi will not continue) -> Stop {transcript_path}
//
// every one carrying session_id, cwd, model and transcript_path. tool_name and
// tool_input are Pi's own, so normalizePiPayload maps the built-in file tools
// onto the canonical ones the rest of ingest handles:
//
//	edit  {path, edits: [{oldText, newText}]}  -> Edit / MultiEdit {file_path, old_string, new_string | edits}
//	write {path, content}                      -> Write {file_path, content}
//	read  {path, offset?, limit?}               -> Read {file_path, offset?, limit?}
//	bash  {command, timeout?}                  -> Bash {command, timeout?}
//
// Pi's own name is kept under pi_tool_name. An edit's result also carries the
// unified patch Pi computed from whole lines (details.patch, sent as
// tool_response.patch), with the file as it was before the edit
// (tool_response.originalFile). The patch is turned into Claude Code's
// structuredPatch, so the edit is counted and placed exactly as Claude
// Code's are, its post-image rebuilt from originalFile. (oldText/newText are
// substrings and can cut through lines, which a string diff would credit
// only in part.) Like every tool_response, both are dropped before the event
// is stored. Typed
// prompts, model and token usage come from the Pi session file at Stop
// (transcript/pi.go).

// normalizePiPayload rewrites a Pi payload in place into the canonical form
// and returns it. Tools without a canonical counterpart, and edits whose
// arguments are not the documented shape, are left as Pi sent them.
func normalizePiPayload(payload map[string]any) map[string]any {
	toolName, _ := payload["tool_name"].(string)
	input, _ := payload["tool_input"].(map[string]any)
	if toolName == "" || input == nil {
		return payload
	}
	name, canonical, ok := canonicalPiTool(toolName, input)
	if !ok {
		return payload
	}
	payload["pi_tool_name"] = toolName
	payload["tool_name"] = name
	payload["tool_input"] = canonical
	if resp, ok := payload["tool_response"].(map[string]any); ok && toolName == "edit" {
		if patch, ok := resp["patch"].(string); ok {
			delete(resp, "patch")
			if hunks, ok := piPatchHunks(patch); ok {
				resp["structuredPatch"] = hunks
			}
		}
	}
	return payload
}

var piHunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// piPatchHunks reads a unified patch (as Pi's edit tool reports it, with
// "--- path" / "+++ path" headers) into structuredPatch hunks:
// {oldStart, newStart, lines}, each line keeping its ' ', '+', '-' or '\'
// marker. ok is false for a patch with no hunks or a line it cannot place,
// so a malformed patch is never half-used.
func piPatchHunks(patch string) ([]any, bool) {
	var hunks []any
	var lines []any
	flush := func() {
		if hunks != nil {
			hunks[len(hunks)-1].(map[string]any)["lines"] = lines
		}
	}
	for _, line := range strings.Split(strings.TrimSuffix(patch, "\n"), "\n") {
		if m := piHunkHeader.FindStringSubmatch(line); m != nil {
			flush()
			oldStart, _ := strconv.Atoi(m[1])
			newStart, _ := strconv.Atoi(m[2])
			hunks = append(hunks, map[string]any{"oldStart": float64(oldStart), "newStart": float64(newStart)})
			lines = []any{}
			continue
		}
		if hunks == nil {
			// File headers come before the first hunk; nothing else may.
			if strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ ") || strings.HasPrefix(line, "Index: ") || strings.HasPrefix(line, "====") {
				continue
			}
			return nil, false
		}
		if line == "" || !strings.ContainsRune(" +-\\", rune(line[0])) {
			return nil, false
		}
		lines = append(lines, line)
	}
	if hunks == nil {
		return nil, false
	}
	flush()
	return hunks, true
}

func canonicalPiTool(toolName string, input map[string]any) (string, map[string]any, bool) {
	switch toolName {
	case "bash":
		return "Bash", input, true
	case "read":
		path, ok := piPath(input)
		if !ok {
			return "", nil, false
		}
		out := map[string]any{"file_path": path}
		for _, k := range []string{"offset", "limit"} {
			if v, ok := input[k]; ok {
				out[k] = v
			}
		}
		return "Read", out, true
	case "write":
		path, ok := piPath(input)
		content, hasContent := input["content"].(string)
		if !ok || !hasContent {
			return "", nil, false
		}
		return "Write", map[string]any{"file_path": path, "content": content}, true
	case "edit":
		path, ok := piPath(input)
		if !ok {
			return "", nil, false
		}
		edits := piEdits(input)
		switch len(edits) {
		case 0:
			return "", nil, false
		case 1:
			return "Edit", map[string]any{"file_path": path, "old_string": edits[0][0], "new_string": edits[0][1]}, true
		}
		list := make([]any, len(edits))
		for i, e := range edits {
			list[i] = map[string]any{"old_string": e[0], "new_string": e[1]}
		}
		return "MultiEdit", map[string]any{"file_path": path, "edits": list}, true
	}
	return "", nil, false
}

// piEdits returns an edit call's replacements as (oldText, newText) pairs:
// the edits array, plus the single top-level pair older Pi releases sent.
func piEdits(input map[string]any) [][2]string {
	var out [][2]string
	if arr, ok := input["edits"].([]any); ok {
		for _, e := range arr {
			m, ok := e.(map[string]any)
			if !ok {
				continue
			}
			oldText, okOld := m["oldText"].(string)
			newText, okNew := m["newText"].(string)
			if okOld && okNew {
				out = append(out, [2]string{oldText, newText})
			}
		}
	}
	oldText, okOld := input["oldText"].(string)
	newText, okNew := input["newText"].(string)
	if okOld && okNew {
		out = append(out, [2]string{oldText, newText})
	}
	return out
}

// piPath returns a tool call's path the way Pi resolves it: a leading "@"
// (the model quoting a file reference) is dropped and "~" is the home
// directory. A relative path stays relative; ingest joins it to the cwd.
func piPath(input map[string]any) (string, bool) {
	p, ok := input["path"].(string)
	if !ok || strings.TrimSpace(p) == "" {
		return "", false
	}
	p = strings.TrimPrefix(p, "@")
	switch {
	case p == "~":
		p = paths.Home()
	case strings.HasPrefix(p, "~/"):
		p = filepath.Join(paths.Home(), p[2:])
	}
	return p, true
}
