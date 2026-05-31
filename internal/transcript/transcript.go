// Package transcript parses Claude Code session transcript JSONL files for
// model and timestamp metadata, and for prompt extraction during posthook
// blame.
//
// Each line is a record like:
//
//	{type: "assistant", message: {model, role, content}, timestamp}
//	{type: "user",      message: {role, content}, timestamp}
//
// Token counts are intentionally not extracted — they're only available for
// Claude Code and presenting them per-session in metrics is misleading when
// Cursor/Codex are zero. Anything that wants tokens can re-parse the JSONL.
package transcript

import (
	"encoding/json"
	"os"
	"strings"
)

type Summary struct {
	Model                  string
	AssistantMessageCount  int
	FirstTS                string
	LastTS                 string
}

type record struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Message   *messageEnvelope `json:"message"`
}

type messageEnvelope struct {
	Model   string          `json:"model"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// ParseFile reads a Claude transcript JSONL and returns model + timestamp
// summary. Returns nil on missing file / read failure.
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
		if rec.Type != "assistant" || rec.Message == nil {
			continue
		}
		out.AssistantMessageCount++
		if rec.Message.Model != "" {
			modelCounts[rec.Message.Model]++
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
	return out
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
		if rec.Type != "user" || rec.Message == nil || rec.Message.Role != "user" {
			continue
		}
		if rec.Timestamp == "" || rec.Timestamp >= beforeTS {
			continue
		}
		text := stringifyContent(rec.Message.Content)
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
