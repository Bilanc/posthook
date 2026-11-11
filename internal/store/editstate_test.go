package store

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestFileSnapshotRoundTrip(t *testing.T) {
	db := newTestDB(t)
	if _, _, ok := db.FileSnapshot("/repo/a.go"); ok {
		t.Fatal("snapshot of a file never saved")
	}
	want := []uint64{LineHash("package a"), LineHash(""), LineHash("func a() {}")}
	at := time.Date(2026, 11, 10, 9, 30, 0, 123456789, time.UTC)
	if err := db.SaveFileSnapshot("/repo/a.go", want, at); err != nil {
		t.Fatal(err)
	}
	got, asOf, ok := db.FileSnapshot("/repo/a.go")
	if !ok || !reflect.DeepEqual(got, want) || !asOf.Equal(at) {
		t.Fatalf("got %v as of %v ok=%v", got, asOf, ok)
	}
	// Saving again replaces, it does not add a row.
	later := at.Add(time.Minute)
	if err := db.SaveFileSnapshot("/repo/a.go", want[:1], later); err != nil {
		t.Fatal(err)
	}
	var rows int
	mustQueryRow(t, db, `SELECT COUNT(*) FROM file_snapshots`, nil, &rows)
	if got, asOf, _ := db.FileSnapshot("/repo/a.go"); rows != 1 || len(got) != 1 || !asOf.Equal(later) {
		t.Fatalf("rows=%d hashes=%v asOf=%v", rows, got, asOf)
	}
	// An empty file is a snapshot too, not the absence of one.
	if err := db.SaveFileSnapshot("/repo/empty.txt", nil, at); err != nil {
		t.Fatal(err)
	}
	if got, _, ok := db.FileSnapshot("/repo/empty.txt"); !ok || len(got) != 0 {
		t.Fatalf("empty file: hashes=%v ok=%v", got, ok)
	}
	if err := db.DropFileSnapshot("/repo/a.go"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := db.FileSnapshot("/repo/a.go"); ok {
		t.Fatal("dropped snapshot is still there")
	}
}

func TestFileSnapshotHoldsNoFileContent(t *testing.T) {
	// The table keeps a fingerprint per line and nothing else: a secret that
	// sits in an edited file must not be readable from the database.
	db := newTestDB(t)
	secret := "API_TOKEN=sk-live-0123456789abcdef"
	lines := []string{"# config", secret, "debug = false"}
	hashes := make([]uint64, len(lines))
	for i, l := range lines {
		hashes[i] = LineHash(l)
	}
	if err := db.SaveFileSnapshot("/repo/config.ini", hashes, time.Now()); err != nil {
		t.Fatal(err)
	}
	var blob []byte
	mustQueryRow(t, db, `SELECT line_hashes FROM file_snapshots WHERE file_path = '/repo/config.ini'`, nil, &blob)
	if len(blob) != 8*len(lines) {
		t.Fatalf("stored %d bytes for %d lines, want 8 per line", len(blob), len(lines))
	}
	if strings.Contains(string(blob), "sk-live") || strings.Contains(string(blob), "API_TOKEN") {
		t.Fatal("snapshot holds file text")
	}
	cols, err := db.columnSet("file_snapshots")
	if err != nil || cols["content"] {
		t.Fatalf("file_snapshots must not have a content column (cols=%v err=%v)", cols, err)
	}
}

func TestSessionFileLinesRoundTripAndCap(t *testing.T) {
	db := newTestDB(t)
	if got := db.SessionFileLines("s1", "/repo/a.go"); len(got) != 0 {
		t.Fatalf("lines before any save: %v", got)
	}
	want := []uint64{LineHash("a"), LineHash("b"), LineHash("a")}
	if err := db.SaveSessionFileLines("s1", "/repo/a.go", want); err != nil {
		t.Fatal(err)
	}
	if got := db.SessionFileLines("s1", "/repo/a.go"); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if got := db.SessionFileLines("s2", "/repo/a.go"); len(got) != 0 {
		t.Fatalf("another session sees these lines: %v", got)
	}
	// An empty list removes the row.
	if err := db.SaveSessionFileLines("s1", "/repo/a.go", nil); err != nil {
		t.Fatal(err)
	}
	var rows int
	mustQueryRow(t, db, `SELECT COUNT(*) FROM session_file_lines`, nil, &rows)
	if rows != 0 {
		t.Fatalf("%d row(s) left after saving no lines", rows)
	}
	// Past the cap the oldest lines are forgotten.
	many := make([]uint64, maxSessionFileLines+3)
	for i := range many {
		many[i] = uint64(i)
	}
	if err := db.SaveSessionFileLines("s1", "/repo/a.go", many); err != nil {
		t.Fatal(err)
	}
	got := db.SessionFileLines("s1", "/repo/a.go")
	if len(got) != maxSessionFileLines || got[0] != 3 {
		t.Fatalf("kept %d lines starting at %d", len(got), got[0])
	}
}

func TestLineHashIgnoresTrailingCR(t *testing.T) {
	if LineHash("return 1;\r") != LineHash("return 1;") {
		t.Error("a CRLF line should hash like its LF form")
	}
	if LineHash("return 1;") == LineHash("return 2;") {
		t.Error("different lines hash the same")
	}
}

func TestPruneEditStateIfDueDropsOnlyStaleRows(t *testing.T) {
	db := newTestDB(t)
	old := time.Now().UTC().Add(-editStateMaxAge - time.Hour).Format(time.RFC3339)
	if err := db.SaveFileSnapshot("/repo/fresh.go", []uint64{1}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveFileSnapshot("/repo/stale.go", []uint64{1}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveSessionFileLines("s-fresh", "/repo/fresh.go", []uint64{1}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveSessionFileLines("s-stale", "/repo/stale.go", []uint64{1}); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `UPDATE file_snapshots SET updated_at = ? WHERE file_path = '/repo/stale.go'`, old)
	mustExec(t, db, `UPDATE session_file_lines SET updated_at = ? WHERE session_id = 's-stale'`, old)

	removed, ran, err := db.PruneEditStateIfDue()
	if err != nil || !ran || removed != 2 {
		t.Fatalf("removed=%d ran=%v err=%v, want 2 rows on the first pass", removed, ran, err)
	}
	if _, _, ok := db.FileSnapshot("/repo/fresh.go"); !ok {
		t.Error("fresh snapshot pruned")
	}
	if len(db.SessionFileLines("s-fresh", "/repo/fresh.go")) != 1 {
		t.Error("fresh lines pruned")
	}
	// Within a day of the last pass it does not run again.
	if _, ran, err := db.PruneEditStateIfDue(); err != nil || ran {
		t.Fatalf("second pass ran=%v err=%v", ran, err)
	}
}

func TestEditStateTablesAreNotSynced(t *testing.T) {
	for _, table := range SyncableTables {
		if table == "file_snapshots" || table == "session_file_lines" {
			t.Errorf("%s holds file content and must stay on the machine", table)
		}
	}
}

// oldEventsTail and newEventsTail are the end of the events DDL before and
// after the measured-line columns, for building a database as an older
// release left it.
const newEventsTail = ",\n  lines_generated INTEGER,\n  lines_reworked INTEGER,\n  file_lines TEXT\n);"

func openBareDB(t *testing.T, ddl string) *DB {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "old.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	db := &DB{DB: sqlDB}
	mustExec(t, db, ddl)
	return db
}

func TestMigrationFromV14AddsTheMeasuredLineColumns(t *testing.T) {
	// A database as v14 left it: an events table without the columns, and
	// neither working table.
	if !strings.Contains(schemaSQL, newEventsTail) {
		t.Fatal("test setup: the events DDL no longer ends the way this test expects")
	}
	ddl := strings.Replace(schemaSQL, newEventsTail, "\n);", 1)
	db := openBareDB(t, ddl)
	mustExec(t, db, `DROP TABLE file_snapshots`)
	mustExec(t, db, `DROP TABLE session_file_lines`)
	mustExec(t, db, `INSERT INTO events (id, ts, event_type, agent_slug, lines_added) VALUES ('old', '2026-01-01T00:00:00Z', 'PostToolUse', 'codex', 7)`)
	mustExec(t, db, `INSERT INTO schema_version (version) VALUES (14)`)

	if err := db.migrate(); err != nil {
		t.Fatal(err)
	}
	if v := db.currentSchemaVersion(); v != schemaVersion {
		t.Fatalf("schema version %d, want %d", v, schemaVersion)
	}
	// The old event keeps NULL in all three: it was never measured.
	var generated, reworked sql.NullInt64
	var fileLines sql.NullString
	var linesAdded int
	mustQueryRow(t, db, `SELECT lines_generated, lines_reworked, file_lines, lines_added FROM events WHERE id = 'old'`, nil, &generated, &reworked, &fileLines, &linesAdded)
	if generated.Valid || reworked.Valid || fileLines.Valid || linesAdded != 7 {
		t.Errorf("old event: generated=%v reworked=%v file_lines=%v lines_added=%d", generated, reworked, fileLines, linesAdded)
	}
	assertSnapshotTableUsable(t, db)
	// The column guards are safe to run again (Open gates migrate() itself
	// on the version, so it is not called twice).
	if err := db.applyMigrations(); err != nil {
		t.Fatalf("second applyMigrations: %v", err)
	}
}

func TestMigrationFromPreReleaseV15RebuildsTheSnapshotTable(t *testing.T) {
	// A database that ran an early build of v15: the two line-count columns
	// are there, file_lines is not, and file_snapshots holds file copies.
	ddl := strings.Replace(schemaSQL, ",\n  file_lines TEXT\n);", "\n);", 1)
	ddl = strings.Replace(ddl, fileSnapshotsDDL, `CREATE TABLE IF NOT EXISTS file_snapshots (
  file_path TEXT PRIMARY KEY,
  content BLOB NOT NULL,
  updated_at TEXT NOT NULL
);`, 1)
	db := openBareDB(t, ddl)
	cols, err := db.columnSet("file_snapshots")
	if err != nil || !cols["content"] || cols["line_hashes"] {
		t.Fatalf("test setup: expected the old snapshot layout (cols=%v err=%v)", cols, err)
	}
	mustExec(t, db, `INSERT INTO file_snapshots (file_path, content, updated_at) VALUES ('/repo/a.go', x'1f8b0800', '2026-11-10T10:00:00Z')`)
	mustExec(t, db, `INSERT INTO events (id, ts, event_type, agent_slug, lines_generated, lines_reworked) VALUES ('e', '2026-11-10T10:00:00Z', 'PostToolUse', 'codex', 4, 1)`)
	mustExec(t, db, `INSERT INTO schema_version (version) VALUES (15)`)

	if err := db.migrate(); err != nil {
		t.Fatal(err)
	}
	if v := db.currentSchemaVersion(); v != schemaVersion {
		t.Fatalf("schema version %d, want %d", v, schemaVersion)
	}
	cols, err = db.columnSet("file_snapshots")
	if err != nil || cols["content"] || !cols["line_hashes"] {
		t.Fatalf("file_snapshots was not rebuilt (cols=%v err=%v)", cols, err)
	}
	// The file copy is gone, not carried over.
	if _, _, ok := db.FileSnapshot("/repo/a.go"); ok {
		t.Error("an old snapshot row survived the rebuild")
	}
	assertSnapshotTableUsable(t, db)
	// Measured counts already recorded are kept, and the new column is there.
	var generated, reworked int
	var fileLines sql.NullString
	mustQueryRow(t, db, `SELECT lines_generated, lines_reworked, file_lines FROM events WHERE id = 'e'`, nil, &generated, &reworked, &fileLines)
	if generated != 4 || reworked != 1 || fileLines.Valid {
		t.Errorf("event: generated=%d reworked=%d file_lines=%v", generated, reworked, fileLines)
	}
}

func assertSnapshotTableUsable(t *testing.T, db *DB) {
	t.Helper()
	want := []uint64{LineHash("a"), LineHash("b")}
	if err := db.SaveFileSnapshot("/repo/new.go", want, time.Now()); err != nil {
		t.Fatalf("save after migration: %v", err)
	}
	if got, _, ok := db.FileSnapshot("/repo/new.go"); !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("read after migration: %v ok=%v", got, ok)
	}
	if err := db.SaveSessionFileLines("s", "/repo/new.go", want); err != nil {
		t.Fatalf("ledger after migration: %v", err)
	}
}
