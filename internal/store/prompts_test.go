package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bilanc/posthook/internal/transcript"
)

func insertTestSession(t *testing.T, db *DB, id, agent string) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO sessions (id, org_id, agent_slug, started_at)
		VALUES (?, 'local', ?, '2026-07-16T10:00:00Z')`, id, agent); err != nil {
		t.Fatalf("insert session: %v", err)
	}
}

func promptRows(t *testing.T, db *DB, sessionID string) []struct {
	Seq    int
	Text   string
	Source string
} {
	t.Helper()
	rows, err := db.Query(`
		SELECT seq, prompt_text, source FROM session_prompts
		WHERE session_id = ? ORDER BY seq`, sessionID)
	if err != nil {
		t.Fatalf("query prompts: %v", err)
	}
	defer rows.Close()
	var out []struct {
		Seq    int
		Text   string
		Source string
	}
	for rows.Next() {
		var r struct {
			Seq    int
			Text   string
			Source string
		}
		if err := rows.Scan(&r.Seq, &r.Text, &r.Source); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, r)
	}
	return out
}

func TestUpsertSessionPromptsIsIdempotentAndAppendsTail(t *testing.T) {
	db := newTestDB(t)
	insertTestSession(t, db, "sess-1", "claude-code")

	first := []transcript.UserPrompt{
		{TS: "2026-07-16T10:00:00Z", Text: "add proration"},
		{TS: "2026-07-16T10:05:00Z", Text: "round to cents"},
	}
	if n, err := db.UpsertSessionPrompts("sess-1", "claude-code", "transcript", first); err != nil || n != 2 {
		t.Fatalf("first upsert: n=%d err=%v", n, err)
	}

	// Re-parse after the transcript grew: same head, new tail.
	grown := append(first, transcript.UserPrompt{TS: "2026-07-16T10:10:00Z", Text: "write tests"})
	if n, err := db.UpsertSessionPrompts("sess-1", "claude-code", "transcript", grown); err != nil || n != 1 {
		t.Fatalf("second upsert should insert only the tail: n=%d err=%v", n, err)
	}

	got := promptRows(t, db, "sess-1")
	if len(got) != 3 || got[2].Text != "write tests" || got[2].Seq != 3 {
		t.Fatalf("unexpected rows: %+v", got)
	}
}

func TestAppendHookPromptAssignsNextSeq(t *testing.T) {
	db := newTestDB(t)
	insertTestSession(t, db, "sess-2", "cursor")

	if err := db.AppendHookPrompt("sess-2", "cursor", "2026-07-16T10:00:00Z", "continue"); err != nil {
		t.Fatalf("append 1: %v", err)
	}
	if err := db.AppendHookPrompt("sess-2", "cursor", "2026-07-16T10:01:00Z", "now fix the types"); err != nil {
		t.Fatalf("append 2: %v", err)
	}
	if err := db.AppendHookPrompt("sess-2", "cursor", "2026-07-16T10:02:00Z", "   "); err != nil {
		t.Fatalf("append blank should no-op: %v", err)
	}

	got := promptRows(t, db, "sess-2")
	if len(got) != 2 || got[0].Seq != 1 || got[1].Seq != 2 || got[1].Source != "hook" {
		t.Fatalf("unexpected rows: %+v", got)
	}
}

func TestBackfillSessionPrompts(t *testing.T) {
	db := newTestDB(t)

	// Session with a transcript on disk, referenced from an event payload.
	transcriptPath := filepath.Join(t.TempDir(), "t.jsonl")
	content := `{"type":"user","timestamp":"2026-07-16T10:00:00Z","message":{"role":"user","content":"backfilled prompt"}}` + "\n"
	if err := os.WriteFile(transcriptPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	insertTestSession(t, db, "sess-t", "claude-code")
	if _, err := db.Exec(`
		INSERT INTO events (id, org_id, session_id, ts, event_type, agent_slug, payload)
		VALUES ('ev-1', 'local', 'sess-t', '2026-07-16T10:00:01Z', 'Stop', 'claude-code',
		        json_object('transcript_path', ?))`, transcriptPath); err != nil {
		t.Fatalf("insert stop event: %v", err)
	}

	// Cursor session whose prompts only exist as beforeSubmitPrompt events.
	insertTestSession(t, db, "sess-c", "cursor")
	if _, err := db.Exec(`
		INSERT INTO events (id, org_id, session_id, ts, event_type, agent_slug, payload)
		VALUES ('ev-2', 'local', 'sess-c', '2026-07-16T11:00:00Z', 'beforeSubmitPrompt', 'cursor',
		        json_object('prompt', 'cursor hook prompt'))`); err != nil {
		t.Fatalf("insert cursor event: %v", err)
	}

	touched, err := db.backfillSessionPrompts()
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if touched != 2 {
		t.Fatalf("expected 2 backfilled prompts, got %d", touched)
	}

	if got := promptRows(t, db, "sess-t"); len(got) != 1 || got[0].Text != "backfilled prompt" || got[0].Source != "transcript" {
		t.Fatalf("transcript backfill rows: %+v", got)
	}
	if got := promptRows(t, db, "sess-c"); len(got) != 1 || got[0].Text != "cursor hook prompt" || got[0].Source != "hook" {
		t.Fatalf("cursor backfill rows: %+v", got)
	}

	// Second run is a no-op.
	if touched, err := db.backfillSessionPrompts(); err != nil || touched != 0 {
		t.Fatalf("re-run should no-op: touched=%d err=%v", touched, err)
	}
}
