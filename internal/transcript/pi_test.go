package transcript

import "testing"

// Shaped like a Pi (pi.dev) session file, version 3: a header, a tree of
// entries linked by id/parentId, and "message" entries whose message.role is
// system, user, assistant, toolResult, custom or bashExecution. Assistant
// messages carry model and usage on every model call; usage.input is already
// uncached (Pi subtracts cache reads and writes before recording it).
const piSession = `
{"type":"session","version":3,"id":"01a1f479-55c7-72ad-a160-eb05f4108cc4","timestamp":"2026-11-19T15:42:34.952Z","cwd":"/home/sam/src/billing"}
{"type":"model_change","id":"a1","parentId":null,"timestamp":"2026-11-19T15:42:34.960Z","provider":"amazon-bedrock","modelId":"eu.anthropic.claude-opus-4-8"}
{"type":"thinking_level_change","id":"a2","parentId":"a1","timestamp":"2026-11-19T15:42:34.961Z","thinkingLevel":"high"}
{"type":"message","id":"a3","parentId":"a2","timestamp":"2026-11-19T15:42:35.000Z","message":{"role":"system","content":"You are a coding assistant.","timestamp":1795103555000}}
{"type":"message","id":"a4","parentId":"a3","timestamp":"2026-11-19T15:42:40.000Z","message":{"role":"user","content":[{"type":"text","text":"add proration when a seat is removed mid-cycle"}],"timestamp":1795103560000}}
{"type":"message","id":"a5","parentId":"a4","timestamp":"2026-11-19T15:42:50.000Z","message":{"role":"assistant","content":[{"type":"text","text":"On it."},{"type":"toolCall","id":"t1","name":"edit","arguments":{"path":"src/seat.go","edits":[{"oldText":"a","newText":"b"}]}}],"api":"bedrock-converse-stream","provider":"amazon-bedrock","model":"eu.anthropic.claude-opus-4-8","usage":{"input":120,"output":40,"cacheRead":9000,"cacheWrite":300,"totalTokens":9460,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"stopReason":"toolUse","timestamp":1795103570000}}
{"type":"message","id":"a6","parentId":"a5","timestamp":"2026-11-19T15:42:51.000Z","message":{"role":"toolResult","toolCallId":"t1","toolName":"edit","content":[{"type":"text","text":"Successfully replaced 1 block"}],"isError":false,"timestamp":1795103571000}}
{"type":"message","id":"a7","parentId":"a6","timestamp":"2026-11-19T15:43:00.000Z","message":{"role":"assistant","content":[{"type":"text","text":"Done."}],"api":"bedrock-converse-stream","provider":"amazon-bedrock","model":"eu.anthropic.claude-opus-4-8","usage":{"input":15,"output":8,"cacheRead":9400,"cacheWrite":0,"totalTokens":9423,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"stopReason":"stop","timestamp":1795103580000}}
{"type":"custom","customType":"pi-memory","id":"a8","parentId":"a7","timestamp":"2026-11-19T15:43:01.000Z","data":{}}
{"type":"message","id":"a9","parentId":"a8","timestamp":"2026-11-19T15:43:02.000Z","message":{"role":"custom","customType":"reminder","content":"injected by an extension","display":false,"timestamp":1795103582000}}
{"type":"message","id":"b1","parentId":"a9","timestamp":"2026-11-19T15:44:00.000Z","message":{"role":"bashExecution","command":"go test ./...","output":"ok","exitCode":0,"timestamp":1795103640000}}
{"type":"message","id":"b2","parentId":"b1","timestamp":"2026-11-19T15:45:00.000Z","message":{"role":"user","content":"credit should round to cents","timestamp":1795103700000}}
{"type":"message","id":"b3","parentId":"b2","timestamp":"2026-11-19T15:45:20.000Z","message":{"role":"assistant","content":[{"type":"text","text":"Rounded."}],"api":"openai-responses","provider":"openai","model":"gpt-6","usage":{"input":30,"output":12,"cacheRead":0,"cacheWrite":0,"totalTokens":42,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"stopReason":"stop","timestamp":1795103720000}}
`

func TestParseFilePi(t *testing.T) {
	sum := ParseFile(writeTranscript(t, piSession))
	if sum == nil {
		t.Fatal("nil summary")
	}
	if sum.Model != "eu.anthropic.claude-opus-4-8" {
		t.Errorf("model = %q, want the model of most assistant messages", sum.Model)
	}
	if sum.AssistantMessageCount != 3 {
		t.Errorf("assistant messages = %d, want 3", sum.AssistantMessageCount)
	}
	if sum.FirstTS != "2026-11-19T15:42:34.952Z" || sum.LastTS != "2026-11-19T15:45:20.000Z" {
		t.Errorf("span = %s .. %s", sum.FirstTS, sum.LastTS)
	}
	want := TokenUsage{Input: 165, Output: 60, CacheRead: 18400, CacheCreation: 300}
	if sum.Tokens == nil || *sum.Tokens != want {
		t.Errorf("tokens = %+v, want %+v", sum.Tokens, want)
	}
}

// A Pi session whose only assistant message ended in an error still names its
// model, and a missing usage block leaves tokens unknown rather than zero.
func TestParseFilePiWithoutUsage(t *testing.T) {
	sum := ParseFile(writeTranscript(t, `
{"type":"session","version":3,"id":"s1","timestamp":"2026-11-19T10:00:00.000Z","cwd":"/tmp"}
{"type":"message","id":"a1","parentId":null,"timestamp":"2026-11-19T10:00:01.000Z","message":{"role":"user","content":"hi","timestamp":1}}
{"type":"message","id":"a2","parentId":"a1","timestamp":"2026-11-19T10:00:02.000Z","message":{"role":"assistant","content":[],"provider":"openai","model":"gpt-6","stopReason":"error","timestamp":2}}
`))
	if sum == nil || sum.Model != "gpt-6" || sum.AssistantMessageCount != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	if sum.Tokens != nil {
		t.Errorf("tokens = %+v, want nil", sum.Tokens)
	}
}

func TestExtractUserPromptsPi(t *testing.T) {
	prompts := ExtractUserPrompts(writeTranscript(t, piSession))
	if len(prompts) != 2 {
		t.Fatalf("prompts = %+v, want the 2 typed by the engineer", prompts)
	}
	if prompts[0].Text != "add proration when a seat is removed mid-cycle" || prompts[0].TS != "2026-11-19T15:42:40.000Z" {
		t.Errorf("first prompt = %+v", prompts[0])
	}
	if prompts[1].Text != "credit should round to cents" {
		t.Errorf("second prompt = %+v", prompts[1])
	}
}

func TestFindPromptBeforePi(t *testing.T) {
	path := writeTranscript(t, piSession)
	if got := FindPromptBefore(path, "2026-11-19T15:45:10.000Z"); got != "credit should round to cents" {
		t.Errorf("prompt before 15:45:10 = %q", got)
	}
	if got := FindPromptBefore(path, "2026-11-19T15:44:00.000Z"); got != "add proration when a seat is removed mid-cycle" {
		t.Errorf("prompt before 15:44:00 = %q", got)
	}
}
