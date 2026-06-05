// Package config loads and saves the posthook user-level config from
// ~/.posthook/config.json. It carries cloud-sync settings — endpoint, install
// token, enabled flag, flush interval — and the engineer identity stamped on
// every session.
//
// Env vars override the on-disk file so devs can point a single binary at a
// staging endpoint without rewriting their config: POSTHOOK_CLOUD_ENDPOINT,
// POSTHOOK_CLOUD_TOKEN, POSTHOOK_CLOUD_ENABLED, POSTHOOK_CLOUD_FLUSH_SECS,
// POSTHOOK_ENGINEER_EMAIL, POSTHOOK_ENGINEER_NAME.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/bilanc/posthook/internal/paths"
)

// Flush every few seconds: each flush is a cheap local SELECT of rows changed
// since the last flush, and a POST only when there's something new. The server
// appends one blob per non-empty flush, so a short interval just means
// near-real-time landing, not more load when idle.
const defaultFlushSeconds = 5

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
// command appends /posthook/ingest. Token is the team install token.
type CloudConfig struct {
	Endpoint            string `json:"endpoint,omitempty"`
	Token               string `json:"token,omitempty"`
	Enabled             bool   `json:"enabled"`
	FlushIntervalSecs   int    `json:"flush_interval_seconds,omitempty"`
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
