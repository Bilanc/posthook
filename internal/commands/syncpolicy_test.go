package commands

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/bilanc/posthook/internal/config"
	"github.com/bilanc/posthook/internal/store"
	pksync "github.com/bilanc/posthook/internal/sync"
)

var (
	testStoreOnce sync.Once
	testStore     *store.DB
	testStoreErr  error
)

// openTestStore opens this package's single store under a throwaway HOME.
// store.Open caches one handle per process, so it is opened once and emptied
// per test; config.json still follows each test's own t.Setenv("HOME"), which
// must come after this call so the store's HOME is what gets restored.
func openTestStore(t *testing.T) *store.DB {
	t.Helper()
	testStoreOnce.Do(func() {
		home, err := os.MkdirTemp("", "posthook-commands-test-")
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
	if _, err := testStore.Exec("DROP TRIGGER IF EXISTS block_mark_synced"); err != nil {
		t.Fatal(err)
	}
	return testStore
}

// seedPending adds one unsynced event (plus its repo and session on first use).
func seedPending(t *testing.T, db *store.DB, eventID string) {
	t.Helper()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT OR IGNORE INTO repositories (id, name, root_path) VALUES ('r1', 'repo', '/repo')`)
	exec(`INSERT OR IGNORE INTO sessions (id, agent_slug, model_slug, repo_id, started_at, engineer_email, input_tokens, output_tokens)
	      VALUES ('s1', 'claude-code', 'claude-fable-5-1', 'r1', '2026-11-06T09:00:00Z', 'eng@example.com', 10, 20)`)
	exec(`INSERT INTO events (id, session_id, ts, event_type, agent_slug, cwd, payload)
	      VALUES (?, 's1', '2026-11-06T09:01:00Z', 'PostToolUse', 'claude-code', '/repo',
	              '{"hook_event_name":"PostToolUse","session_id":"s1","tool_name":"Bash","tool_input":{"command":"SECRET"}}')`, eventID)
}

// policyServer is an ingest endpoint whose org policy is metadata-only unless
// replyMode says otherwise. It records the sync-mode header of every batch
// and can run a hook once a batch has arrived, before it replies.
type policyServer struct {
	*httptest.Server
	mu        sync.Mutex
	modes     []string
	replyMode string
	onRequest func()
}

func newPolicyServer(t *testing.T) *policyServer {
	t.Helper()
	ps := &policyServer{}
	ps.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		ps.mu.Lock()
		ps.modes = append(ps.modes, r.Header.Get("X-Posthook-Sync-Mode"))
		hook, reply := ps.onRequest, ps.replyMode
		ps.mu.Unlock()
		if hook != nil {
			hook()
		}
		if reply == "" {
			reply = config.SyncModeMetadataOnly
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"received":{},"sync_mode":"` + reply + `"}`))
	}))
	t.Cleanup(ps.Close)
	return ps
}

func (ps *policyServer) headers() []string {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return append([]string(nil), ps.modes...)
}

func (ps *policyServer) setReply(mode string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.replyMode = mode
}

func saveFullModeConfig(t *testing.T, endpoint string) {
	t.Helper()
	cloud := config.CloudConfig{Endpoint: endpoint, Token: "tok", Enabled: true, SyncMode: config.SyncModeFull}
	if err := config.Save(config.Config{Cloud: cloud}); err != nil {
		t.Fatal(err)
	}
}

// The server may tighten a machine to metadata-only; it may never widen one.
func TestAdoptServerPolicy_TightensButNeverWidens(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("POSTHOOK_CLOUD_SYNC_MODE", "")
	cloud := config.CloudConfig{Endpoint: "https://api.example", Token: "tok", Enabled: true, SyncMode: config.SyncModeFull}
	if err := config.Save(config.Config{Cloud: cloud}); err != nil {
		t.Fatal(err)
	}

	if adoptServerPolicy(&cloud, pksync.Result{ServerSyncMode: config.SyncModeFull}) || cloud.MetadataOnly() {
		t.Fatal("a full reply must not change a full-mode client")
	}
	if adoptServerPolicy(&cloud, pksync.Result{}) || cloud.MetadataOnly() {
		t.Fatal("a reply with no policy must not change the client")
	}

	if !adoptServerPolicy(&cloud, pksync.Result{ServerSyncMode: config.SyncModeMetadataOnly}) || !cloud.MetadataOnly() {
		t.Fatal("client did not tighten in memory")
	}
	disk, err := config.ReadDisk()
	if err != nil || disk.Cloud.SyncMode != config.SyncModeMetadataOnly || disk.Cloud.Token != "tok" || !disk.Cloud.Enabled {
		t.Fatalf("disk after tightening = %+v (err %v)", disk, err)
	}

	if adoptServerPolicy(&cloud, pksync.Result{ServerSyncMode: config.SyncModeFull}) {
		t.Fatal("a full reply must not report a metadata-only org")
	}
	disk, _ = config.ReadDisk()
	if !cloud.MetadataOnly() || disk.Cloud.SyncMode != config.SyncModeMetadataOnly {
		t.Fatal("a later full reply widened a metadata-only client")
	}
	if !adoptServerPolicy(&cloud, pksync.Result{ServerSyncMode: config.SyncModeMetadataOnly}) {
		t.Fatal("an already-tight client must still report the org policy")
	}
}

// Flush reads the org policy from the 2xx reply before it marks rows synced.
// If that marking fails the policy must still be adopted: those rows go out
// again on the next attempt and must not do so in full.
func TestFlushAndAdopt_AdoptsPolicyWhenMarkSyncedFails(t *testing.T) {
	db := openTestStore(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("POSTHOOK_CLOUD_SYNC_MODE", "")
	srv := newPolicyServer(t)
	// Once the batch is on the wire, make the post-2xx UPDATE fail.
	srv.onRequest = func() {
		if _, err := db.Exec(`CREATE TRIGGER block_mark_synced BEFORE UPDATE OF synced_at ON events
		                      BEGIN SELECT RAISE(ABORT, 'mark synced blocked'); END`); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { _, _ = db.Exec("DROP TRIGGER IF EXISTS block_mark_synced") })
	saveFullModeConfig(t, srv.URL)
	cloud := config.CloudConfig{Endpoint: srv.URL, Token: "tok", Enabled: true, SyncMode: config.SyncModeFull}
	seedPending(t, db, "e1")

	_, orgMetadataOnly, err := flushAndAdopt(context.Background(), db, &cloud)
	if err == nil || !strings.Contains(err.Error(), "mark synced") {
		t.Fatalf("err = %v, want a mark-synced failure", err)
	}
	if !orgMetadataOnly || !cloud.MetadataOnly() {
		t.Fatal("policy from the 2xx reply was dropped because marking failed")
	}
	if disk, _ := config.ReadDisk(); disk.Cloud.SyncMode != config.SyncModeMetadataOnly {
		t.Fatalf("disk sync_mode = %q, want metadata_only", disk.Cloud.SyncMode)
	}
	if left, _ := pksync.Pending(db); left == 0 {
		t.Fatal("rows must stay pending for the retry")
	}
}

// readOnlyConfigDir makes config.json unwritable until undo is called (or
// the test ends), so adopting a policy cannot persist it.
func readOnlyConfigDir(t *testing.T) (undo func()) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := filepath.Dir(config.Path())
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	undo = func() { _ = os.Chmod(dir, 0o755) }
	t.Cleanup(undo)
	return undo
}

// editIdentity rewrites config.json the way `posthook identity --set-email`
// does: every field preserved, cloud.sync_mode included.
func editIdentity(t *testing.T) {
	t.Helper()
	disk, err := config.ReadDisk()
	if err != nil {
		t.Fatal(err)
	}
	disk.Engineer.Email = "someone@example.com"
	if err := config.Save(disk); err != nil {
		t.Fatal(err)
	}
}

func assertHeaders(t *testing.T, srv *policyServer, want ...string) {
	t.Helper()
	if got := srv.headers(); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("sync-mode headers = %v, want %v", got, want)
	}
}

// The daemon re-reads config.json every tick. A metadata-only org policy must
// survive that reload when it could not be written to disk, and an env
// override must not widen a policy that was written.
func TestSyncLoopTick_PinsOrgPolicyAcrossConfigReloads(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T)
	}{
		{"config.json cannot be written", func(t *testing.T) { readOnlyConfigDir(t) }},
		{"POSTHOOK_CLOUD_SYNC_MODE=full cannot widen the written policy", func(t *testing.T) {
			t.Setenv("POSTHOOK_CLOUD_SYNC_MODE", "full")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestStore(t)
			t.Setenv("HOME", t.TempDir())
			t.Setenv("POSTHOOK_CLOUD_SYNC_MODE", "")
			srv := newPolicyServer(t)
			saveFullModeConfig(t, srv.URL)
			tc.setup(t)

			loop := &syncLoopState{db: db}
			seedPending(t, db, "e1")
			loop.tick()
			seedPending(t, db, "e2")
			loop.tick()

			assertHeaders(t, srv, "full", "metadata_only")
			if !loop.orgMetadataOnly {
				t.Fatal("loop forgot the org policy")
			}
		})
	}
}

// A remembered policy yields to a change of cloud.sync_mode on disk: after an
// org goes back to full, `posthook sync --set-sync-mode full` on the machine
// is the documented way to widen it, and it must take effect on the running
// daemon's next flush. If the org is in fact still metadata-only, the server
// says so again on that flush and the machine is re-tightened.
func TestSyncLoopTick_ExplicitSetFullLiftsPin(t *testing.T) {
	for _, tc := range []struct {
		name     string
		orgReply string // what the server says once the machine is widened
		want     []string
	}{
		{"org back to full", config.SyncModeFull, []string{"full", "metadata_only", "full", "full"}},
		{"org still metadata-only", config.SyncModeMetadataOnly, []string{"full", "metadata_only", "full", "metadata_only"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestStore(t)
			t.Setenv("HOME", t.TempDir())
			t.Setenv("POSTHOOK_CLOUD_SYNC_MODE", "")
			srv := newPolicyServer(t)
			saveFullModeConfig(t, srv.URL)

			loop := &syncLoopState{db: db}
			seedPending(t, db, "e1")
			loop.tick()
			seedPending(t, db, "e2")
			loop.tick()

			srv.setReply(tc.orgReply)
			if err := runSyncConfigure("", "", "", config.SyncModeFull); err != nil {
				t.Fatal(err)
			}
			seedPending(t, db, "e3")
			loop.tick()
			seedPending(t, db, "e4")
			loop.tick()

			assertHeaders(t, srv, tc.want...)
			if loop.orgMetadataOnly != (tc.orgReply == config.SyncModeMetadataOnly) {
				t.Fatalf("orgMetadataOnly = %v after org reply %q", loop.orgMetadataOnly, tc.orgReply)
			}
		})
	}
}

// An edit that leaves cloud.sync_mode alone — identity, token, retention —
// must not lift the remembered policy, whether the policy was written to
// disk (and an env override says full) or could not be written.
func TestSyncLoopTick_UnrelatedConfigEditKeepsPin(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T) (undo func())
	}{
		{"policy written, env override says full", func(t *testing.T) func() {
			t.Setenv("POSTHOOK_CLOUD_SYNC_MODE", "full")
			return func() {}
		}},
		{"policy could not be written", readOnlyConfigDir},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestStore(t)
			t.Setenv("HOME", t.TempDir())
			t.Setenv("POSTHOOK_CLOUD_SYNC_MODE", "")
			srv := newPolicyServer(t)
			saveFullModeConfig(t, srv.URL)
			undo := tc.setup(t)

			loop := &syncLoopState{db: db}
			seedPending(t, db, "e1")
			loop.tick()
			seedPending(t, db, "e2")
			loop.tick()

			undo()
			editIdentity(t)
			seedPending(t, db, "e3")
			loop.tick()

			assertHeaders(t, srv, "full", "metadata_only", "metadata_only")
			if !loop.orgMetadataOnly {
				t.Fatal("an unrelated edit lifted the org policy")
			}
		})
	}
}

// When the policy could not be written when it arrived, the daemon keeps
// trying; once the file is writable again the disk converges to
// metadata_only, and an explicit `--set-sync-mode full` after that is seen
// as the change it is.
func TestSyncLoopTick_RetriesWritingPolicyThenHonoursExplicitFull(t *testing.T) {
	db := openTestStore(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("POSTHOOK_CLOUD_SYNC_MODE", "")
	srv := newPolicyServer(t)
	saveFullModeConfig(t, srv.URL)
	undo := readOnlyConfigDir(t)

	loop := &syncLoopState{db: db}
	seedPending(t, db, "e1")
	loop.tick()
	if disk, _ := config.ReadDisk(); disk.Cloud.SyncMode != config.SyncModeFull {
		t.Fatalf("precondition: the write should have failed, disk = %q", disk.Cloud.SyncMode)
	}
	seedPending(t, db, "e2")
	loop.tick()

	undo()
	seedPending(t, db, "e3")
	loop.tick()
	if disk, _ := config.ReadDisk(); disk.Cloud.SyncMode != config.SyncModeMetadataOnly {
		t.Fatalf("disk did not converge once writable, sync_mode = %q", disk.Cloud.SyncMode)
	}

	srv.setReply(config.SyncModeFull)
	if err := runSyncConfigure("", "", "", config.SyncModeFull); err != nil {
		t.Fatal(err)
	}
	seedPending(t, db, "e4")
	loop.tick()

	assertHeaders(t, srv, "full", "metadata_only", "metadata_only", "full")
	if loop.orgMetadataOnly {
		t.Fatal("explicit --set-sync-mode full did not lift the policy")
	}
}
