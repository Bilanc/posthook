package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedDB writes a schema'd database with one recorded repository at path
// and closes it, the way an installed posthook leaves it on disk.
func seedDB(t *testing.T, path, root string) {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db := &DB{DB: sqlDB}
	if _, err := db.Exec(schemaSQL); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	if _, err := db.EnsureRepository(root); err != nil {
		t.Fatalf("ensure repository: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestOpenReadOnlyReadsWithoutWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "posthook.db")
	seedDB(t, path, "/srv/repo")

	db, err := openReadOnly(path)
	if err != nil {
		t.Fatalf("openReadOnly: %v", err)
	}
	defer db.Close()

	roots, err := db.RepositoryRoots()
	if err != nil {
		t.Fatalf("RepositoryRoots: %v", err)
	}
	if len(roots) != 1 || roots[0] != "/srv/repo" {
		t.Fatalf("roots = %v, want [/srv/repo]", roots)
	}

	_, err = db.Exec(`INSERT INTO schema_version (version) VALUES (999)`)
	if err == nil {
		t.Fatal("a write through the read-only handle succeeded")
	}
	if !strings.Contains(err.Error(), "readonly") && !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("unexpected write error: %v", err)
	}
}

func TestOpenReadOnlyNeverCreatesTheDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "posthook.db")
	if _, err := openReadOnly(path); err == nil {
		t.Fatal("openReadOnly on a missing file should fail")
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("openReadOnly created the database file")
	}
	if _, err := os.Stat(filepath.Dir(path)); err == nil {
		t.Fatal("openReadOnly created the data directory")
	}
}
