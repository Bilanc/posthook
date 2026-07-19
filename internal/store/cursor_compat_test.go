package store

import "testing"

// seedSession inserts a bare session row.
func seedSession(t *testing.T, db *DB, id, agent string) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO sessions (id, org_id, agent_slug, started_at)
		VALUES (?, 'local', ?, '2026-07-14T10:00:00Z')`, id, agent); err != nil {
		t.Fatalf("insert session %s: %v", id, err)
	}
}

// seedEvent inserts an event with the given payload JSON.
func seedEvent(t *testing.T, db *DB, id, sessionID, agent, payload string) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO events (id, org_id, session_id, ts, event_type, agent_slug, payload)
		VALUES (?, 'local', ?, '2026-07-14T10:00:01Z', 'PostToolUse', ?, ?)`,
		id, sessionID, agent, payload); err != nil {
		t.Fatalf("insert event %s: %v", id, err)
	}
}

// TestDeleteCursorCompatDuplicates: events Cursor delivered through the
// claude-code hook registration (payload stamped with cursor_version, agent
// flag claude-code) are purged along with the phantom sessions they created,
// while native cursor deliveries and genuine claude-code sessions survive.
func TestDeleteCursorCompatDuplicates(t *testing.T) {
	db := newTestDB(t)

	cursorPayload := `{"tool_name":"Edit","model":"gpt-5.6-sol","cursor_version":"2026.07.09-a3815c0"}`
	claudePayload := `{"tool_name":"Edit","hook_event_name":"PostToolUse"}`

	// Twin pair: native cursor session + phantom claude-code copy that lost
	// the id race and got prefixed by resolveSessionID.
	seedSession(t, db, "sess-1", "cursor")
	seedEvent(t, db, "ev-1", "sess-1", "cursor", cursorPayload)
	seedSession(t, db, "claude-code:sess-1", "claude-code")
	seedEvent(t, db, "ev-2", "claude-code:sess-1", "claude-code", cursorPayload)
	if _, err := db.Exec(`
		INSERT INTO event_line_ranges (id, event_id, file_path, start_line, end_line, new_text_lines)
		VALUES ('elr-1', 'ev-2', '/tmp/f.go', 1, 10, 10)`); err != nil {
		t.Fatalf("insert line range: %v", err)
	}

	// Reverse twin: phantom claude-code copy claimed the bare id first.
	seedSession(t, db, "sess-2", "claude-code")
	seedEvent(t, db, "ev-3", "sess-2", "claude-code", cursorPayload)
	seedSession(t, db, "cursor:sess-2", "cursor")
	seedEvent(t, db, "ev-4", "cursor:sess-2", "cursor", cursorPayload)

	// Genuine claude-code session: no cursor_version in its payloads.
	seedSession(t, db, "sess-3", "claude-code")
	seedEvent(t, db, "ev-5", "sess-3", "claude-code", claudePayload)

	if _, err := db.deleteCursorCompatDuplicates(); err != nil {
		t.Fatalf("deleteCursorCompatDuplicates: %v", err)
	}

	assertGone := func(table, col, id string) {
		t.Helper()
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE `+col+` = ?`, id).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("%s %s=%s: expected deleted, still present", table, col, id)
		}
	}
	assertKept := func(table, col, id string) {
		t.Helper()
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE `+col+` = ?`, id).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n == 0 {
			t.Errorf("%s %s=%s: expected kept, was deleted", table, col, id)
		}
	}

	// Phantom copies gone, whichever side of the id race they landed on.
	assertGone("sessions", "id", "claude-code:sess-1")
	assertGone("events", "id", "ev-2")
	assertGone("event_line_ranges", "id", "elr-1")
	assertGone("sessions", "id", "sess-2")
	assertGone("events", "id", "ev-3")

	// Native cursor deliveries and the genuine claude-code session survive.
	assertKept("sessions", "id", "sess-1")
	assertKept("events", "id", "ev-1")
	assertKept("sessions", "id", "cursor:sess-2")
	assertKept("events", "id", "ev-4")
	assertKept("sessions", "id", "sess-3")
	assertKept("events", "id", "ev-5")
}
