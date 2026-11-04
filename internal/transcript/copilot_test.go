package transcript

import "testing"

// Trimmed from a real Copilot CLI 1.0.105 session (Auto mode resolved to
// GPT-6 Luna). Usage inputTokens includes the cache traffic, hence the
// uncached remainder of 3 per call.
const copilotCLIStream = `
{"type":"session.start","data":{"sessionId":"60a0ed8e","version":1,"producer":"copilot-agent","copilotVersion":"1.0.105","startTime":"2026-11-04T07:15:20.338Z","context":{"cwd":"/Users/sam/Desktop/Bilanc"}},"id":"e1","timestamp":"2026-11-04T07:15:20.338Z","parentId":null}
{"type":"session.model_change","data":{"cause":"initial_resolution","source":"automatic","newModel":"auto","reasoningEffort":null},"id":"e2","timestamp":"2026-11-04T07:15:20.400Z","parentId":"e1"}
{"type":"user.message","data":{"content":"can you explain what's happening in the docs?","transformedContent":"<current_datetime>2026-11-03T23:15:47.843-08:00</current_datetime>\n\ncan you explain what's happening in the docs?","messageId":"m1","interactionId":"i1","turnId":"0"},"id":"e3","timestamp":"2026-11-04T07:15:47.843Z","parentId":"e2"}
{"type":"session.auto_mode_resolved","data":{"chosenModel":"gpt-6-luna","selectionReason":"Copilot Fusion selected GPT-6 Luna","routingMethod":"auto_v2","fallback":false},"id":"e4","timestamp":"2026-11-04T07:15:48.300Z","parentId":"e3"}
{"type":"assistant.message","data":{"messageId":"a1","content":"","toolRequests":[{"toolCallId":"c1","name":"glob","arguments":"{\"pattern\":\"**/*.md\"}","type":"function"}]},"id":"e5","timestamp":"2026-11-04T07:15:51.000Z","parentId":"e4"}
{"type":"session.usage_record","data":{"usage":{"model":"gpt-6-luna","provider":{"id":"copilot"},"modelDisplayName":"GPT-6 Luna","inputTokens":16509,"outputTokens":124,"cacheReadTokens":0,"cacheWriteTokens":16506,"reasoningTokens":63,"cost":1.0,"isAuto":true}},"id":"e6","timestamp":"2026-11-04T07:15:51.100Z","parentId":"e5"}
{"type":"tool.execution_start","data":{"toolCallId":"c1","toolName":"glob","arguments":{"pattern":"**/*.md"},"turnId":"0","model":"gpt-6-luna","toolTitle":"Finding files"},"id":"e7","timestamp":"2026-11-04T07:15:51.200Z","parentId":"e5"}
{"type":"tool.execution_complete","data":{"toolCallId":"c1","model":"gpt-6-luna","success":true,"result":{"content":"53 paths"}},"id":"e8","timestamp":"2026-11-04T07:15:51.500Z","parentId":"e7"}
{"type":"assistant.message","data":{"messageId":"a2","content":"The docs folder holds the product documentation."},"id":"e9","timestamp":"2026-11-04T07:15:55.000Z","parentId":"e8"}
{"type":"session.usage_record","data":{"usage":{"model":"gpt-6-luna","inputTokens":23173,"outputTokens":216,"cacheReadTokens":16506,"cacheWriteTokens":6664,"reasoningTokens":95}},"id":"e10","timestamp":"2026-11-04T07:15:55.100Z","parentId":"e9"}
{"type":"user.message","data":{"content":"now update the uninstall note","messageId":"m2","turnId":"1"},"id":"e11","timestamp":"2026-11-04T07:17:00.000Z","parentId":"e10"}
{"type":"assistant.turn_end","data":{"turnId":"1"},"id":"e12","timestamp":"2026-11-04T07:17:38.000Z","parentId":"e11"}
`

// Trimmed from a real VS Code 1.144 / Copilot Chat 0.72 session: same
// producer, but no model or usage events at all.
const copilotVSCodeStream = `
{"type":"session.start","data":{"sessionId":"5d2cab69","version":1,"producer":"copilot-agent","copilotVersion":"0.72.0","vscodeVersion":"1.144.1","startTime":"2026-11-04T06:47:28.234Z"},"id":"v1","timestamp":"2026-11-04T06:47:28.234Z","parentId":null}
{"type":"user.message","data":{"content":"can you explain what's happening in our docs?","attachments":[]},"id":"v2","timestamp":"2026-11-04T06:47:28.300Z","parentId":"v1"}
{"type":"assistant.message","data":{"messageId":"d8","content":"I'll take a quick look.","reasoningText":"","toolRequests":[{"toolCallId":"call_1","name":"list_dir","arguments":"{\"path\":\"/Users/sam/Desktop/Bilanc/docs\"}","type":"function"}]},"id":"v3","timestamp":"2026-11-04T06:47:31.600Z","parentId":"v2"}
{"type":"tool.execution_start","data":{"toolCallId":"call_1","toolName":"list_dir","arguments":{"path":"/Users/sam/Desktop/Bilanc/docs"}},"id":"v4","timestamp":"2026-11-04T06:47:31.682Z","parentId":"v3"}
{"type":"tool.execution_complete","data":{"toolCallId":"call_1","success":true},"id":"v5","timestamp":"2026-11-04T06:47:31.700Z","parentId":"v4"}
{"type":"assistant.message","data":{"messageId":"d9","content":"The docs are a Mintlify site.","toolRequests":[]},"id":"v6","timestamp":"2026-11-04T06:47:46.331Z","parentId":"v5"}
`

func TestParseFileCopilotCLI(t *testing.T) {
	path := writeTranscript(t, copilotCLIStream)
	sum := ParseFile(path)
	if sum == nil {
		t.Fatal("nil summary")
	}
	if sum.Model != "gpt-6-luna" {
		t.Errorf("model = %q, want gpt-6-luna (auto mode resolution)", sum.Model)
	}
	if sum.AssistantMessageCount != 2 {
		t.Errorf("assistant messages = %d, want 2", sum.AssistantMessageCount)
	}
	if sum.FirstTS != "2026-11-04T07:15:20.338Z" || sum.LastTS != "2026-11-04T07:17:38.000Z" {
		t.Errorf("span = %s .. %s", sum.FirstTS, sum.LastTS)
	}
	if sum.Tokens == nil {
		t.Fatal("expected token usage from session.usage_record events")
	}
	want := TokenUsage{Input: 6, Output: 340, CacheRead: 16506, CacheCreation: 23170}
	if *sum.Tokens != want {
		t.Errorf("tokens = %+v, want %+v", *sum.Tokens, want)
	}
}

func TestParseFileCopilotModelSwitchWins(t *testing.T) {
	path := writeTranscript(t, copilotCLIStream+`
{"type":"session.model_change","data":{"cause":"user_selection","source":"user","newModel":"claude-sonnet-5.5"},"id":"e13","timestamp":"2026-11-04T07:18:00.000Z","parentId":"e12"}
`)
	sum := ParseFile(path)
	if sum == nil || sum.Model != "claude-sonnet-5.5" {
		t.Fatalf("model = %+v, want the later explicit switch", sum)
	}
}

func TestParseFileCopilotVSCode(t *testing.T) {
	path := writeTranscript(t, copilotVSCodeStream)
	sum := ParseFile(path)
	if sum == nil {
		t.Fatal("nil summary")
	}
	if sum.Model != "" {
		t.Errorf("VS Code streams carry no model, got %q", sum.Model)
	}
	if sum.Tokens != nil {
		t.Errorf("VS Code streams carry no usage, got %+v", sum.Tokens)
	}
	if sum.AssistantMessageCount != 2 {
		t.Errorf("assistant messages = %d, want 2", sum.AssistantMessageCount)
	}
	if sum.FirstTS != "2026-11-04T06:47:28.234Z" || sum.LastTS != "2026-11-04T06:47:46.331Z" {
		t.Errorf("span = %s .. %s", sum.FirstTS, sum.LastTS)
	}
}

func TestExtractUserPromptsCopilot(t *testing.T) {
	path := writeTranscript(t, copilotCLIStream)
	prompts := ExtractUserPrompts(path)
	if len(prompts) != 2 {
		t.Fatalf("expected 2 prompts, got %d: %+v", len(prompts), prompts)
	}
	if prompts[0].Text != "can you explain what's happening in the docs?" || prompts[0].TS != "2026-11-04T07:15:47.843Z" {
		t.Errorf("unexpected first prompt: %+v", prompts[0])
	}
	if prompts[1].Text != "now update the uninstall note" {
		t.Errorf("unexpected second prompt: %+v", prompts[1])
	}
}

func TestFindPromptBeforeCopilot(t *testing.T) {
	path := writeTranscript(t, copilotCLIStream)
	if got := FindPromptBefore(path, "2026-11-04T07:17:30.000Z"); got != "now update the uninstall note" {
		t.Errorf("prompt before second turn's end = %q", got)
	}
	if got := FindPromptBefore(path, "2026-11-04T07:16:00.000Z"); got != "can you explain what's happening in the docs?" {
		t.Errorf("prompt before first turn's end = %q", got)
	}
	if got := FindPromptBefore(path, "2026-11-04T07:15:00.000Z"); got != "" {
		t.Errorf("no prompt before session start, got %q", got)
	}
}
