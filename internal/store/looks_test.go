package store

import (
	"fmt"
	"reflect"
	"testing"
)

func TestSetEventLineRanges(t *testing.T) {
	db := newTestDB(t)
	mustExec(t, db, `INSERT INTO events (id, ts, event_type, agent_slug) VALUES ('shell', '2026-11-14T10:00:00Z', 'PostToolUse', 'claude-code')`)
	mustExec(t, db, `INSERT INTO events (id, ts, event_type, agent_slug) VALUES ('other', '2026-11-14T10:00:01Z', 'PostToolUse', 'claude-code')`)
	add := func(id, event, file string, first, last int, uploaded bool) {
		t.Helper()
		var synced any
		if uploaded {
			synced = "2026-11-14T10:05:00Z"
		}
		mustExec(t, db, `
			INSERT INTO event_line_ranges (id, event_id, file_path, rel_file_path, blob_sha_after, start_line, end_line, new_text_lines, synced_at)
			VALUES (?, ?, ?, ?, 'blob', ?, ?, ?, ?)`, id, event, "/repo/"+file, file, first, last, last-first+1, synced)
	}
	add("r1", "shell", "a.go", 2, 2, true)
	add("r2", "shell", "a.go", 6, 9, true)
	add("r3", "shell", "a.go", 20, 21, false)
	add("r4", "shell", "b.go", 1, 3, true)
	add("r5", "other", "a.go", 6, 9, true)

	// rowsOf lists an event's rows in a file as "id first-last lines", with
	// a "*" on the ones due for upload.
	rowsOf := func(event, file string) []string {
		t.Helper()
		rows, err := db.Query(`
			SELECT id, start_line, end_line, new_text_lines, synced_at IS NULL, COALESCE(rel_file_path, ''), COALESCE(blob_sha_after, '')
			FROM event_line_ranges WHERE event_id = ? AND file_path = ? ORDER BY start_line, end_line`, event, "/repo/"+file)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var id, rel, blob string
			var first, last, lines int
			var pending bool
			if err := rows.Scan(&id, &first, &last, &lines, &pending, &rel, &blob); err != nil {
				t.Fatal(err)
			}
			if rel != file || blob != "blob" {
				t.Errorf("row %s: rel_file_path %q, blob_sha_after %q", id, rel, blob)
			}
			if len(id) > 2 {
				id = "new"
			}
			s := fmt.Sprintf("%s %d-%d %d", id, first, last, lines)
			if pending {
				s += " *"
			}
			out = append(out, s)
		}
		return out
	}
	want := func(event, file string, rows ...string) {
		t.Helper()
		if got := rowsOf(event, file); !reflect.DeepEqual(got, rows) {
			t.Errorf("%s in %s: rows %v, want %v", event, file, got, rows)
		}
	}

	// The shell call keeps line 2, lines 7 and 8 of what was 6-9, and none
	// of 20-21. The row that is still right is not touched; the other two
	// are rewritten in place: one takes the run that is left, the last names
	// no line any more.
	if err := db.SetEventLineRanges("shell", "/repo/a.go", [][2]int{{2, 2}, {7, 8}}); err != nil {
		t.Fatal(err)
	}
	want("shell", "a.go", "r1 2-2 1", "r2 7-8 2 *", "r3 20-19 0 *")
	// Another file of the event and another event's rows are not its business.
	want("shell", "b.go", "r4 1-3 3")
	want("other", "a.go", "r5 6-9 4")

	// More runs than rows: the row that names no line is used again, and
	// the run after that gets a row of its own.
	if err := db.SetEventLineRanges("shell", "/repo/a.go", [][2]int{{2, 2}, {7, 7}, {12, 13}, {30, 30}}); err != nil {
		t.Fatal(err)
	}
	want("shell", "a.go", "r1 2-2 1", "r2 7-7 1 *", "r3 12-13 2 *", "new 30-30 1 *")

	// Nothing left: every row stays, naming no line, so the copy the cloud
	// holds of each stops counting.
	if err := db.SetEventLineRanges("shell", "/repo/a.go", nil); err != nil {
		t.Fatal(err)
	}
	want("shell", "a.go", "r1 2-1 0 *", "r2 7-6 0 *", "r3 12-11 0 *", "new 30-29 0 *")

	// An event with no ranges in a file is given none.
	if err := db.SetEventLineRanges("other", "/repo/b.go", [][2]int{{1, 2}}); err != nil {
		t.Fatal(err)
	}
	want("other", "b.go")
}
