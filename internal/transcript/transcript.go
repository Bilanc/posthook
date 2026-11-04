// Package transcript parses agent session transcript JSONL files for model,
// timestamp, and token-usage metadata, and for prompt extraction during
// posthook blame. Two shapes are recognized, per-line, in a single pass:
//
// Claude Code (~/.claude/projects/**.jsonl):
//
//	{type: "assistant", message: {id, model, role, content, usage}, timestamp}
//	{type: "user",      message: {role, content}, timestamp}
//
// Codex rollouts (~/.codex/sessions/**.jsonl), which the Codex Stop hook also
// hands us via transcript_path:
//
//	{type: "event_msg", payload: {type: "token_count", info: {total_token_usage}}, timestamp}
//
// GitHub Copilot event streams — Copilot CLI's
// ~/.copilot/session-state/<id>/events.jsonl and VS Code's
// workspaceStorage/<hash>/GitHub.copilot-chat/transcripts/<id>.jsonl share
// one producer ("copilot-agent"):
//
//	{type: "user.message",              data: {content}, timestamp}
//	{type: "assistant.message",         data: {content, modelId?}, timestamp}
//	{type: "session.auto_mode_resolved", data: {chosenModel}, timestamp}
//	{type: "session.model_change",      data: {newModel}, timestamp}
//	{type: "session.usage_record",      data: {usage: {model, inputTokens, …}}, timestamp}
//
// Only the CLI writes the model and usage events; VS Code's stream carries
// neither, so those sessions keep a NULL model and NULL tokens.
//
// Tokens are nullable by design: Cursor exposes no usage anywhere, so a
// session without a Tokens summary means "agent doesn't report usage", not
// zero. (Session token columns were dropped in schema v3 because cross-agent
// zeros were misleading; v8 re-adds them as NULLable for exactly this reason.)
package transcript

import (
	"encoding/json"
	"os"
	"strings"
)

// TokenUsage is a whole-session token total. Input is uncached input tokens;
// cache reads/writes are broken out so the two never double-count. For Codex,
// input_tokens includes cached_input_tokens upstream — we subtract so Input
// means the same thing for both agents. Codex has no cache-write concept, so
// CacheCreation stays 0 there.
type TokenUsage struct {
	Input         int64
	Output        int64
	CacheRead     int64
	CacheCreation int64
}

type Summary struct {
	Model                 string
	AssistantMessageCount int
	FirstTS               string
	LastTS                string
	Tokens                *TokenUsage // nil when the transcript carries no usage data
}

type record struct {
	Type        string           `json:"type"`
	Timestamp   string           `json:"timestamp"`
	Message     *messageEnvelope `json:"message"`
	Payload     *codexPayload    `json:"payload"`
	Data        json.RawMessage  `json:"data"` // Copilot event streams
	IsMeta      bool             `json:"isMeta"`
	IsSidechain bool             `json:"isSidechain"`
}

type messageEnvelope struct {
	ID      string          `json:"id"`
	Model   string          `json:"model"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	Usage   *claudeUsage    `json:"usage"`
}

type claudeUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

type codexPayload struct {
	Type    string     `json:"type"`
	Info    *codexInfo `json:"info"`
	Message string     `json:"message"`
}

type codexInfo struct {
	TotalTokenUsage *codexUsage `json:"total_token_usage"`
}

type codexUsage struct {
	InputTokens       int64 `json:"input_tokens"`
	CachedInputTokens int64 `json:"cached_input_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
}

// copilotData is the union of the Copilot event fields we read; every event
// type carries a different subset.
type copilotData struct {
	Content     json.RawMessage `json:"content"`     // user.message (string, or blocks)
	ModelID     string          `json:"modelId"`     // assistant.message (VS Code never sets it)
	Model       string          `json:"model"`       // tool.execution_start
	ChosenModel string          `json:"chosenModel"` // session.auto_mode_resolved
	NewModel    string          `json:"newModel"`    // session.model_change ("auto" until resolved)
	Usage       *copilotUsage   `json:"usage"`       // session.usage_record, one per model call
}

type copilotUsage struct {
	Model            string `json:"model"`
	InputTokens      int64  `json:"inputTokens"`
	OutputTokens     int64  `json:"outputTokens"`
	CacheReadTokens  int64  `json:"cacheReadTokens"`
	CacheWriteTokens int64  `json:"cacheWriteTokens"`
}

// isCopilotEvent recognises the dotted event types of a Copilot stream
// (Claude's are bare "assistant"/"user", Codex's "event_msg"/"response_item").
func isCopilotEvent(t string) bool {
	for _, prefix := range []string{"session.", "user.", "assistant.", "tool.", "model.", "system."} {
		if strings.HasPrefix(t, prefix) {
			return true
		}
	}
	return false
}

// copilotState accumulates model and usage across one Copilot stream.
type copilotState struct {
	seen              bool
	assistantMessages int
	lastModel         string         // most recent explicit resolution (auto-mode pick or user switch)
	modelCounts       map[string]int // models stamped on usage records / tool calls, as a fallback
	usage             *TokenUsage
}

func (c *copilotState) observe(rec record) {
	c.seen = true
	var d copilotData
	if len(rec.Data) > 0 {
		_ = json.Unmarshal(rec.Data, &d)
	}
	switch rec.Type {
	case "assistant.message":
		c.assistantMessages++
		if d.ModelID != "" {
			c.count(d.ModelID)
		}
	case "session.auto_mode_resolved":
		if d.ChosenModel != "" {
			c.lastModel = d.ChosenModel
		}
	case "session.model_change":
		if d.NewModel != "" && d.NewModel != "auto" {
			c.lastModel = d.NewModel
		}
	case "tool.execution_start":
		if d.Model != "" {
			c.count(d.Model)
		}
	case "session.usage_record":
		u := d.Usage
		if u == nil {
			return
		}
		if c.usage == nil {
			c.usage = &TokenUsage{}
		}
		// inputTokens is the full prompt including cached tokens; subtract the
		// cache traffic so Input means uncached input, as for the other agents.
		uncached := u.InputTokens - u.CacheReadTokens - u.CacheWriteTokens
		if uncached < 0 {
			uncached = 0
		}
		c.usage.Input += uncached
		c.usage.Output += u.OutputTokens
		c.usage.CacheRead += u.CacheReadTokens
		c.usage.CacheCreation += u.CacheWriteTokens
		if u.Model != "" {
			c.count(u.Model)
		}
	}
}

func (c *copilotState) count(model string) {
	if c.modelCounts == nil {
		c.modelCounts = map[string]int{}
	}
	c.modelCounts[model]++
}

func (c *copilotState) model() string {
	if c.lastModel != "" {
		return c.lastModel
	}
	top, topN := "", 0
	for m, n := range c.modelCounts {
		if n > topN {
			top, topN = m, n
		}
	}
	return top
}

// copilotPromptText returns the typed prompt from a user.message event.
func copilotPromptText(rec record) string {
	var d copilotData
	if len(rec.Data) == 0 || json.Unmarshal(rec.Data, &d) != nil {
		return ""
	}
	return cleanPromptText(stringifyContent(d.Content))
}

// ParseFile reads a session transcript (Claude JSONL, Codex rollout or Copilot
// event stream) and returns model + timestamp summary. Returns nil on missing
// file / read failure.
func ParseFile(path string) *Summary {
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return parseString(string(data))
}

func parseString(raw string) *Summary {
	modelCounts := map[string]int{}
	out := &Summary{}

	// Claude repeats one API message across several JSONL lines (one per
	// content block), each carrying the same usage — dedup by message id so a
	// 3-block reply doesn't count 3x. Codex's total_token_usage is cumulative,
	// so the last token_count event IS the session total.
	claudeByMsgID := map[string]claudeUsage{}
	var claudeNoID []claudeUsage
	var codexTotal *codexUsage
	var copilot copilotState

	for _, line := range strings.Split(raw, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec.Timestamp != "" {
			if out.FirstTS == "" || rec.Timestamp < out.FirstTS {
				out.FirstTS = rec.Timestamp
			}
			if rec.Timestamp > out.LastTS {
				out.LastTS = rec.Timestamp
			}
		}
		if rec.Type == "event_msg" && rec.Payload != nil &&
			rec.Payload.Type == "token_count" &&
			rec.Payload.Info != nil && rec.Payload.Info.TotalTokenUsage != nil {
			codexTotal = rec.Payload.Info.TotalTokenUsage
			continue
		}
		if isCopilotEvent(rec.Type) {
			copilot.observe(rec)
			continue
		}
		if rec.Type != "assistant" || rec.Message == nil {
			continue
		}
		out.AssistantMessageCount++
		if rec.Message.Model != "" {
			modelCounts[rec.Message.Model]++
		}
		if u := rec.Message.Usage; u != nil {
			if rec.Message.ID != "" {
				claudeByMsgID[rec.Message.ID] = *u
			} else {
				claudeNoID = append(claudeNoID, *u)
			}
		}
	}

	top := ""
	topN := 0
	for m, n := range modelCounts {
		if n > topN {
			topN = n
			top = m
		}
	}
	out.Model = top
	out.Tokens = sumTokens(claudeByMsgID, claudeNoID, codexTotal)
	if copilot.seen {
		out.AssistantMessageCount += copilot.assistantMessages
		if out.Model == "" {
			out.Model = copilot.model()
		}
		if out.Tokens == nil {
			out.Tokens = copilot.usage
		}
	}
	return out
}

func sumTokens(byMsgID map[string]claudeUsage, noID []claudeUsage, codexTotal *codexUsage) *TokenUsage {
	if codexTotal != nil {
		return &TokenUsage{
			Input:     codexTotal.InputTokens - codexTotal.CachedInputTokens,
			Output:    codexTotal.OutputTokens,
			CacheRead: codexTotal.CachedInputTokens,
		}
	}
	if len(byMsgID) == 0 && len(noID) == 0 {
		return nil
	}
	t := &TokenUsage{}
	add := func(u claudeUsage) {
		t.Input += u.InputTokens
		t.Output += u.OutputTokens
		t.CacheRead += u.CacheReadInputTokens
		t.CacheCreation += u.CacheCreationInputTokens
	}
	for _, u := range byMsgID {
		add(u)
	}
	for _, u := range noID {
		add(u)
	}
	return t
}

// UserPrompt is one prompt the engineer typed during a session.
type UserPrompt struct {
	TS   string
	Text string
}

// ExtractUserPrompts returns the engineer's typed prompts from a transcript,
// oldest first. Claude Code JSONL: type=user lines whose content carries text
// (tool_result-only lines flatten to "" and drop out), skipping meta/sidechain
// lines and slash-command noise. Codex rollouts: event_msg/user_message lines,
// which hold the typed prompt verbatim (the response_item user messages also
// present in rollouts duplicate these plus environment-context noise, so they
// are ignored). Copilot streams: user.message events, whose content is the
// prompt as typed (transformedContent, which adds a datetime header, is
// ignored). Returns nil on missing/unreadable file.
func ExtractUserPrompts(path string) []UserPrompt {
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	var out []UserPrompt
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}

		// Codex rollout shape.
		if rec.Type == "event_msg" && rec.Payload != nil && rec.Payload.Type == "user_message" {
			if text := cleanPromptText(rec.Payload.Message); text != "" {
				out = append(out, UserPrompt{TS: rec.Timestamp, Text: text})
			}
			continue
		}

		// Copilot event-stream shape.
		if rec.Type == "user.message" {
			if text := copilotPromptText(rec); text != "" {
				out = append(out, UserPrompt{TS: rec.Timestamp, Text: text})
			}
			continue
		}

		// Claude Code shape.
		if rec.Type != "user" || rec.Message == nil || rec.Message.Role != "user" {
			continue
		}
		if rec.IsMeta || rec.IsSidechain {
			continue
		}
		if text := cleanPromptText(stringifyContent(rec.Message.Content)); text != "" {
			out = append(out, UserPrompt{TS: rec.Timestamp, Text: text})
		}
	}
	return out
}

// promptNoisePrefixes mark user-role lines that are tooling artifacts, not
// typed prompts: slash-command envelopes and the resumed-session caveat from
// Claude Code, and the environment context Codex injects as a user message.
var promptNoisePrefixes = []string{
	"<command-name>",
	"<command-message>",
	"<local-command-stdout>",
	"<local-command-stderr>",
	"<environment_context>",
	"Caveat: the messages below were generated",
}

func cleanPromptText(s string) string {
	// Codex prefixes prompts replayed into rollouts with a "❯ " marker.
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "❯"))
	if s == "" {
		return ""
	}
	for _, noise := range promptNoisePrefixes {
		if strings.HasPrefix(s, noise) {
			return ""
		}
	}
	return s
}

// FindPromptBefore returns the text of the most recent user-role message in
// the transcript whose timestamp is strictly before beforeTS. Used by
// posthook blame to show the prompt that triggered a given tool call.
func FindPromptBefore(transcriptPath, beforeTS string) string {
	if _, err := os.Stat(transcriptPath); err != nil {
		return ""
	}
	data, err := os.ReadFile(transcriptPath)
	if err != nil {
		return ""
	}

	bestTS := ""
	bestText := ""
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		var text string
		switch {
		case rec.Type == "user.message": // Copilot
			text = copilotPromptText(rec)
		case rec.Type == "user" && rec.Message != nil && rec.Message.Role == "user": // Claude Code
			text = stringifyContent(rec.Message.Content)
		default:
			continue
		}
		if rec.Timestamp == "" || rec.Timestamp >= beforeTS {
			continue
		}
		if text == "" {
			continue
		}
		if rec.Timestamp > bestTS {
			bestTS = rec.Timestamp
			bestText = text
		}
	}
	return bestText
}

// stringifyContent flattens a message content field into a single string.
// Claude messages can be either a plain string or an array of content blocks
// like [{type: "text", text: "..."}, {type: "tool_result", ...}]. For blame
// we want the user's actual prompt — tool_result blocks are auto-generated
// noise and are skipped.
func stringifyContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	// Try as plain string.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}

	// Try as array of content blocks.
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}
