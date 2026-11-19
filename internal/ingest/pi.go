package ingest

import (
	"path/filepath"
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
// Pi's own name is kept under pi_tool_name. Typed prompts, model and token
// usage come from the Pi session file at Stop (transcript/pi.go).

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
	return payload
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
