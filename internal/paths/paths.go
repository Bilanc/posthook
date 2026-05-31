package paths

import (
	"os"
	"path/filepath"
)

const (
	PosthookMarker = "posthook ingest"

	// NotesRef is the Git Notes ref used to carry line attribution data with
	// the repo. The post-commit hook self-configures push/fetch refspecs on
	// remote.origin so attribution travels with the code.
	NotesRef = "refs/notes/posthook"
)

func Home() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return h
}

func PosthookDir() string         { return filepath.Join(Home(), ".posthook") }
func DBPath() string              { return filepath.Join(PosthookDir(), "posthook.db") }
func GitTemplateDir() string      { return filepath.Join(PosthookDir(), "git-template") }
func GitPathFile() string         { return filepath.Join(PosthookDir(), "git-path") }
func ClaudeSettingsPath() string  { return filepath.Join(Home(), ".claude", "settings.json") }
func CursorHooksPath() string     { return filepath.Join(Home(), ".cursor", "hooks.json") }
func CodexConfigPath() string     { return filepath.Join(Home(), ".codex", "config.toml") }

// DashDir is where install.sh stages the built Next.js standalone dashboard
// (the contents of dash/.next/standalone) so `posthook dash` can spawn it
// without depending on where the source tree lives.
func DashDir() string { return filepath.Join(PosthookDir(), "dash") }

// DashServer is the standalone Next.js entrypoint inside DashDir.
func DashServer() string { return filepath.Join(DashDir(), "server.js") }

// DashPidFile records the PID of the background dashboard server so repeated
// `posthook dash` invocations reuse the running server instead of spawning
// duplicates.
func DashPidFile() string { return filepath.Join(PosthookDir(), "dash.pid") }
