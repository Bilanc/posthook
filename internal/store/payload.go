package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

// Stored payload limits.
//
// A hook payload carries everything the agent knows about a tool call,
// including what the tool returned: the whole file for Read, full command
// output for Bash, the pre-edit file plus a structured patch for Edit/Write,
// base64 images for view_image. posthook never reads any of that after
// ingest — line ranges, prompts, file paths and token counts are extracted
// while the raw payload is still in memory — yet on a six-month install the
// tool output alone was 82% of the database. TrimPayload drops it before the
// row is written and caps the rest, so a stored event carries metadata and
// the agent's inputs, never what came back.
const (
	// MaxStoredStringBytes caps any single string kept in a stored payload:
	// a Write's content, a prompt, the text of a non-JSON payload.
	MaxStoredStringBytes = 256 * 1024
	// MaxStoredStderrBytes caps each status string kept from a tool response
	// (stderr, Copilot's result_type) so the dashboard can still flag a tool
	// call that failed.
	MaxStoredStderrBytes = 2 * 1024
	// TrimMarkerKey is set at the top level of every payload TrimPayload
	// changed, holding {"dropped_bytes": N} (cumulative across re-runs).
	TrimMarkerKey = "_posthook_trim"
)

// toolOutputKeys name what a tool returned: tool_response for Claude Code,
// Codex and Devin; tool_output for Cursor; tool_result for GitHub Copilot.
var toolOutputKeys = []string{"tool_response", "tool_output", "tool_result"}

// keptOutputStrings are the string members of a tool output worth keeping,
// capped at MaxStoredStderrBytes: they say whether the call failed, not what
// it returned.
var keptOutputStrings = map[string]bool{"stderr": true, "result_type": true}

// TrimPayload returns the form of a hook payload posthook stores and how many
// bytes this call dropped. Rules:
//
//   - tool_response / tool_output / tool_result: an object keeps only its
//     boolean, number and null members plus the first MaxStoredStderrBytes of
//     the strings "stderr" and "result_type"; a string, array or anything
//     else becomes null.
//   - every other value: strings longer than MaxStoredStringBytes are cut and
//     suffixed with "[...posthook truncated N bytes]", wherever they nest.
//   - non-object payloads are cut to MaxStoredStringBytes.
//
// A payload that needs no change is returned as-is with dropped == 0, so the
// function is idempotent and already-trimmed rows stay byte-identical.
func TrimPayload(raw []byte) ([]byte, int) {
	body := bytes.TrimSpace(raw)
	if len(body) == 0 || body[0] != '{' {
		return capBytes(raw)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return capBytes(raw)
	}

	dropped := 0
	for _, key := range toolOutputKeys {
		v, ok := top[key]
		if !ok {
			continue
		}
		if kept, n := trimToolOutput(v); n > 0 {
			top[key] = kept
			dropped += n
		}
	}
	for key, v := range top {
		if key == TrimMarkerKey || len(v) <= MaxStoredStringBytes {
			continue
		}
		if kept, n := truncateLargeStrings(v); n > 0 {
			top[key] = kept
			dropped += n
		}
	}
	if dropped == 0 {
		return raw, 0
	}

	total := dropped
	if prev, ok := top[TrimMarkerKey]; ok {
		var m struct {
			DroppedBytes int `json:"dropped_bytes"`
		}
		if json.Unmarshal(prev, &m) == nil {
			total += m.DroppedBytes
		}
	}
	top[TrimMarkerKey] = json.RawMessage(fmt.Sprintf(`{"dropped_bytes":%d}`, total))
	out, err := marshalNoEscape(top)
	if err != nil {
		return raw, 0
	}
	return out, dropped
}

// trimToolOutput reduces one tool_response/tool_output/tool_result value.
// Objects keep their scalar flags (interrupted, userModified, replaceAll, exit
// codes…) and a bounded stderr / result_type; anything else is replaced by null.
func trimToolOutput(v json.RawMessage) (json.RawMessage, int) {
	body := bytes.TrimSpace(v)
	if len(body) == 0 || bytes.Equal(body, []byte("null")) {
		return v, 0
	}
	if body[0] != '{' {
		return json.RawMessage("null"), len(v)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return json.RawMessage("null"), len(v)
	}
	kept := make(map[string]json.RawMessage, len(obj))
	changed := false
	for k, val := range obj {
		t := bytes.TrimSpace(val)
		switch {
		case len(t) == 0:
			changed = true
		case isScalarFlag(t):
			kept[k] = val
		case keptOutputStrings[k] && t[0] == '"':
			var s string
			if json.Unmarshal(t, &s) != nil {
				changed = true
				continue
			}
			cut := truncateString(s, MaxStoredStderrBytes)
			if cut != s {
				changed = true
			}
			enc, err := marshalNoEscape(cut)
			if err != nil {
				changed = true
				continue
			}
			kept[k] = enc
		default:
			changed = true
		}
	}
	if !changed {
		return v, 0
	}
	out, err := marshalNoEscape(kept)
	if err != nil {
		return json.RawMessage("null"), len(v)
	}
	return out, nonNegative(len(v) - len(out))
}

// isScalarFlag reports whether a raw JSON value is a boolean, null or number.
func isScalarFlag(t []byte) bool {
	if bytes.Equal(t, []byte("true")) || bytes.Equal(t, []byte("false")) || bytes.Equal(t, []byte("null")) {
		return true
	}
	return t[0] == '-' || (t[0] >= '0' && t[0] <= '9')
}

// truncateLargeStrings cuts every string longer than MaxStoredStringBytes
// inside v, however deeply nested. Only called for values whose encoded size
// already exceeds the cap, so the generic decode is rare.
func truncateLargeStrings(v json.RawMessage) (json.RawMessage, int) {
	dec := json.NewDecoder(bytes.NewReader(v))
	dec.UseNumber()
	var val any
	if err := dec.Decode(&val); err != nil {
		return v, 0
	}
	val, changed := truncateWalk(val)
	if !changed {
		return v, 0
	}
	out, err := marshalNoEscape(val)
	if err != nil {
		return v, 0
	}
	return out, nonNegative(len(v) - len(out))
}

func truncateWalk(val any) (any, bool) {
	switch x := val.(type) {
	case string:
		cut := truncateString(x, MaxStoredStringBytes)
		return cut, cut != x
	case map[string]any:
		changed := false
		for k, child := range x {
			c, ok := truncateWalk(child)
			if ok {
				x[k] = c
				changed = true
			}
		}
		return x, changed
	case []any:
		changed := false
		for i, child := range x {
			c, ok := truncateWalk(child)
			if ok {
				x[i] = c
				changed = true
			}
		}
		return x, changed
	}
	return val, false
}

// truncateString keeps the first max bytes of s (on a rune boundary) and
// records how much was cut in a trailing marker.
func truncateString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	head := truncateUTF8([]byte(s), max)
	return fmt.Sprintf("%s[...posthook truncated %d bytes]", head, len(s)-len(head))
}

// truncateUTF8 cuts b to at most max bytes without splitting a rune.
func truncateUTF8(b []byte, max int) []byte {
	if len(b) <= max {
		return b
	}
	i := max
	for i > 0 && !utf8.RuneStart(b[i]) {
		i--
	}
	return b[:i]
}

// capBytes bounds a payload that is not a JSON object (raw text, an array).
func capBytes(raw []byte) ([]byte, int) {
	if len(raw) <= MaxStoredStringBytes {
		return raw, 0
	}
	cut := truncateUTF8(raw, MaxStoredStringBytes)
	return cut, len(raw) - len(cut)
}

// marshalNoEscape encodes like json.Marshal but leaves <, > and & alone, so
// shell commands and code in the stored payload read as the agent sent them.
func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func nonNegative(n int) int {
	if n < 0 {
		return 0
	}
	return n
}
