// Package installers writes posthook's hook command into each AI agent's
// configuration file (Claude Code, Cursor, Codex). All installers follow the
// same idempotent merge-and-deduplicate pattern: read existing config,
// surgically add/update only posthook's command, write atomically. Existing
// user hooks are preserved.
package installers

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/bilanc/posthook/internal/atomicfs"
	"github.com/bilanc/posthook/internal/paths"
)

type Result struct {
	Changed bool
	Path    string
	Message string
}

// ReadJSONOrEmpty reads a JSON object from path, returning {} if the file
// doesn't exist or is empty. Errors on non-object JSON (we never write
// arrays or scalars at the top level).
func ReadJSONOrEmpty(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(data)) == "" {
		return map[string]any{}, nil
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("expected %s to contain a JSON object", path)
	}
	return m, nil
}

func WriteJSONAtomic(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return atomicfs.Write(path, append(data, '\n'), 0o644)
}

// IsPosthookCommand reports whether a hook command is one of ours (matches
// the posthook marker plus the agent slug, regardless of binary location).
func IsPosthookCommand(cmd, agentSlug string) bool {
	return strings.Contains(cmd, paths.PosthookMarker) && strings.Contains(cmd, agentSlug)
}

func PosthookCommandFor(binaryPath, agentSlug string) string {
	return fmt.Sprintf("%s ingest --agent %s", binaryPath, agentSlug)
}

// DetectBinaryPath returns POSTHOOK_BIN if set, else os.Executable().
// Honoring POSTHOOK_BIN matters for dev installs where the running process
// path might be `go run` or similar.
func DetectBinaryPath() string {
	if v := os.Getenv("POSTHOOK_BIN"); v != "" {
		return v
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return exe
}
