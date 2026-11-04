// Package config loads and saves the posthook user-level config from
// ~/.posthook/config.json. It carries cloud-sync settings — endpoint, install
// token, enabled flag, flush interval — and the engineer identity stamped on
// every session.
//
// Env vars override the on-disk file so devs can point a single binary at a
// staging endpoint without rewriting their config: POSTHOOK_CLOUD_ENDPOINT,
// POSTHOOK_CLOUD_TOKEN, POSTHOOK_CLOUD_ENABLED, POSTHOOK_CLOUD_FLUSH_SECS,
// POSTHOOK_CLOUD_SYNC_MODE, POSTHOOK_ENGINEER_EMAIL, POSTHOOK_ENGINEER_NAME.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/bilanc/posthook/internal/paths"
)

// Flush every few seconds: each flush is a cheap local SELECT of rows changed
// since the last flush, and a POST only when there's something new. The server
// appends one blob per non-empty flush, so a short interval just means
// near-real-time landing, not more load when idle.
const defaultFlushSeconds = 5

// Sync modes for CloudConfig.SyncMode — what a cloud flush is allowed to carry.
//
//   - SyncModeFull replicates the local tables as-is, including prompt text and
//     verbatim tool-call payloads (code, shell output, assistant replies).
//   - SyncModeMetadataOnly ships requests, sessions, tokens, turns, file paths
//     and line counts only. Prompt text, code and tool output never leave the
//     machine; see internal/sync/redact.go for the exact allowlist.
//
// An unrecognised value is a load error, not a silent fallback to full: a
// typo in a privacy setting must stop the flush rather than widen it.
const (
	SyncModeFull         = "full"
	SyncModeMetadataOnly = "metadata_only"
)

// NormalizeSyncMode maps user spellings onto the canonical constants. Empty
// means "unset" and resolves to full, the pre-sync_mode behaviour.
func NormalizeSyncMode(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", SyncModeFull:
		return SyncModeFull, nil
	case SyncModeMetadataOnly, "metadata-only", "metadata":
		return SyncModeMetadataOnly, nil
	}
	return "", fmt.Errorf("unknown sync mode %q (want %s or %s)", s, SyncModeFull, SyncModeMetadataOnly)
}

// Config is the on-disk shape of ~/.posthook/config.json. Add fields as the
// product grows; unknown JSON fields are ignored on load.
type Config struct {
	Cloud    CloudConfig    `json:"cloud"`
	Engineer EngineerConfig `json:"engineer"`
}

// EngineerConfig identifies the human behind this machine's sessions. Email is
// the canonical identity key — the cloud joins it against PR/commit data — so
// it should be the work email, confirmed by the person via `posthook identity
// --setup` rather than inferred. It takes precedence over per-repo git config
// (which is often unset, or a personal address on side repos). GitHubLogin is
// captured best-effort from the gh CLI as a secondary join key.
type EngineerConfig struct {
	Email       string `json:"email,omitempty"`
	Name        string `json:"name,omitempty"`
	GitHubLogin string `json:"github_login,omitempty"`
}

// CloudConfig holds everything the sync command needs to flush data upstream.
// Endpoint is the FastAPI base URL (e.g. https://api.bilanc.co); the sync
// command appends /posthook/ingest. Token is the team install token. SyncMode
// is one of the SyncMode* constants (Load always fills it in and rejects
// anything else).
type CloudConfig struct {
	Endpoint          string `json:"endpoint,omitempty"`
	Token             string `json:"token,omitempty"`
	Enabled           bool   `json:"enabled"`
	FlushIntervalSecs int    `json:"flush_interval_seconds,omitempty"`
	SyncMode          string `json:"sync_mode,omitempty"`
}

// MetadataOnly reports whether flushes must strip prompt text and tool-call
// content before leaving the machine. An unrecognised sync_mode reads as
// metadata-only too — never as the wider mode — and the flush path refuses
// to run on it at all (ResolveSyncMode).
func (c CloudConfig) MetadataOnly() bool {
	mode, err := NormalizeSyncMode(c.SyncMode)
	return err != nil || mode == SyncModeMetadataOnly
}

// ResolveSyncMode returns the canonical sync mode, or an error naming the
// unrecognised value so the caller can fail closed and say how to fix it.
func (c CloudConfig) ResolveSyncMode() (string, error) { return NormalizeSyncMode(c.SyncMode) }

// ReadDisk reads ~/.posthook/config.json exactly as written — no env
// overrides, no normalisation — so a caller can change one field and save it
// back without baking an env-only setting into the file. A missing file is
// an empty Config.
func ReadDisk() (Config, error) {
	var c Config
	b, err := os.ReadFile(Path())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return c, nil
		}
		return c, err
	}
	return c, json.Unmarshal(b, &c)
}

// SetSyncMode writes cloud.sync_mode to disk and leaves every other setting
// as it is. Returns false when the file already held that mode. The sync
// command calls it when the team server reports a metadata-only org policy,
// so an already-installed machine tightens itself on its next flush instead
// of waiting for someone to re-run the installer.
func SetSyncMode(mode string) (changed bool, err error) {
	mode, err = NormalizeSyncMode(mode)
	if err != nil {
		return false, err
	}
	disk, err := ReadDisk()
	if err != nil {
		return false, err
	}
	if current, cerr := NormalizeSyncMode(disk.Cloud.SyncMode); cerr == nil && current == mode {
		return false, nil
	}
	disk.Cloud.SyncMode = mode
	return true, Save(disk)
}

// Path returns the absolute path to the on-disk config file.
func Path() string { return filepath.Join(paths.PosthookDir(), "config.json") }

// Load reads the config, applies env overrides, and returns the merged view.
// A missing file is not an error — it returns a zero-value Config.
func Load() (Config, error) {
	var c Config
	b, err := os.ReadFile(Path())
	if err == nil {
		if jerr := json.Unmarshal(b, &c); jerr != nil {
			return c, fmt.Errorf("parse %s: %w", Path(), jerr)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return c, fmt.Errorf("read %s: %w", Path(), err)
	}

	if v := os.Getenv("POSTHOOK_CLOUD_ENDPOINT"); v != "" {
		c.Cloud.Endpoint = v
	}
	if v := os.Getenv("POSTHOOK_CLOUD_TOKEN"); v != "" {
		c.Cloud.Token = v
	}
	if v := os.Getenv("POSTHOOK_CLOUD_ENABLED"); v != "" {
		c.Cloud.Enabled = v == "1" || v == "true" || v == "TRUE"
	}
	if v := os.Getenv("POSTHOOK_CLOUD_FLUSH_SECS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.Cloud.FlushIntervalSecs = n
		}
	}
	if v := os.Getenv("POSTHOOK_CLOUD_SYNC_MODE"); v != "" {
		c.Cloud.SyncMode = v
	}
	// An unknown sync_mode is kept verbatim rather than failing the load: the
	// flush path refuses to run on it (fail closed, see ResolveSyncMode) while
	// `posthook sync --set-sync-mode` can still repair the typo and `posthook
	// uninstall` still sees cloud sync as configured, so its final flush is
	// attempted (and, refused, preserves the local data) instead of skipped.
	if mode, err := NormalizeSyncMode(c.Cloud.SyncMode); err == nil {
		c.Cloud.SyncMode = mode
	}
	if v := os.Getenv("POSTHOOK_ENGINEER_EMAIL"); v != "" {
		c.Engineer.Email = v
	}
	if v := os.Getenv("POSTHOOK_ENGINEER_NAME"); v != "" {
		c.Engineer.Name = v
	}
	if c.Cloud.FlushIntervalSecs <= 0 {
		c.Cloud.FlushIntervalSecs = defaultFlushSeconds
	}
	return c, nil
}

// Save writes the config atomically (tmp+rename) so a crash mid-write can't
// leave a half-written JSON blob behind.
func Save(c Config) error {
	if err := os.MkdirAll(paths.PosthookDir(), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := Path() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, Path())
}
