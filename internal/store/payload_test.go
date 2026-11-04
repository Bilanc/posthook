package store

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("stored payload is not valid JSON: %v\n%s", err, b)
	}
	return m
}

func TestTrimPayloadDropsBashOutputKeepsFlagsAndStderr(t *testing.T) {
	stdout := strings.Repeat("line of output\n", 2000)
	raw := []byte(`{"hook_event_name":"PostToolUse","session_id":"s1","tool_name":"Bash",` +
		`"tool_input":{"command":"ls -la | grep <foo> && echo done","description":"list"},` +
		`"tool_response":{"stdout":` + mustJSON(stdout) + `,"stderr":"boom","interrupted":false,"isImage":false,"noOutputExpected":true,"returnCodeInterpretation":"ok"},` +
		`"transcript_path":"/tmp/t.jsonl"}`)

	out, dropped := TrimPayload(raw)
	if dropped == 0 {
		t.Fatal("expected bytes to be dropped")
	}
	m := decode(t, out)
	resp, _ := m["tool_response"].(map[string]any)
	if _, ok := resp["stdout"]; ok {
		t.Error("stdout should be dropped")
	}
	if _, ok := resp["returnCodeInterpretation"]; ok {
		t.Error("string members other than stderr should be dropped")
	}
	if resp["stderr"] != "boom" || resp["interrupted"] != false || resp["noOutputExpected"] != true {
		t.Errorf("flags/stderr not preserved: %v", resp)
	}
	// Everything outside tool_response is byte-identical, HTML left alone.
	if !bytes.Contains(out, []byte(`"command":"ls -la | grep <foo> && echo done"`)) {
		t.Errorf("tool_input altered or HTML-escaped: %s", out)
	}
	marker, _ := m[TrimMarkerKey].(map[string]any)
	if marker["dropped_bytes"].(float64) != float64(dropped) {
		t.Errorf("marker %v != dropped %d", marker, dropped)
	}
	if len(out) >= len(raw) {
		t.Errorf("trimmed payload (%d) not smaller than raw (%d)", len(out), len(raw))
	}
}

func TestTrimPayloadEditResponseKeepsOnlyFlags(t *testing.T) {
	raw := []byte(`{"tool_name":"Edit","tool_input":{"file_path":"a.go","old_string":"x","new_string":"y"},` +
		`"tool_response":{"filePath":"a.go","oldString":"x","newString":"y","originalFile":"` + strings.Repeat("x", 5000) + `",` +
		`"structuredPatch":[{"lines":["-x","+y"]}],"userModified":false,"replaceAll":true}}`)
	out, dropped := TrimPayload(raw)
	if dropped == 0 {
		t.Fatal("expected drop")
	}
	resp := decode(t, out)["tool_response"].(map[string]any)
	if len(resp) != 2 || resp["userModified"] != false || resp["replaceAll"] != true {
		t.Errorf("want only the two flags, got %v", resp)
	}
}

func TestTrimPayloadCopilotResultKeepsResultType(t *testing.T) {
	raw := []byte(`{"hook_event_name":"PostToolUse","tool_name":"Read","tool_input":{"file_path":"a.go"},` +
		`"tool_result":{"result_type":"success","text_result_for_llm":` + mustJSON(strings.Repeat("package main\n", 3000)) + `}}`)
	out, dropped := TrimPayload(raw)
	if dropped == 0 {
		t.Fatal("expected text_result_for_llm to be dropped")
	}
	res := decode(t, out)["tool_result"].(map[string]any)
	if len(res) != 1 || res["result_type"] != "success" {
		t.Errorf("want only result_type kept, got %v", res)
	}
}

func TestTrimPayloadNonObjectToolOutputBecomesNull(t *testing.T) {
	cases := map[string]string{
		"codex string":    `"` + strings.Repeat("o", 100) + `"`,
		"cursor string":   `"{\"output\":\"...\",\"exitCode\":0}"`,
		"view_image list": `[{"type":"image","data":"` + strings.Repeat("A", 100) + `"}]`,
		"number":          `42`,
	}
	for name, val := range cases {
		for _, key := range toolOutputKeys {
			raw := []byte(`{"tool_name":"x","` + key + `":` + val + `}`)
			out, dropped := TrimPayload(raw)
			if dropped != len(val) {
				t.Errorf("%s/%s: dropped %d, want %d", name, key, dropped, len(val))
			}
			if v, ok := decode(t, out)[key]; !ok || v != nil {
				t.Errorf("%s/%s: want null, got %v", name, key, v)
			}
		}
	}
}

func TestTrimPayloadNullToolResponseUnchanged(t *testing.T) {
	raw := []byte(`{"tool_name":"Read","tool_response":null,"tool_input":{"file_path":"a"}}`)
	out, dropped := TrimPayload(raw)
	if dropped != 0 || !bytes.Equal(out, raw) {
		t.Errorf("null tool_response should be left alone: %d %s", dropped, out)
	}
}

func TestTrimPayloadStderrIsCapped(t *testing.T) {
	long := strings.Repeat("é", MaxStoredStderrBytes) // 2 bytes per rune: over the cap
	raw := []byte(`{"tool_response":{"stderr":` + mustJSON(long) + `,"stdout":"x"}}`)
	out, _ := TrimPayload(raw)
	stderr := decode(t, out)["tool_response"].(map[string]any)["stderr"].(string)
	if !strings.HasSuffix(stderr, "bytes]") || !strings.Contains(stderr, "[...posthook truncated ") {
		t.Errorf("stderr not marked truncated: %q", stderr[len(stderr)-40:])
	}
	if len(stderr) > MaxStoredStderrBytes+64 {
		t.Errorf("stderr too long: %d", len(stderr))
	}
	if !strings.HasPrefix(stderr, "éé") {
		t.Errorf("rune boundary broken: %q", stderr[:4])
	}
}

func TestTrimPayloadIdempotentAndCumulative(t *testing.T) {
	raw := []byte(`{"tool_name":"Bash","tool_response":{"stdout":"` + strings.Repeat("z", 300) + `","stderr":""}}`)
	once, d1 := TrimPayload(raw)
	twice, d2 := TrimPayload(once)
	if d2 != 0 || !bytes.Equal(once, twice) {
		t.Errorf("second pass changed the payload (dropped %d)", d2)
	}
	// A trimmed payload that later grows a new blob accumulates the marker.
	var m map[string]json.RawMessage
	_ = json.Unmarshal(once, &m)
	m["tool_output"] = json.RawMessage(`"` + strings.Repeat("q", 50) + `"`)
	again, _ := json.Marshal(m)
	third, d3 := TrimPayload(again)
	marker := decode(t, third)[TrimMarkerKey].(map[string]any)
	if int(marker["dropped_bytes"].(float64)) != d1+d3 {
		t.Errorf("marker %v, want %d", marker, d1+d3)
	}
}

func TestTrimPayloadUntouchedWhenNothingToDrop(t *testing.T) {
	raw := []byte(` {"hook_event_name":"Stop","last_assistant_message":"done <ok>","transcript_path":"/t"} `)
	out, dropped := TrimPayload(raw)
	if dropped != 0 || !bytes.Equal(out, raw) {
		t.Errorf("payload should be returned as-is, got %d %s", dropped, out)
	}
	cursorEdit := []byte(`{"hook_event_name":"afterFileEdit","edits":[{"old_string":"a","new_string":"b"}],"file_path":"/x","cursor_version":"1.0"}`)
	if out, dropped := TrimPayload(cursorEdit); dropped != 0 || !bytes.Equal(out, cursorEdit) {
		t.Error("cursor afterFileEdit must be stored whole")
	}
}

func TestTrimPayloadTruncatesOversizedStringsAnywhere(t *testing.T) {
	huge := strings.Repeat("package main\n", MaxStoredStringBytes/10)
	raw := []byte(`{"tool_name":"Write","tool_input":{"file_path":"big.go","content":` + mustJSON(huge) + `},"nested":{"deep":[{"s":` + mustJSON(huge) + `}]}}`)
	out, dropped := TrimPayload(raw)
	if dropped == 0 {
		t.Fatal("expected truncation")
	}
	m := decode(t, out)
	content := m["tool_input"].(map[string]any)["content"].(string)
	if len(content) > MaxStoredStringBytes+64 || !strings.Contains(content, "[...posthook truncated ") {
		t.Errorf("content not truncated: len=%d", len(content))
	}
	if m["tool_input"].(map[string]any)["file_path"] != "big.go" {
		t.Error("sibling key lost")
	}
	deep := m["nested"].(map[string]any)["deep"].([]any)[0].(map[string]any)["s"].(string)
	if len(deep) > MaxStoredStringBytes+64 {
		t.Error("nested string not truncated")
	}
}

func TestTrimPayloadNonJSONIsCapped(t *testing.T) {
	small := []byte("not json at all")
	if out, d := TrimPayload(small); d != 0 || !bytes.Equal(out, small) {
		t.Error("small raw text should pass through")
	}
	big := []byte(strings.Repeat("x", MaxStoredStringBytes+10))
	out, d := TrimPayload(big)
	if d != 10 || len(out) != MaxStoredStringBytes {
		t.Errorf("raw text cap: dropped %d len %d", d, len(out))
	}
	if out, d := TrimPayload(nil); d != 0 || out != nil {
		t.Error("nil passes through")
	}
}

func mustJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
