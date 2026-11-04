package commands

import (
	"testing"

	"github.com/bilanc/posthook/internal/config"
	pksync "github.com/bilanc/posthook/internal/sync"
)

// The server may tighten a machine to metadata-only; it may never widen one.
func TestAdoptServerPolicy_TightensButNeverWidens(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("POSTHOOK_CLOUD_SYNC_MODE", "")
	cloud := config.CloudConfig{Endpoint: "https://api.example", Token: "tok", Enabled: true, SyncMode: config.SyncModeFull}
	if err := config.Save(config.Config{Cloud: cloud}); err != nil {
		t.Fatal(err)
	}

	adoptServerPolicy(&cloud, pksync.Result{ServerSyncMode: config.SyncModeFull})
	if cloud.MetadataOnly() {
		t.Fatal("a full reply must not change a full-mode client")
	}
	adoptServerPolicy(&cloud, pksync.Result{})
	if cloud.MetadataOnly() {
		t.Fatal("a reply with no policy must not change the client")
	}

	adoptServerPolicy(&cloud, pksync.Result{ServerSyncMode: config.SyncModeMetadataOnly})
	if !cloud.MetadataOnly() {
		t.Fatal("client did not tighten in memory")
	}
	disk, err := config.ReadDisk()
	if err != nil || disk.Cloud.SyncMode != config.SyncModeMetadataOnly || disk.Cloud.Token != "tok" || !disk.Cloud.Enabled {
		t.Fatalf("disk after tightening = %+v (err %v)", disk, err)
	}

	adoptServerPolicy(&cloud, pksync.Result{ServerSyncMode: config.SyncModeFull})
	disk, _ = config.ReadDisk()
	if !cloud.MetadataOnly() || disk.Cloud.SyncMode != config.SyncModeMetadataOnly {
		t.Fatal("a later full reply widened a metadata-only client")
	}
}
