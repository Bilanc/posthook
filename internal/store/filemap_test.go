package store

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// mapOf builds the map of a file whose lines were written by the authors
// named per line ("" for nobody).
func mapOf(lines []string, authors []string) FileMap {
	m, _ := FileMap{}.Carry(lines)
	for i, a := range authors {
		if a != "" {
			m.SetAuthor(i, LineAuthor{EventID: "event-" + a, SessionID: a, Agent: "claude-code", TS: "2026-11-13T10:00:00Z"})
		}
	}
	return m
}

// sessionsOf is the session credited with each line, "" for nobody.
func sessionsOf(m FileMap) []string {
	out := make([]string, len(m.Hashes))
	for i := range out {
		if a, ok := m.AuthorOf(i); ok {
			out[i] = a.SessionID
		}
	}
	return out
}

func TestFileMapRoundTrip(t *testing.T) {
	db := newTestDB(t)
	m := mapOf([]string{"package a", "", "func a() {}", "// by hand"}, []string{"s1", "s1", "s2", ""})
	m.SetCommit(0, "1111111111111111111111111111111111111111")
	m.AsOf = time.Date(2026, 11, 13, 9, 30, 0, 123456789, time.UTC)
	m.Blob = "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391"
	if err := db.SaveFileMap("/repo/a.go", m); err != nil {
		t.Fatal(err)
	}
	got, ok := db.FileMap("/repo/a.go")
	if !ok || !got.HasAuthors() {
		t.Fatalf("map not read back: ok=%v authors=%v", ok, got.HasAuthors())
	}
	if !reflect.DeepEqual(got.Hashes, m.Hashes) || !reflect.DeepEqual(got.Norms, m.Norms) {
		t.Errorf("fingerprints changed: %v / %v", got.Hashes, got.Norms)
	}
	if want := []string{"s1", "s1", "s2", ""}; !reflect.DeepEqual(sessionsOf(got), want) {
		t.Errorf("authors %v, want %v", sessionsOf(got), want)
	}
	if got.CommitOf(0) != "1111111111111111111111111111111111111111" || got.CommitOf(1) != "" {
		t.Errorf("commit marks: %q, %q", got.CommitOf(0), got.CommitOf(1))
	}
	if !got.AsOf.Equal(m.AsOf) || got.Blob != m.Blob {
		t.Errorf("as of %v blob %q", got.AsOf, got.Blob)
	}
	// The plain snapshot read sees the same row.
	if hashes, asOf, ok := db.FileSnapshot("/repo/a.go"); !ok || !reflect.DeepEqual(hashes, m.Hashes) || !asOf.Equal(m.AsOf) {
		t.Errorf("FileSnapshot disagrees with the map: %v %v %v", hashes, asOf, ok)
	}
}

func TestFileMapOfAnEmptyFileKeepsItsAuthorship(t *testing.T) {
	db := newTestDB(t)
	m, _ := FileMap{}.Carry(nil)
	m.AsOf = time.Now()
	if err := db.SaveFileMap("/repo/empty.txt", m); err != nil {
		t.Fatal(err)
	}
	got, ok := db.FileMap("/repo/empty.txt")
	if !ok || !got.HasAuthors() || len(got.Hashes) != 0 {
		t.Fatalf("empty file: ok=%v authors=%v lines=%d", ok, got.HasAuthors(), len(got.Hashes))
	}
}

func TestSnapshotWrittenBeforeTheMapHasNoAuthors(t *testing.T) {
	db := newTestDB(t)
	hashes := []uint64{LineHash("a"), LineHash("b")}
	// A row as schema v16 wrote it: fingerprints and an instant, nothing else.
	mustExec(t, db, `INSERT INTO file_snapshots (file_path, line_hashes, updated_at) VALUES (?, ?, ?)`,
		"/repo/old.go", encodeHashes(hashes), "2026-11-10T10:00:00Z")
	got, ok := db.FileMap("/repo/old.go")
	if !ok || got.HasAuthors() || got.Norms != nil || !reflect.DeepEqual(got.Hashes, hashes) {
		t.Fatalf("old row: ok=%v authors=%v norms=%v hashes=%v", ok, got.HasAuthors(), got.Norms, got.Hashes)
	}
	// It can still be lined up with a newer copy, on the exact fingerprints.
	next, from := got.Carry([]string{"new", "a", "b"})
	if want := []int{-1, 0, 1}; !reflect.DeepEqual(from, want) {
		t.Errorf("from %v, want %v", from, want)
	}
	if !next.HasAuthors() || next.Norms == nil {
		t.Error("the carried map should have authorship columns")
	}
}

func TestFileMapStoresNoFileContentAndDropsUnusedAuthors(t *testing.T) {
	db := newTestDB(t)
	secret := "API_TOKEN=sk-live-0123456789abcdef"
	m := mapOf([]string{"# config", secret}, []string{"s1", "s2"})
	// s1's only line is rewritten by s3: s1 must not linger in the row.
	m.SetAuthor(0, LineAuthor{EventID: "event-s3", SessionID: "s3"})
	m.AsOf = time.Now()
	if err := db.SaveFileMap("/repo/.env", m); err != nil {
		t.Fatal(err)
	}
	var norms, owners []byte
	var meta string
	mustQueryRow(t, db, `SELECT line_norms, line_owners, owners FROM file_snapshots WHERE file_path = '/repo/.env'`, nil, &norms, &owners, &meta)
	for _, col := range []string{string(norms), string(owners), meta} {
		if strings.Contains(col, "sk-live") || strings.Contains(col, "API_TOKEN") {
			t.Fatalf("a column holds file content: %q", col)
		}
	}
	if strings.Contains(meta, "event-s1") || !strings.Contains(meta, "event-s2") || !strings.Contains(meta, "event-s3") {
		t.Errorf("authors kept: %s", meta)
	}
}

func TestCarryKeepsAuthorsWhereLinesAreKept(t *testing.T) {
	old := []string{"func a() {", "\treturn 1", "}", "", "func b() {", "\treturn 2", "}"}
	m := mapOf(old, []string{"", "s1", "", "", "s2", "s2", "s2"})
	m.SetCommit(1, "c1")

	cases := []struct {
		name  string
		lines []string
		want  []string
	}{
		{"unchanged", old, []string{"", "s1", "", "", "s2", "s2", "s2"}},
		{"lines added above", append([]string{"// header", ""}, old...), []string{"", "", "", "s1", "", "", "s2", "s2", "s2"}},
		{"a line rewritten by hand", []string{"func a() {", "\treturn 100", "}", "", "func b() {", "\treturn 2", "}"}, []string{"", "", "", "", "s2", "s2", "s2"}},
		{"a line removed", []string{"func a() {", "\treturn 1", "}", "", "func b() {", "}"}, []string{"", "s1", "", "", "s2", "s2"}},
		{"whitespace only", []string{"func a() {", "    return 1", "}", "", "func b()  {", "  return 2", "}"}, []string{"", "s1", "", "", "s2", "s2", "s2"}},
		{"everything replaced", []string{"x", "y"}, []string{"", ""}},
		{"emptied", nil, []string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			next, from := m.Carry(c.lines)
			if len(from) != len(c.lines) || len(next.Hashes) != len(c.lines) {
				t.Fatalf("lengths: from=%d hashes=%d lines=%d", len(from), len(next.Hashes), len(c.lines))
			}
			if got := sessionsOf(next); !reflect.DeepEqual(got, c.want) {
				t.Errorf("authors %q, want %q", got, c.want)
			}
		})
	}

	t.Run("a moved block keeps its authors, and nobody else gains one", func(t *testing.T) {
		moved := []string{"func b() {", "\treturn 2", "}", "", "func a() {", "\treturn 1", "}"}
		next, _ := m.Carry(moved)
		got := sessionsOf(next)
		// The lines that say something are found again wherever they went.
		if got[0] != "s2" || got[1] != "s2" || got[5] != "s1" {
			t.Errorf("authors after the move: %q", got)
		}
		// "func a() {" and the blank line never had an author.
		if got[4] != "" || got[3] != "" {
			t.Errorf("a line nobody wrote gained an author: %q", got)
		}
		// Of the two closing braces one was s2's and one nobody's, and it
		// stays that way: four lines with an author in all, as before.
		authored := 0
		for _, s := range got {
			if s != "" {
				authored++
			}
		}
		if authored != 4 || (got[2] == "") == (got[6] == "") {
			t.Errorf("authors after the move: %q, want exactly one of the braces to be s2's", got)
		}
	})

	t.Run("the commit a line was counted in travels with it", func(t *testing.T) {
		next, _ := m.Carry(append([]string{"// header"}, old...))
		if next.CommitOf(2) != "c1" || next.CommitOf(1) != "" {
			t.Errorf("commit marks after an insert above: %q %q", next.CommitOf(2), next.CommitOf(1))
		}
	})

	t.Run("carry does not change the map it starts from", func(t *testing.T) {
		before := sessionsOf(m)
		next, _ := m.Carry(old)
		next.SetAuthor(0, LineAuthor{EventID: "event-s9", SessionID: "s9"})
		next.SetCommit(0, "c9")
		if !reflect.DeepEqual(sessionsOf(m), before) || len(m.Authors) != 2 || len(m.Commits) != 1 {
			t.Errorf("the source map changed: %q authors=%d commits=%d", sessionsOf(m), len(m.Authors), len(m.Commits))
		}
	})
}

func TestSetAuthorStartsALineAfresh(t *testing.T) {
	m := mapOf([]string{"a", "b"}, []string{"s1", "s1"})
	m.SetCommit(0, "c1")
	m.SetAuthor(0, LineAuthor{EventID: "event-s2", SessionID: "s2"})
	if a, _ := m.AuthorOf(0); a.SessionID != "s2" || m.CommitOf(0) != "" {
		t.Errorf("rewritten line: author %q commit %q", a.SessionID, m.CommitOf(0))
	}
	// Out of range is ignored, not a panic.
	m.SetAuthor(7, LineAuthor{EventID: "x"})
	m.SetCommit(-1, "c")
}

func TestUpdateFileMapMarksLeavesANewerRowAlone(t *testing.T) {
	db := newTestDB(t)
	m := mapOf([]string{"a", "b"}, []string{"s1", "s1"})
	m.AsOf = time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	if err := db.SaveFileMap("/repo/f", m); err != nil {
		t.Fatal(err)
	}
	read, _ := db.FileMap("/repo/f")

	// The worker handles a later edit of the file before the commit path
	// writes its marks back.
	newer := mapOf([]string{"a", "b", "c"}, []string{"s1", "s1", "s2"})
	newer.AsOf = m.AsOf.Add(time.Second)
	if err := db.SaveFileMap("/repo/f", newer); err != nil {
		t.Fatal(err)
	}
	read.SetCommit(0, "c1")
	if written, err := db.UpdateFileMapMarks("/repo/f", read); err != nil || written {
		t.Fatalf("marks written over a newer row: written=%v err=%v", written, err)
	}
	if got, _ := db.FileMap("/repo/f"); len(got.Hashes) != 3 || got.CommitOf(0) != "" {
		t.Errorf("the newer row was changed: %d lines, mark %q", len(got.Hashes), got.CommitOf(0))
	}

	// With the row as it was read, the marks land.
	again, _ := db.FileMap("/repo/f")
	again.SetCommit(2, "c2")
	if written, err := db.UpdateFileMapMarks("/repo/f", again); err != nil || !written {
		t.Fatalf("marks not written: written=%v err=%v", written, err)
	}
	if got, _ := db.FileMap("/repo/f"); got.CommitOf(2) != "c2" {
		t.Errorf("mark %q, want c2", got.CommitOf(2))
	}
}

func TestNormHashIgnoresWhitespaceOnly(t *testing.T) {
	same := [][2]string{
		{"\treturn x", "    return x"},
		{"a = b", "a=b"},
		{"foo(a, b)\r", "foo(a,b)"},
		{"", "   \t"},
	}
	for _, p := range same {
		if NormHash(p[0]) != NormHash(p[1]) {
			t.Errorf("%q and %q should read the same", p[0], p[1])
		}
	}
	if NormHash("return x") == NormHash("return y") || NormHash("ab") == NormHash("ba") {
		t.Error("different text reads the same")
	}
}

func TestContentAttributionSince(t *testing.T) {
	db := newTestDB(t)
	if _, ok := db.ContentAttributionSince(); ok {
		t.Fatal("a database with no version row matches by content")
	}
	mustExec(t, db, `INSERT INTO schema_version (version, applied_at) VALUES (16, '2026-11-01 08:00:00')`)
	if _, ok := db.ContentAttributionSince(); ok {
		t.Fatal("a v16 database matches by content")
	}
	mustExec(t, db, `INSERT INTO schema_version (version, applied_at) VALUES (17, '2026-11-13 09:00:00')`)
	since, ok := db.ContentAttributionSince()
	if !ok || !since.Equal(time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("since %v ok=%v", since, ok)
	}
	for at, want := range map[string]bool{
		"2026-11-13T08:59:59+00:00": false,
		"2026-11-13T09:00:00+00:00": true,
		"2026-11-13T11:00:00+02:00": true,
		"2026-11-13T10:59:59+02:00": false,
		"not a time":                false,
	} {
		if got := db.matchesByContent(at); got != want {
			t.Errorf("commit at %s: by content = %v, want %v", at, got, want)
		}
	}
}

func TestMigrationFromV16MakesTheSnapshotAMap(t *testing.T) {
	// A database as v16 left it: file_snapshots without the map columns, no
	// commit_ai_lines, and one snapshot row.
	if !strings.Contains(schemaSQL, fileSnapshotsDDL) {
		t.Fatal("test setup: the schema no longer contains the snapshot DDL as declared")
	}
	ddl := strings.Replace(schemaSQL, fileSnapshotsDDL, `CREATE TABLE IF NOT EXISTS file_snapshots (
  file_path TEXT PRIMARY KEY,
  line_hashes BLOB,
  updated_at TEXT NOT NULL
);`, 1)
	db := openBareDB(t, ddl)
	mustExec(t, db, `DROP TABLE commit_ai_lines`)
	hashes := []uint64{LineHash("a"), LineHash("b")}
	mustExec(t, db, `INSERT INTO file_snapshots (file_path, line_hashes, updated_at) VALUES ('/repo/a.go', ?, '2026-11-10T10:00:00Z')`, encodeHashes(hashes))
	mustExec(t, db, `INSERT INTO schema_version (version, applied_at) VALUES (16, '2026-11-01 08:00:00')`)

	if err := db.migrate(); err != nil {
		t.Fatal(err)
	}
	if v := db.currentSchemaVersion(); v != schemaVersion {
		t.Fatalf("schema version %d, want %d", v, schemaVersion)
	}
	// The old snapshot is kept and has no authors yet.
	got, ok := db.FileMap("/repo/a.go")
	if !ok || got.HasAuthors() || !reflect.DeepEqual(got.Hashes, hashes) {
		t.Fatalf("old snapshot: ok=%v authors=%v hashes=%v", ok, got.HasAuthors(), got.Hashes)
	}
	// Both new stores work.
	m := mapOf([]string{"a"}, []string{"s1"})
	m.AsOf = time.Now()
	if err := db.SaveFileMap("/repo/b.go", m); err != nil {
		t.Fatalf("save a map after migration: %v", err)
	}
	if err := db.SaveCommitLines("c1", "b.go", []CommitLines{{SessionID: "s1", Numbers: []int{1}, Norms: []uint64{NormHash("a")}}}); err != nil {
		t.Fatalf("save commit lines after migration: %v", err)
	}
	// Commits made from now on are matched by content; earlier ones are not.
	if since, ok := db.ContentAttributionSince(); !ok || time.Since(since) > time.Minute {
		t.Errorf("content attribution since %v ok=%v", since, ok)
	}
	if db.matchesByContent("2026-11-12T10:00:00Z") {
		t.Error("a commit from before the upgrade is matched by content")
	}
	// The column guards are safe to run again.
	if err := db.applyMigrations(); err != nil {
		t.Fatalf("second applyMigrations: %v", err)
	}
}

func TestLocalTablesAreNotSynced(t *testing.T) {
	// Tables that hold fingerprints of file content, or working state.
	local := map[string]bool{
		"file_snapshots": true, "session_file_lines": true, "commit_ai_lines": true,
		"pending_looks": true, "repo_looks": true,
	}
	for _, table := range SyncableTables {
		if local[table] {
			t.Errorf("%s must stay on the machine", table)
		}
	}
	// A synced table goes out with SELECT *, so a column added to one is
	// synced. These are the columns each has; changing one is a change to
	// what leaves the machine and to the cloud's schema, and has to be made
	// here on purpose.
	want := map[string]int{
		"repositories": 7, "sessions": 16, "events": 17, "commits": 14, "commit_files": 5,
		"commit_sessions": 12, "commit_session_files": 8, "event_line_ranges": 10, "session_prompts": 10,
	}
	db := newTestDB(t)
	if len(want) != len(SyncableTables) {
		t.Fatalf("%d synced tables, this test knows %d", len(SyncableTables), len(want))
	}
	for _, table := range SyncableTables {
		cols, err := db.columnSet(table)
		if err != nil || len(cols) != want[table] {
			t.Errorf("%s has %d columns (err %v), want %d", table, len(cols), err, want[table])
		}
	}
}

func TestMigrationFromV17AddsTheLookTables(t *testing.T) {
	// A database as v17 left it: no pending_looks, no repo_looks, and no
	// index on the commit id that blame looks a commit up by. v18 adds them
	// as whole tables and an index, which the schema itself creates on an
	// upgrade (migrate applies it before the column guards).
	db := openBareDB(t, schemaSQL)
	mustExec(t, db, `DROP TABLE pending_looks`)
	mustExec(t, db, `DROP TABLE repo_looks`)
	mustExec(t, db, `DROP INDEX idx_commits_sha`)
	mustExec(t, db, `INSERT INTO schema_version (version, applied_at) VALUES (17, '2026-11-13 09:00:00')`)

	if err := db.migrate(); err != nil {
		t.Fatal(err)
	}
	if v := db.currentSchemaVersion(); v != schemaVersion {
		t.Fatalf("schema version %d, want %d", v, schemaVersion)
	}
	fired := time.Now().Add(-time.Second)
	if err := db.AddPendingLook(PendingLook{Kind: LookShell, EventID: "e1", Root: "/repo", FiredAt: fired}); err != nil {
		t.Fatalf("queue a look after migration: %v", err)
	}
	if n := db.CountPendingLooks(); n != 1 {
		t.Errorf("%d pending look(s), want 1", n)
	}
	if err := db.SaveRepoLook("/repo", RepoLook{Head: "abc", LookedAt: fired, Partial: true}); err != nil {
		t.Fatalf("record a look after migration: %v", err)
	}
	if look, ok := db.RepoLook("/repo"); !ok || look.Head != "abc" || !look.Partial {
		t.Errorf("the recorded look reads back as %+v (found %v)", look, ok)
	}
	var indexes int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_commits_sha'`).Scan(&indexes); err != nil || indexes != 1 {
		t.Errorf("idx_commits_sha: %d index(es), err %v", indexes, err)
	}
	if err := db.applyMigrations(); err != nil {
		t.Fatalf("second applyMigrations: %v", err)
	}
}
