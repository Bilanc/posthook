package ingest

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/bilanc/posthook/internal/store"
)

// A Pi session file as Pi writes it under ~/.pi/agent/sessions, trimmed to
// the entries ingest reads. Two model calls; the second prompt is typed text.
const piSessionFixture = `{"type":"session","version":3,"id":"pi-s1","timestamp":"2026-11-19T15:42:34.952Z","cwd":"/repo"}
{"type":"model_change","id":"e1","parentId":null,"timestamp":"2026-11-19T15:42:34.960Z","provider":"amazon-bedrock","modelId":"eu.anthropic.claude-opus-4-8"}
{"type":"message","id":"e2","parentId":"e1","timestamp":"2026-11-19T15:42:40.000Z","message":{"role":"user","content":[{"type":"text","text":"prorate removed seats"}],"timestamp":1}}
{"type":"message","id":"e3","parentId":"e2","timestamp":"2026-11-19T15:42:50.000Z","message":{"role":"assistant","content":[],"provider":"amazon-bedrock","model":"eu.anthropic.claude-opus-4-8","usage":{"input":120,"output":40,"cacheRead":9000,"cacheWrite":300,"totalTokens":9460},"stopReason":"toolUse","timestamp":2}}
{"type":"message","id":"e4","parentId":"e3","timestamp":"2026-11-19T15:43:00.000Z","message":{"role":"assistant","content":[],"provider":"amazon-bedrock","model":"eu.anthropic.claude-opus-4-8","usage":{"input":15,"output":8,"cacheRead":9400,"cacheWrite":0,"totalTokens":9423},"stopReason":"stop","timestamp":3}}
`

// piEvent builds the payload posthook's Pi extension sends: Pi's own tool
// name and arguments, plus the session's id, cwd, transcript and model.
func piEvent(event, session, cwd string, extra map[string]any) map[string]any {
	p := map[string]any{
		"hook_event_name": event,
		"session_id":      session,
		"cwd":             cwd,
		"model":           "eu.anthropic.claude-opus-4-8",
	}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

func TestPiEndToEnd(t *testing.T) {
	_ = store.Close()
	home := isolateIngest(t)
	t.Cleanup(func() { _ = store.Close() })

	const session = "pi-s1"
	repo := filepath.Join(home, "repo-pi")
	initRepo(t, repo)
	seat := filepath.Join(repo, "src", "seat.go")
	writeFile(t, seat, "package seat\n\nfunc Remove() {}\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "init")

	// A prompt marks the start of a turn; Pi's extension sends no prompt text.
	deliver(t, piAgentSlug, repo, piEvent("UserPromptSubmit", session, repo, nil))

	// The edit tool has written the file by the time its result is reported.
	writeFile(t, seat, "package seat\n\nfunc Remove() {\n\tprorate()\n}\n")
	deliver(t, piAgentSlug, repo, piEvent("PostToolUse", session, repo, map[string]any{
		"tool_name":   "edit",
		"tool_use_id": "t1",
		"tool_input": map[string]any{"path": "src/seat.go", "edits": []any{
			map[string]any{"oldText": "func Remove() {}\n", "newText": "func Remove() {\n\tprorate()\n}\n"},
		}},
	}))

	credit := filepath.Join(repo, "src", "credit.go")
	writeFile(t, credit, "package seat\n\nfunc prorate() {}\n")
	deliver(t, piAgentSlug, repo, piEvent("PostToolUse", session, repo, map[string]any{
		"tool_name":   "write",
		"tool_use_id": "t2",
		"tool_input":  map[string]any{"path": credit, "content": "package seat\n\nfunc prorate() {}\n"},
	}))

	deliver(t, piAgentSlug, repo, piEvent("PostToolUse", session, repo, map[string]any{
		"tool_name": "read", "tool_use_id": "t3", "tool_input": map[string]any{"path": "src/seat.go"},
	}))

	transcriptPath := filepath.Join(home, ".pi", "agent", "sessions", "--repo--", "2026-11-19T15-42-34-952Z_pi-s1.jsonl")
	writeFile(t, transcriptPath, piSessionFixture)
	deliver(t, piAgentSlug, repo, piEvent("Stop", session, repo, map[string]any{"transcript_path": transcriptPath}))

	db := openDB(t)

	// Stored payloads carry the canonical tool names metrics select on, with
	// Pi's own name alongside.
	rows, err := db.Query(`SELECT json_extract(payload, '$.tool_name'), json_extract(payload, '$.pi_tool_name'), rel_file_path FROM events WHERE session_id = ? AND event_type = 'PostToolUse' ORDER BY rowid`, session)
	if err != nil {
		t.Fatal(err)
	}
	type stored struct{ tool, piTool, rel string }
	var got []stored
	for rows.Next() {
		var tool, piTool, rel sql.NullString
		if err := rows.Scan(&tool, &piTool, &rel); err != nil {
			t.Fatal(err)
		}
		got = append(got, stored{tool.String, piTool.String, rel.String})
	}
	rows.Close()
	want := []stored{{"Edit", "edit", "src/seat.go"}, {"Write", "write", "src/credit.go"}, {"Read", "read", "src/seat.go"}}
	if len(got) != len(want) {
		t.Fatalf("events = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	if r := rangesForSession(t, db, session); len(r) != 2 {
		t.Errorf("line ranges = %+v, want one for the edit and one for the write", r)
	}

	var agent, model sql.NullString
	var in, out, cacheRead, cacheWrite sql.NullInt64
	if err := db.QueryRow(`SELECT agent_slug, model_slug, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens FROM sessions WHERE id = ?`, session).
		Scan(&agent, &model, &in, &out, &cacheRead, &cacheWrite); err != nil {
		t.Fatalf("session row: %v", err)
	}
	if agent.String != piAgentSlug || model.String != "eu.anthropic.claude-opus-4-8" {
		t.Errorf("session agent=%q model=%q", agent.String, model.String)
	}
	if in.Int64 != 135 || out.Int64 != 48 || cacheRead.Int64 != 18400 || cacheWrite.Int64 != 300 {
		t.Errorf("tokens = in %d out %d read %d write %d", in.Int64, out.Int64, cacheRead.Int64, cacheWrite.Int64)
	}

	// The prompt comes from the session file, once.
	var prompts int
	var promptText sql.NullString
	if err := db.QueryRow(`SELECT COUNT(*), MAX(prompt_text) FROM session_prompts WHERE session_id = ?`, session).Scan(&prompts, &promptText); err != nil {
		t.Fatal(err)
	}
	if prompts != 1 || promptText.String != "prorate removed seats" {
		t.Errorf("prompts = %d (%q), want the 1 from the session file", prompts, promptText.String)
	}

	// Every line the commit adds was written by the Pi session.
	added, attributed := commitAndAttribute(t, repo, session)
	if added != 6 || attributed != 6 {
		t.Errorf("commit added %d lines, %d attributed to the Pi session; want 6 and 6", added, attributed)
	}
}
