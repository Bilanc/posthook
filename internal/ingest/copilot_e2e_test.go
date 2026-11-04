package ingest

import (
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bilanc/posthook/internal/spool"
	"github.com/bilanc/posthook/internal/store"
)

// A Copilot CLI session stream as written to
// ~/.copilot/session-state/<id>/events.jsonl, trimmed from a real 1.0.105 run.
const copilotCLIStreamFixture = `{"type":"session.start","data":{"sessionId":"60a0ed8e","producer":"copilot-agent","copilotVersion":"1.0.105","startTime":"2026-11-04T07:15:20.338Z"},"id":"e1","timestamp":"2026-11-04T07:15:20.338Z","parentId":null}
{"type":"session.model_change","data":{"cause":"initial_resolution","newModel":"auto"},"id":"e2","timestamp":"2026-11-04T07:15:20.400Z","parentId":"e1"}
{"type":"user.message","data":{"content":"update the uninstall note"},"id":"e3","timestamp":"2026-11-04T07:15:47.843Z","parentId":"e2"}
{"type":"session.auto_mode_resolved","data":{"chosenModel":"gpt-6-luna"},"id":"e4","timestamp":"2026-11-04T07:15:48.300Z","parentId":"e3"}
{"type":"assistant.message","data":{"messageId":"a1","content":""},"id":"e5","timestamp":"2026-11-04T07:15:51.000Z","parentId":"e4"}
{"type":"session.usage_record","data":{"usage":{"model":"gpt-6-luna","inputTokens":16509,"outputTokens":124,"cacheReadTokens":0,"cacheWriteTokens":16506}},"id":"e6","timestamp":"2026-11-04T07:15:51.100Z","parentId":"e5"}
{"type":"assistant.turn_end","data":{"turnId":"0"},"id":"e7","timestamp":"2026-11-04T07:16:38.000Z","parentId":"e6"}
`

func isolateIngest(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("COPILOT_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".gitconfig"))
	t.Setenv("POSTHOOK_BYPASS", "1")
	if err := os.MkdirAll(filepath.Join(home, ".posthook"), 0o755); err != nil {
		t.Fatal(err)
	}
	return home
}

func initRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "dev@example.com"}, {"config", "user.name", "Dev"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func deliver(t *testing.T, agent, cwd string, payload map[string]any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := ProcessAgentEnvelope(spool.Envelope{
		Agent: agent, Cwd: cwd, ReceivedAt: time.Now().UTC().Format(time.RFC3339Nano), Payload: raw,
	}); err != nil {
		t.Fatalf("ProcessAgentEnvelope: %v", err)
	}
}

// openDB returns the process-cached store handle. store.Open caches one
// handle per process, so the end-to-end scenarios below share a single
// isolated HOME and never close it; they stay independent through distinct
// session ids and repos.
func openDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	return db
}

type rangeRow struct{ start, end int }

func rangesForSession(t *testing.T, db *store.DB, sessionID string) []rangeRow {
	t.Helper()
	rows, err := db.Query(`SELECT r.start_line, r.end_line FROM event_line_ranges r JOIN events e ON e.id = r.event_id WHERE e.session_id = ? ORDER BY r.start_line`, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []rangeRow
	for rows.Next() {
		var r rangeRow
		if err := rows.Scan(&r.start, &r.end); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

const noteBefore = "# Deploy\n\n<Note>\n  Uninstalling a machine does not delete data.\n</Note>\n"
const noteAfter = "# Deploy\n\n<Note>\n  Uninstalling removes Posthook from that machine.\n  Data already synced remains.\n</Note>\n"
const notePatchBody = "@@\n <Note>\n-  Uninstalling a machine does not delete data.\n+  Uninstalling removes Posthook from that machine.\n+  Data already synced remains.\n </Note>\n*** End Patch\n"

func TestCopilotEndToEnd(t *testing.T) {
	home := isolateIngest(t)
	t.Run("cli apply_patch then Stop", func(t *testing.T) { testCopilotCLISession(t, home) })
	t.Run("vscode apply_patch", func(t *testing.T) { testCopilotVSCodeApplyPatch(t, home) })
	t.Run("cli create without located ranges", func(t *testing.T) { testCopilotCreateWithoutRanges(t, home) })
	t.Run("claude-code delivery of a Copilot event is dropped", func(t *testing.T) { testCopilotCompatDuplicateDropped(t, home) })
}

func testCopilotCLISession(t *testing.T, home string) {
	repo := filepath.Join(home, "repo-cli")
	initRepo(t, repo)
	rel := "docs/posthook/mdm-deployment.mdx"
	target := filepath.Join(repo, rel)
	writeFile(t, target, noteBefore)

	// PostToolUse fires after Copilot CLI applied the patch. PascalCase
	// registration: tool_name is Claude-mapped ("Edit"), tool_input is the raw
	// patch text with a repo-relative path, cwd is the session's cwd.
	writeFile(t, target, noteAfter)
	deliver(t, copilotAgentSlug, repo, map[string]any{
		"hook_event_name": "PostToolUse",
		"session_id":      "60a0ed8e",
		"timestamp":       "2026-11-04T07:16:30.000Z",
		"cwd":             repo,
		"tool_name":       "Edit",
		"tool_input":      "*** Begin Patch\n*** Update File: " + rel + "\n" + notePatchBody,
		"tool_result":     map[string]any{"result_type": "success", "text_result_for_llm": "Modified 1 file(s): " + target},
	})

	db := openDB(t)
	var eventType, filePath, relFilePath string
	var sessionID sql.NullString
	var linesAdded, linesRemoved sql.NullInt64
	if err := db.QueryRow(`SELECT event_type, session_id, file_path, rel_file_path, lines_added, lines_removed FROM events WHERE agent_slug = ? AND session_id = ?`, copilotAgentSlug, "60a0ed8e").
		Scan(&eventType, &sessionID, &filePath, &relFilePath, &linesAdded, &linesRemoved); err != nil {
		t.Fatalf("event row: %v", err)
	}
	if eventType != "PostToolUse" || sessionID.String != "60a0ed8e" || relFilePath != rel {
		t.Errorf("event = %s session=%s file=%s rel=%s", eventType, sessionID.String, filePath, relFilePath)
	}
	if linesAdded.Int64 != 2 || linesRemoved.Int64 != 1 {
		t.Errorf("patch totals = +%d/-%d, want +2/-1", linesAdded.Int64, linesRemoved.Int64)
	}
	// The stored payload is the normalized one: metrics and the dashboard
	// select edits by json_extract(payload, '$.tool_name') and count lines
	// from lines_added for apply_patch, so the CLI's "Edit" + patch string
	// must be persisted as apply_patch.
	var storedTool, originalTool, patch sql.NullString
	if err := db.QueryRow(`SELECT json_extract(payload, '$.tool_name'), json_extract(payload, '$.copilot_tool_name'), json_extract(payload, '$.tool_input.command') FROM events WHERE session_id = ? AND event_type = 'PostToolUse'`, "60a0ed8e").
		Scan(&storedTool, &originalTool, &patch); err != nil {
		t.Fatalf("stored payload: %v", err)
	}
	if storedTool.String != "apply_patch" || originalTool.String != "Edit" || !patch.Valid {
		t.Errorf("stored payload tool=%q original=%q patch present=%v", storedTool.String, originalTool.String, patch.Valid)
	}
	if got := rangesForSession(t, db, "60a0ed8e"); len(got) != 1 || got[0] != (rangeRow{4, 5}) {
		t.Errorf("line ranges = %+v, want [{4 5}]", got)
	}

	// Stop arrives with no transcript_path: the CLI session stream is found
	// by session id and backfills model, span, tokens and prompts.
	writeFile(t, filepath.Join(home, ".copilot", "session-state", "60a0ed8e", "events.jsonl"), copilotCLIStreamFixture)
	deliver(t, copilotAgentSlug, repo, map[string]any{
		"hook_event_name": "Stop", "session_id": "60a0ed8e", "cwd": repo,
		"stop_reason": "end_turn", "stop_hook_active": false,
	})
	var model, startedAt, endedAt sql.NullString
	var in, out, cacheRead, cacheWrite sql.NullInt64
	if err := db.QueryRow(`SELECT model_slug, started_at, ended_at, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens FROM sessions WHERE id = ?`, "60a0ed8e").
		Scan(&model, &startedAt, &endedAt, &in, &out, &cacheRead, &cacheWrite); err != nil {
		t.Fatalf("session row: %v", err)
	}
	if model.String != "gpt-6-luna" {
		t.Errorf("model = %q", model.String)
	}
	if startedAt.String != "2026-11-04T07:15:20.338Z" || endedAt.String != "2026-11-04T07:16:38.000Z" {
		t.Errorf("span = %s .. %s", startedAt.String, endedAt.String)
	}
	if in.Int64 != 3 || out.Int64 != 124 || cacheRead.Int64 != 0 || cacheWrite.Int64 != 16506 {
		t.Errorf("tokens = in %d out %d read %d write %d", in.Int64, out.Int64, cacheRead.Int64, cacheWrite.Int64)
	}
	var prompts int
	if err := db.QueryRow(`SELECT COUNT(*) FROM session_prompts WHERE session_id = ? AND source = 'transcript'`, "60a0ed8e").Scan(&prompts); err != nil {
		t.Fatal(err)
	}
	if prompts != 1 {
		t.Errorf("prompts = %d, want 1", prompts)
	}
}

// testCopilotCreateWithoutRanges covers the metrics fallback: a Copilot CLI
// create whose file cannot be read back yields no line ranges, so the stored
// payload must carry Write + content for the line count to come from the
// payload (the metrics CASE: located ranges, else apply_patch totals, else
// new_string/content newlines).
func testCopilotCreateWithoutRanges(t *testing.T, home string) {
	repo := filepath.Join(home, "repo-create")
	initRepo(t, repo)
	deliver(t, copilotAgentSlug, repo, map[string]any{
		"hook_event_name": "PostToolUse", "session_id": "create-1", "cwd": repo,
		"tool_name":   "Write",
		"tool_input":  map[string]any{"path": "src/new.go", "file_text": "package x\n\nfunc A() {}\n"},
		"tool_result": map[string]any{"result_type": "success", "text_result_for_llm": "Created src/new.go"},
	})
	db := openDB(t)
	var storedTool, content, filePath sql.NullString
	if err := db.QueryRow(`SELECT json_extract(payload, '$.tool_name'), json_extract(payload, '$.tool_input.content'), file_path FROM events WHERE session_id = ?`, "create-1").
		Scan(&storedTool, &content, &filePath); err != nil {
		t.Fatalf("stored payload: %v", err)
	}
	if storedTool.String != "Write" || content.String != "package x\n\nfunc A() {}\n" || !strings.HasSuffix(filePath.String, "src/new.go") {
		t.Errorf("stored tool=%q content=%q file=%q", storedTool.String, content.String, filePath.String)
	}
	if got := rangesForSession(t, db, "create-1"); len(got) != 0 {
		t.Errorf("no file on disk, so no ranges expected, got %+v", got)
	}
}

func testCopilotVSCodeApplyPatch(t *testing.T, home string) {
	repo := filepath.Join(home, "repo-vscode")
	initRepo(t, repo)
	rel := "docs/posthook/mdm-deployment.mdx"
	target := filepath.Join(repo, rel)
	writeFile(t, target, noteAfter)

	// VS Code's Copilot target: apply_patch with {input: <patch>} and an
	// absolute path inside the patch.
	deliver(t, copilotAgentSlug, repo, map[string]any{
		"hook_event_name": "PostToolUse", "session_id": "5d2cab69", "cwd": repo,
		"tool_name": "apply_patch",
		"tool_input": map[string]any{
			"input":       "*** Begin Patch\n*** Update File: " + target + "\n" + notePatchBody,
			"explanation": "Document the uninstall flow",
			"toolUses":    []any{},
		},
		"tool_result": map[string]any{"result_type": "success", "text_result_for_llm": "ok"},
	})
	db := openDB(t)
	if got := rangesForSession(t, db, "5d2cab69"); len(got) != 1 || got[0] != (rangeRow{4, 5}) {
		t.Errorf("line ranges = %+v, want [{4 5}]", got)
	}
}

func testCopilotCompatDuplicateDropped(t *testing.T, home string) {
	repo := filepath.Join(home, "repo-compat")
	initRepo(t, repo)

	// The same Copilot event delivered through a Claude-format registration
	// (repo .claude/settings.json) must not create a phantom claude-code session.
	deliver(t, "claude-code", repo, map[string]any{
		"hook_event_name": "Stop", "session_id": "dup-60a0ed8e", "cwd": repo,
		"transcript_path": filepath.Join(home, ".copilot", "session-state", "60a0ed8e", "events.jsonl"),
		"stop_reason":     "end_turn",
	})
	deliver(t, "claude-code", repo, map[string]any{
		"hook_event_name": "PostToolUse", "session_id": "dup-60a0ed8e", "cwd": repo,
		"tool_name": "Edit", "tool_input": "*** Begin Patch\n*** Update File: a.md\n@@\n+x\n*** End Patch\n",
		"tool_result": map[string]any{"result_type": "success", "text_result_for_llm": "ok"},
	})
	db := openDB(t)
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE agent_slug = 'claude-code'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("expected both Copilot-shaped claude-code deliveries to be dropped, got %d events", n)
	}
}
