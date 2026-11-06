package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, body string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("POSTHOOK_CLOUD_SYNC_MODE", "")
	if err := os.MkdirAll(filepath.Join(home, ".posthook"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A typo in sync_mode must not make Load fail: the flush path refuses to run
// on it, but the repair command and uninstall's final flush still need the
// rest of the config.
func TestLoad_KeepsUnknownSyncModeAndReadsAsNotFull(t *testing.T) {
	writeConfig(t, `{"cloud":{"enabled":true,"endpoint":"https://api.example","token":"tok","sync_mode":"metdata_only"}}`)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Cloud.SyncMode != "metdata_only" || !c.Cloud.Enabled || c.Cloud.Token != "tok" {
		t.Errorf("unknown mode not kept verbatim: %+v", c.Cloud)
	}
	if !c.Cloud.MetadataOnly() {
		t.Errorf("an unknown mode must never read as full")
	}
	if _, err := c.Cloud.ResolveSyncMode(); err == nil {
		t.Errorf("ResolveSyncMode must reject an unknown mode")
	}
}

func TestLoad_NormalisesSyncMode(t *testing.T) {
	for raw, want := range map[string]string{"": SyncModeFull, "Metadata-Only": SyncModeMetadataOnly, "FULL": SyncModeFull} {
		writeConfig(t, `{"cloud":{"sync_mode":"`+raw+`"}}`)
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.Cloud.SyncMode != want {
			t.Errorf("sync_mode %q loaded as %q, want %q", raw, c.Cloud.SyncMode, want)
		}
	}
}

func TestSetSyncMode_PreservesEverythingElse(t *testing.T) {
	writeConfig(t, `{"cloud":{"enabled":true,"endpoint":"https://api.example","token":"tok","flush_interval_seconds":7},"engineer":{"email":"eng@example.com"}}`)

	changed, err := SetSyncMode(SyncModeMetadataOnly)
	if err != nil || !changed {
		t.Fatalf("SetSyncMode = %v, %v; want changed", changed, err)
	}
	disk, err := ReadDisk()
	if err != nil {
		t.Fatal(err)
	}
	if disk.Cloud.SyncMode != SyncModeMetadataOnly || disk.Cloud.Token != "tok" || disk.Cloud.Endpoint != "https://api.example" ||
		!disk.Cloud.Enabled || disk.Cloud.FlushIntervalSecs != 7 || disk.Engineer.Email != "eng@example.com" {
		t.Errorf("SetSyncMode clobbered other settings: %+v", disk)
	}

	if changed, err := SetSyncMode("metadata-only"); err != nil || changed {
		t.Errorf("second SetSyncMode = %v, %v; want no change", changed, err)
	}
	if _, err := SetSyncMode("bogus"); err == nil {
		t.Errorf("SetSyncMode must reject an unknown mode")
	}
	if disk, _ := ReadDisk(); disk.Cloud.SyncMode != SyncModeMetadataOnly {
		t.Errorf("rejected write altered the file: %+v", disk)
	}
}
