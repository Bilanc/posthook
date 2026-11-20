package ingest

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// An edit whose oldText/newText cut through lines: Pi changed
// "func Remove() {}" into three lines while sending only the text around
// "Remove() {". Diffing the strings would credit only the new middle line;
// the whole-line patch Pi reports with the result (details.patch, sent as
// tool_response.patch) credits all three.
func TestPiEditCreditsWholeLinesFromItsPatch(t *testing.T) {
	_ = store.Close()
	home := isolateIngest(t)
	t.Cleanup(func() { _ = store.Close() })

	const session = "pi-patch"
	repo := filepath.Join(home, "repo-pi-patch")
	initRepo(t, repo)
	seat := filepath.Join(repo, "src", "seat.go")
	writeFile(t, seat, "package seat\n\nfunc Remove() {}\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "init")

	writeFile(t, seat, "package seat\n\nfunc Remove() {\n\tprorate()\n}\n")
	deliver(t, piAgentSlug, repo, piEvent("PostToolUse", session, repo, map[string]any{
		"tool_name":   "edit",
		"tool_use_id": "t1",
		"tool_input": map[string]any{"path": "src/seat.go", "edits": []any{
			map[string]any{"oldText": "Remove() {", "newText": "Remove() {\n\tprorate()\n"},
		}},
		"tool_response": map[string]any{
			"is_error": false,
			"patch":    "--- src/seat.go\n+++ src/seat.go\n@@ -1,3 +1,5 @@\n package seat\n \n-func Remove() {}\n+func Remove() {\n+\tprorate()\n+}\n",
		},
	}))

	db := openDB(t)
	if got := rangesForSession(t, db, session); len(got) != 1 || got[0] != (rangeRow{3, 5}) {
		t.Errorf("line ranges = %+v, want [{3 5}]", got)
	}
	if c := countsForSession(t, db, session); len(c) != 1 || c[0].generated.Int64 != 3 {
		t.Errorf("lines generated = %+v, want 3", c)
	}
	// The patch is used, not stored: tool_response keeps only its flags.
	var stored sql.NullString
	if err := db.QueryRow(`SELECT json_extract(payload, '$.tool_response') FROM events WHERE session_id = ?`, session).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored.String, "prorate") {
		t.Errorf("stored tool_response carries code: %s", stored.String)
	}
	if added, attributed := commitAndAttribute(t, repo, session); added != 3 || attributed != 3 {
		t.Errorf("commit added %d lines, %d attributed to the Pi session; want 3 and 3", added, attributed)
	}
}

// Two Pi edits queued before either is handled: the second puts a line
// above the first's, so by the time the worker reads the file the first
// edit's line has moved. With the file as it was before each edit
// (tool_response.originalFile, sent by the extension), each patch is applied
// to its own before-image, exactly as for Claude Code, and both lines keep
// their author whatever the disk holds by now.
func TestQueuedPiEditsKeepTheirLines(t *testing.T) {
	for _, tc := range []struct {
		name         string
		withOriginal bool
		want         int
	}{
		{"with the file before each edit", true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = store.Close()
			home := isolateIngest(t)
			t.Cleanup(func() { _ = store.Close() })

			session := "pi-queued"
			repo := filepath.Join(home, "repo-pi-queued")
			initRepo(t, repo)
			path := filepath.Join(repo, "list.txt")
			writeFile(t, path, "a\nb\nc\n")
			gitIn(t, repo, "add", "-A")
			gitIn(t, repo, "commit", "-q", "-m", "init")

			// Both edits have run by the time the worker gets to either.
			writeFile(t, path, "X\na\nB\nc\n")
			edit := func(id, before, oldText, newText, patch string, firedAt time.Time) {
				resp := map[string]any{"is_error": false, "patch": patch}
				if tc.withOriginal {
					resp["originalFile"] = before
				}
				deliverAt(t, piAgentSlug, repo, piEvent("PostToolUse", session, repo, map[string]any{
					"tool_name": "edit", "tool_use_id": id,
					"tool_input":    map[string]any{"path": "list.txt", "edits": []any{map[string]any{"oldText": oldText, "newText": newText}}},
					"tool_response": resp,
				}), firedAt)
			}
			fired := time.Now().Add(-3 * time.Second)
			edit("t1", "a\nb\nc\n", "b", "B", "--- list.txt\n+++ list.txt\n@@ -1,3 +1,3 @@\n a\n-b\n+B\n c\n", fired)
			edit("t2", "a\nB\nc\n", "a", "X\na", "--- list.txt\n+++ list.txt\n@@ -1,3 +1,4 @@\n+X\n a\n B\n c\n", fired.Add(time.Second))

			var stored sql.NullString
			if err := openDB(t).QueryRow(`SELECT json_extract(payload, '$.tool_response') FROM events WHERE session_id = ? AND json_extract(payload, '$.tool_use_id') = 't1'`, session).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(stored.String, "originalFile") || strings.Contains(stored.String, "patch") {
				t.Errorf("stored tool_response = %s, want neither the file nor the patch", stored.String)
			}
			if added, attributed := commitAndAttribute(t, repo, session); added != 2 || attributed != tc.want {
				t.Errorf("commit added %d lines, %d attributed to the Pi session; want 2 and %d", added, attributed, tc.want)
			}
		})
	}
}

// Files that start with a UTF-8 BOM: Pi strips it before matching and
// patching, then writes it back, so its patches never show it. Two edits are
// queued each time, so the first edit's lines are recorded against its own
// rebuilt post-image, not the disk, and that post-image has to carry the BOM
// on line 1 for the commit to match, including when line 1 is a new line or
// the old line 1 is gone. The patches are the ones Pi 1.5.2 produces.
func TestPiEditOfAFileWithABOM(t *testing.T) {
	const bom = "\uFEFF"
	type step struct{ before, patch string }
	for _, tc := range []struct {
		name     string
		after    string
		steps    []step
		added    int
		credited int
	}{
		{
			name:  "change line 1, then add a line",
			after: bom + "A\nb\nc\n",
			steps: []step{
				{bom + "a\nb\n", "--- f\n+++ f\n@@ -1,2 +1,2 @@\n-a\n+A\n b\n"},
				{bom + "A\nb\n", "--- f\n+++ f\n@@ -1,2 +1,3 @@\n A\n b\n+c\n"},
			},
			added: 2, credited: 2,
		},
		{
			name:  "insert above line 1, then add a line",
			after: bom + "x\na\nb\nc\n",
			steps: []step{
				{bom + "a\nb\n", "--- f\n+++ f\n@@ -1,2 +1,3 @@\n+x\n a\n b\n"},
				{bom + "x\na\nb\n", "--- f\n+++ f\n@@ -1,3 +1,4 @@\n x\n a\n b\n+c\n"},
			},
			// The BOM moves off "a" onto "x", so git counts "a" as changed
			// too; neither edit wrote "a", so it stays unattributed.
			added: 3, credited: 2,
		},
		{
			name:  "delete line 1, then add a line",
			after: bom + "b\nc\n",
			steps: []step{
				{bom + "a\nb\n", "--- f\n+++ f\n@@ -1,2 +1,1 @@\n-a\n b\n"},
				{bom + "b\n", "--- f\n+++ f\n@@ -1 +1,2 @@\n b\n+c\n"},
			},
			// Removing line 1 moves the BOM onto "b", which git counts as a
			// changed line; neither edit wrote "b", so it stays unattributed.
			added: 2, credited: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = store.Close()
			home := isolateIngest(t)
			t.Cleanup(func() { _ = store.Close() })

			session := "pi-bom"
			repo := filepath.Join(home, "repo-pi-bom")
			initRepo(t, repo)
			path := filepath.Join(repo, "list.txt")
			writeFile(t, path, bom+"a\nb\n")
			gitIn(t, repo, "add", "-A")
			gitIn(t, repo, "commit", "-q", "-m", "init")

			writeFile(t, path, tc.after)
			fired := time.Now().Add(-3 * time.Second)
			for i, s := range tc.steps {
				deliverAt(t, piAgentSlug, repo, piEvent("PostToolUse", session, repo, map[string]any{
					"tool_name": "edit", "tool_use_id": fmt.Sprintf("t%d", i),
					"tool_input":    map[string]any{"path": "list.txt", "edits": []any{map[string]any{"oldText": "x", "newText": "y"}}},
					"tool_response": map[string]any{"is_error": false, "patch": s.patch, "originalFile": s.before},
				}), fired.Add(time.Duration(i)*time.Second))
			}

			if added, attributed := commitAndAttribute(t, repo, session); added != tc.added || attributed != tc.credited {
				t.Errorf("commit added %d lines, %d attributed to the Pi session; want %d and %d", added, attributed, tc.added, tc.credited)
			}
		})
	}
}

// Rework across the BOM: a session's own earlier lines are recorded as the
// file holds them, BOM and all on line 1. A Pi edit revising that line has
// to count it as rework (its patch, made after Pi strips the BOM, shows the
// line without it), and a later whole-file write replacing a line 1 that Pi
// wrote has to as well.
func TestPiReworkOfABOMLine(t *testing.T) {
	_ = store.Close()
	home := isolateIngest(t)
	t.Cleanup(func() { _ = store.Close() })

	const bom = "\uFEFF"
	session := "pi-bom-rework"
	repo := filepath.Join(home, "repo-pi-bom-rework")
	initRepo(t, repo)
	writeFile(t, filepath.Join(repo, "README.md"), "# demo\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "init")
	path := filepath.Join(repo, "list.txt")

	write := func(content string) {
		writeFile(t, path, content)
		deliver(t, piAgentSlug, repo, piEvent("PostToolUse", session, repo, map[string]any{
			"tool_name": "write", "tool_input": map[string]any{"path": "list.txt", "content": content},
		}))
	}
	edit := func(before, after, patch string) {
		writeFile(t, path, after)
		deliver(t, piAgentSlug, repo, piEvent("PostToolUse", session, repo, map[string]any{
			"tool_name":     "edit",
			"tool_input":    map[string]any{"path": "list.txt", "edits": []any{map[string]any{"oldText": "x", "newText": "y"}}},
			"tool_response": map[string]any{"is_error": false, "patch": patch, "originalFile": before},
		}))
	}

	// The session writes the file, then Pi revises the session's own line 1.
	write(bom + "a\nb\n")
	edit(bom+"a\nb\n", bom+"A\nb\n", "--- list.txt\n+++ list.txt\n@@ -1,2 +1,2 @@\n-a\n+A\n b\n")
	// Then a whole-file write replaces the line 1 that Pi wrote.
	write(bom + "Z\nb\n")

	db := openDB(t)
	wantCounts(t, db, session, [2]int64{2, 0}, [2]int64{1, 1}, [2]int64{1, 1})
}
