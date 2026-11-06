package store

import (
	"strconv"
	"strings"
	"testing"
)

func TestTrimStoredPayloadsRewritesOnlyWhatNeedsIt(t *testing.T) {
	db := newTestDB(t)
	big := `{"tool_name":"Read","tool_response":{"type":"text","file":{"content":"` + strings.Repeat("x", 20000) + `"}},"tool_input":{"file_path":"a"}}`
	small := `{"hook_event_name":"Stop","last_assistant_message":"done"}`
	mustExec(t, db, `INSERT INTO events (id, ts, event_type, agent_slug, payload, synced_at) VALUES
		('big',   '2026-01-01T00:00:00Z', 'PostToolUse', 'claude-code', ?, '2026-01-02T00:00:00Z'),
		('small', '2026-01-01T00:00:00Z', 'Stop',        'claude-code', ?, NULL),
		('nullp', '2026-01-01T00:00:00Z', 'SessionStart','claude-code', NULL, NULL)`, big, small)

	rows, dropped, err := db.trimStoredPayloads()
	if err != nil {
		t.Fatal(err)
	}
	if rows != 1 || dropped <= 0 {
		t.Fatalf("rows=%d dropped=%d, want 1 row and >0 bytes", rows, dropped)
	}

	var payload, synced string
	mustQueryRow(t, db, `SELECT payload, synced_at FROM events WHERE id = 'big'`, nil, &payload, &synced)
	if strings.Contains(payload, "xxxxx") || !strings.Contains(payload, TrimMarkerKey) {
		t.Errorf("big payload not trimmed: %.120s", payload)
	}
	if synced != "2026-01-02T00:00:00Z" {
		t.Errorf("synced_at must not change, got %q", synced)
	}
	mustQueryRow(t, db, `SELECT payload FROM events WHERE id = 'small'`, nil, &payload)
	if payload != small {
		t.Errorf("small payload rewritten: %s", payload)
	}

	// Second pass is a no-op.
	if rows, _, err := db.trimStoredPayloads(); err != nil || rows != 0 {
		t.Errorf("second pass rows=%d err=%v", rows, err)
	}
}

func TestTrimStoredPayloadsWalksPastOneBatch(t *testing.T) {
	db := newTestDB(t)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1203; i++ {
		if _, err := tx.Exec(`INSERT INTO events (id, ts, event_type, agent_slug, payload) VALUES (?, '2026-01-01T00:00:00Z', 'PostToolUse', 'codex', ?)`,
			"e"+strconv.Itoa(i), `{"tool_name":"Bash","tool_response":"`+strings.Repeat("o", 300)+`"}`); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	rows, _, err := db.trimStoredPayloads()
	if err != nil || rows != 1203 {
		t.Fatalf("rows=%d err=%v", rows, err)
	}
	var left int
	mustQueryRow(t, db, `SELECT count(*) FROM events WHERE payload LIKE '%ooooo%'`, nil, &left)
	if left != 0 {
		t.Errorf("%d rows still carry output", left)
	}
}
