package sync

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/bilanc/posthook/internal/config"
	"github.com/bilanc/posthook/internal/store"
)

var (
	testStoreOnce sync.Once
	testStore     *store.DB
	testStoreErr  error
)

// openTestStore points HOME at a throwaway dir so store.Open() creates a
// production-schema database there, with migrations applied exactly as on
// an engineer's machine. Open() caches one handle per process, so the store
// is opened once and emptied before each test instead of reopened.
func openTestStore(t *testing.T) *store.DB {
	t.Helper()
	testStoreOnce.Do(func() {
		home, err := os.MkdirTemp("", "posthook-sync-test-")
		if err != nil {
			testStoreErr = err
			return
		}
		os.Setenv("HOME", home)
		testStore, testStoreErr = store.Open()
	})
	if testStoreErr != nil {
		t.Fatalf("store.Open: %v", testStoreErr)
	}
	// FK-safe order: children before parents.
	for _, table := range []string{
		"event_line_ranges", "commit_session_files", "commit_sessions", "commit_files",
		"session_prompts", "events", "commits", "sessions", "repositories", "sync_state",
	} {
		if _, err := testStore.Exec("DELETE FROM " + table); err != nil {
			t.Fatalf("reset %s: %v", table, err)
		}
	}
	return testStore
}

func seedContent(t *testing.T, db *store.DB) {
	t.Helper()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO repositories (id, name, root_path) VALUES ('r1', 'repo', '/repo')`)
	exec(`INSERT INTO sessions (id, agent_slug, model_slug, repo_id, started_at, engineer_email, input_tokens, output_tokens)
	      VALUES ('s1', 'claude-code', 'claude-fable-5-1', 'r1', '2026-11-03T09:00:00Z', 'eng@example.com', 10, 20)`)
	editPayload := `{"hook_event_name":"PostToolUse","session_id":"s1","tool_name":"Edit","tool_use_id":"tu1","cwd":"/repo",
	  "tool_input":{"file_path":"/repo/a.go","old_string":"SECRET_OLD","new_string":"SECRET_NEW\nline2\n"},
	  "tool_response":{"filePath":"/repo/a.go","originalFile":"SECRET_ORIGINAL_FILE"}}`
	bashPayload := `{"hook_event_name":"PostToolUse","session_id":"s1","tool_name":"Bash",
	  "tool_input":{"command":"SECRET_COMMAND","description":"SECRET_DESCRIPTION"},
	  "tool_response":{"stdout":"SECRET_STDOUT","stderr":""}}`
	stopPayload := `{"hook_event_name":"Stop","session_id":"s1","last_assistant_message":"SECRET_REPLY","stop_hook_active":false}`
	exec(`INSERT INTO events (id, session_id, ts, event_type, agent_slug, cwd, file_path, repo_id, rel_file_path, payload)
	      VALUES ('e1', 's1', '2026-11-03T09:01:00Z', 'PostToolUse', 'claude-code', '/repo', '/repo/a.go', 'r1', 'a.go', ?)`, editPayload)
	exec(`INSERT INTO events (id, session_id, ts, event_type, agent_slug, cwd, payload)
	      VALUES ('e2', 's1', '2026-11-03T09:02:00Z', 'PostToolUse', 'claude-code', '/repo', ?)`, bashPayload)
	exec(`INSERT INTO events (id, session_id, ts, event_type, agent_slug, cwd, payload)
	      VALUES ('e3', 's1', '2026-11-03T09:03:00Z', 'Stop', 'claude-code', '/repo', ?)`, stopPayload)
	exec(`INSERT INTO events (id, session_id, ts, event_type, agent_slug, cwd, payload)
	      VALUES ('e4', NULL, '2026-11-03T09:04:00Z', 'hook_misfire', 'claude-code', '/repo', NULL)`)
	exec(`INSERT INTO commits (id, repo_id, sha, author_email, committed_at, message, lines_added, lines_removed, files_changed)
	      VALUES ('c1', 'r1', 'abc123', 'eng@example.com', '2026-11-03T09:30:00Z', 'SECRET_COMMIT_SUBJECT', 3, 0, 1)`)
	exec(`INSERT INTO session_prompts (id, session_id, ts, seq, agent_slug, source, prompt_text)
	      VALUES ('p1', 's1', '2026-11-03T09:00:30Z', 1, 'claude-code', 'transcript', 'SECRET_PROMPT please fix the bug')`)
}

var seededSecrets = []string{
	"SECRET_OLD", "SECRET_NEW", "SECRET_ORIGINAL_FILE", "SECRET_COMMAND", "SECRET_DESCRIPTION",
	"SECRET_STDOUT", "SECRET_REPLY", "SECRET_COMMIT_SUBJECT", "SECRET_PROMPT",
}

// captureServer records every ingest POST (body + the sync-mode header).
type captureServer struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []string
	modes  []string
}

func newCaptureServer(t *testing.T) *captureServer {
	t.Helper()
	cs := &captureServer{}
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != ingestPath {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		cs.mu.Lock()
		cs.bodies = append(cs.bodies, string(b))
		cs.modes = append(cs.modes, r.Header.Get("X-Posthook-Sync-Mode"))
		cs.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"received":{}}`))
	}))
	t.Cleanup(cs.Close)
	return cs
}

func cloudCfg(endpoint, mode string) config.CloudConfig {
	return config.CloudConfig{Endpoint: endpoint, Token: "tok", Enabled: true, FlushIntervalSecs: 5, SyncMode: mode}
}

func TestFlush_FullModeShipsContentVerbatim(t *testing.T) {
	db := openTestStore(t)
	seedContent(t, db)
	srv := newCaptureServer(t)

	res, err := Flush(context.Background(), db, cloudCfg(srv.URL, config.SyncModeFull))
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped || len(srv.bodies) != 1 {
		t.Fatalf("expected one POST, got res=%+v posts=%d", res, len(srv.bodies))
	}
	body := srv.bodies[0]
	for _, s := range seededSecrets {
		if !strings.Contains(body, s) {
			t.Errorf("full mode dropped %q", s)
		}
	}
	if srv.modes[0] != "full" {
		t.Errorf("X-Posthook-Sync-Mode = %q", srv.modes[0])
	}
	if strings.Contains(body, redactedMarker) {
		t.Errorf("full mode must not stamp %s", redactedMarker)
	}
}

func TestFlush_MetadataOnlyStripsContentAndKeepsLocalStore(t *testing.T) {
	db := openTestStore(t)
	seedContent(t, db)
	srv := newCaptureServer(t)

	res, err := Flush(context.Background(), db, cloudCfg(srv.URL, config.SyncModeMetadataOnly))
	if err != nil {
		t.Fatal(err)
	}
	if len(srv.bodies) != 1 {
		t.Fatalf("expected one POST, got %d", len(srv.bodies))
	}
	body := srv.bodies[0]
	for _, s := range seededSecrets {
		if strings.Contains(body, s) {
			t.Errorf("metadata-only leaked %q", s)
		}
	}
	if srv.modes[0] != config.SyncModeMetadataOnly {
		t.Errorf("X-Posthook-Sync-Mode = %q", srv.modes[0])
	}

	var payload ingestPayload
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.SchemaVersion != schemaVersion {
		t.Errorf("schema_version = %d", payload.SchemaVersion)
	}
	// Every row still ships — redaction changes content, never row counts.
	want := map[string]int{"repositories": 1, "sessions": 1, "events": 4, "commits": 1, "session_prompts": 1}
	for table, n := range want {
		if got := len(payload.Tables[table]); got != n {
			t.Errorf("%s rows = %d, want %d", table, got, n)
		}
	}
	for _, ev := range payload.Tables["events"] {
		switch ev["id"] {
		case "e1":
			if ev["lines_added"] != float64(2) {
				t.Errorf("e1 lines_added = %v, want 2", ev["lines_added"])
			}
			var p map[string]any
			_ = json.Unmarshal([]byte(ev["payload"].(string)), &p)
			if p["tool_name"] != "Edit" || p[redactedMarker] != true || p["tool_input"].(map[string]any)["file_path"] != "/repo/a.go" {
				t.Errorf("e1 payload lost metadata: %v", p)
			}
		case "e4":
			if ev["payload"] != nil {
				t.Errorf("misfire payload should stay null, got %v", ev["payload"])
			}
		}
	}
	p := payload.Tables["session_prompts"][0]
	if p["prompt_text"] != "" || p["redacted"] != true || p["prompt_chars"] != float64(len("SECRET_PROMPT please fix the bug")) {
		t.Errorf("prompt row not redacted: %v", p)
	}
	if payload.Tables["commits"][0]["message"] != nil {
		t.Errorf("commit message survived")
	}
	// Sessions carry no free text and must be byte-for-byte what full mode sends.
	if s := payload.Tables["sessions"][0]; s["engineer_email"] != "eng@example.com" || s["input_tokens"] != float64(10) {
		t.Errorf("session row altered: %v", s)
	}

	// The laptop keeps everything: redaction is applied to the outgoing copy only.
	var local string
	if err := db.QueryRow(`SELECT payload FROM events WHERE id = 'e1'`).Scan(&local); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(local, "SECRET_NEW") {
		t.Errorf("local store was altered by redaction")
	}
	var prompt string
	_ = db.QueryRow(`SELECT prompt_text FROM session_prompts WHERE id = 'p1'`).Scan(&prompt)
	if !strings.Contains(prompt, "SECRET_PROMPT") {
		t.Errorf("local prompt was altered by redaction")
	}

	// And the rows are marked synced, so a second flush is a no-op.
	left, err := Pending(db)
	if err != nil || left != 0 {
		t.Fatalf("pending after flush = %d (err %v)", left, err)
	}
	res, err = Flush(context.Background(), db, cloudCfg(srv.URL, config.SyncModeMetadataOnly))
	if err != nil || len(srv.bodies) != 1 || len(res.Synced) != 0 {
		t.Errorf("second flush should send nothing: res=%+v posts=%d err=%v", res, len(srv.bodies), err)
	}
}

func TestPreview_DoesNotSendOrMarkSynced(t *testing.T) {
	db := openTestStore(t)
	seedContent(t, db)
	srv := newCaptureServer(t)

	cfg := cloudCfg(srv.URL, config.SyncModeMetadataOnly)
	cfg.Enabled = false // preview must work before sync is switched on
	payload, err := Preview(db, cfg)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(payload)
	for _, s := range seededSecrets {
		if strings.Contains(string(b), s) {
			t.Errorf("preview leaked %q", s)
		}
	}
	if len(srv.bodies) != 0 {
		t.Errorf("preview must not POST")
	}
	if left, _ := Pending(db); left == 0 {
		t.Errorf("preview must not mark rows synced")
	}

	// Full-mode preview of the same rows is the verbatim replica.
	cfg.SyncMode = config.SyncModeFull
	payload, err = Preview(db, cfg)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(payload)
	if !strings.Contains(string(b), "SECRET_PROMPT") {
		t.Errorf("full preview should carry content")
	}
}
