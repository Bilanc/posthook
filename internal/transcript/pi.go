package transcript

import "encoding/json"

// Pi (pi.dev) session files (~/.pi/agent/sessions/--<cwd>--/<ts>_<id>.jsonl,
// or wherever PI_CODING_AGENT_SESSION_DIR / --session-dir points) are one
// JSON entry per line. Conversation entries all have type "message" and say
// who spoke in message.role:
//
//	{type: "message", timestamp, message: {role: "user", content}}
//	{type: "message", timestamp, message: {role: "assistant", model, provider, usage: {input, output, cacheRead, cacheWrite}}}
//
// Other roles (system, toolResult, custom, bashExecution, …) and other entry
// types (session, model_change, compaction, …) carry nothing read here.
// Pi's usage.input is already the uncached part of the prompt, so it maps
// straight onto TokenUsage.Input. Entries form a tree (a session can hold
// several branches); every model call in the file is counted once.

const piMessageType = "message"

type piUsage struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cacheRead"`
	CacheWrite int64 `json:"cacheWrite"`
}

type piLine struct {
	Message *struct {
		Usage *piUsage `json:"usage"`
	} `json:"message"`
}

// piState accumulates model and usage across one Pi session file.
type piState struct {
	seen              bool
	assistantMessages int
	modelCounts       map[string]int
	usage             *TokenUsage
}

// isPiMessage reports whether rec is a Pi conversation entry.
func isPiMessage(rec record) bool {
	return rec.Type == piMessageType && rec.Message != nil
}

// observe takes one Pi conversation entry; line is the raw JSON line, read
// again for the usage block, whose field names differ from Claude's.
func (p *piState) observe(rec record, line string) {
	p.seen = true
	if rec.Message.Role != "assistant" {
		return
	}
	p.assistantMessages++
	if rec.Message.Model != "" {
		if p.modelCounts == nil {
			p.modelCounts = map[string]int{}
		}
		p.modelCounts[rec.Message.Model]++
	}
	var l piLine
	if json.Unmarshal([]byte(line), &l) != nil || l.Message == nil || l.Message.Usage == nil {
		return
	}
	u := l.Message.Usage
	if p.usage == nil {
		p.usage = &TokenUsage{}
	}
	p.usage.Input += u.Input
	p.usage.Output += u.Output
	p.usage.CacheRead += u.CacheRead
	p.usage.CacheCreation += u.CacheWrite
}

func (p *piState) model() string {
	top, topN := "", 0
	for m, n := range p.modelCounts {
		if n > topN || (n == topN && m < top) {
			top, topN = m, n
		}
	}
	return top
}

// piPromptText returns the typed prompt from a Pi user message, or "" for
// any other entry.
func piPromptText(rec record) string {
	if !isPiMessage(rec) || rec.Message.Role != "user" {
		return ""
	}
	return cleanPromptText(stringifyContent(rec.Message.Content))
}
