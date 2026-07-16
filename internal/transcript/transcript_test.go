package transcript

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTranscript(t *testing.T, lines string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(path, []byte(lines), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return path
}

func TestExtractUserPromptsClaude(t *testing.T) {
	path := writeTranscript(t, `
{"type":"user","timestamp":"2026-07-16T10:00:00Z","message":{"role":"user","content":"add proration when a seat is removed mid-cycle"}}
{"type":"assistant","timestamp":"2026-07-16T10:00:05Z","message":{"id":"m1","model":"claude-fable-5","role":"assistant","content":[{"type":"text","text":"On it."}]}}
{"type":"user","timestamp":"2026-07-16T10:01:00Z","message":{"role":"user","content":[{"tool_use_id":"t1","type":"tool_result","content":"file contents"}]}}
{"type":"user","timestamp":"2026-07-16T10:02:00Z","isMeta":true,"message":{"role":"user","content":"meta noise"}}
{"type":"user","timestamp":"2026-07-16T10:03:00Z","isSidechain":true,"message":{"role":"user","content":"subagent task prompt"}}
{"type":"user","timestamp":"2026-07-16T10:04:00Z","message":{"role":"user","content":"<command-name>/clear</command-name>"}}
{"type":"user","timestamp":"2026-07-16T10:05:00Z","message":{"role":"user","content":[{"type":"text","text":"credit should round to cents"}]}}
`)

	prompts := ExtractUserPrompts(path)
	if len(prompts) != 2 {
		t.Fatalf("expected 2 prompts, got %d: %+v", len(prompts), prompts)
	}
	if prompts[0].Text != "add proration when a seat is removed mid-cycle" {
		t.Errorf("unexpected first prompt: %q", prompts[0].Text)
	}
	if prompts[1].Text != "credit should round to cents" {
		t.Errorf("unexpected second prompt: %q", prompts[1].Text)
	}
	if prompts[0].TS != "2026-07-16T10:00:00Z" {
		t.Errorf("unexpected first prompt ts: %q", prompts[0].TS)
	}
}

func TestExtractUserPromptsCodexRollout(t *testing.T) {
	// Codex writes the typed prompt both as a response_item user message
	// (alongside environment-context noise) and as an event_msg user_message;
	// only the latter should be extracted, once.
	path := writeTranscript(t, `
{"type":"session_meta","timestamp":"2026-07-16T09:59:59Z"}
{"type":"response_item","timestamp":"2026-07-16T10:00:00Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>\n<cwd>/tmp</cwd>\n</environment_context>"}]}}
{"type":"response_item","timestamp":"2026-07-16T10:00:00Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"❯ fix the flaky login test"}]}}
{"type":"event_msg","timestamp":"2026-07-16T10:00:00Z","payload":{"type":"user_message","message":"❯ fix the flaky login test"}}
{"type":"event_msg","timestamp":"2026-07-16T10:05:00Z","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"output_tokens":50}}}}
{"type":"event_msg","timestamp":"2026-07-16T10:10:00Z","payload":{"type":"user_message","message":"now make it parallel-safe"}}
`)

	prompts := ExtractUserPrompts(path)
	if len(prompts) != 2 {
		t.Fatalf("expected 2 prompts, got %d: %+v", len(prompts), prompts)
	}
	if prompts[0].Text != "fix the flaky login test" {
		t.Errorf("expected ❯ prefix stripped, got %q", prompts[0].Text)
	}
	if prompts[1].Text != "now make it parallel-safe" {
		t.Errorf("unexpected second prompt: %q", prompts[1].Text)
	}
}

func TestExtractUserPromptsMissingFile(t *testing.T) {
	if prompts := ExtractUserPrompts(filepath.Join(t.TempDir(), "nope.jsonl")); prompts != nil {
		t.Fatalf("expected nil for missing file, got %+v", prompts)
	}
}
