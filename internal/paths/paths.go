package paths

import (
	"os"
	"path/filepath"
	"runtime"
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

func PosthookDir() string        { return filepath.Join(Home(), ".posthook") }
func DBPath() string             { return filepath.Join(PosthookDir(), "posthook.db") }
func GitTemplateDir() string     { return filepath.Join(PosthookDir(), "git-template") }
func GitPathFile() string        { return filepath.Join(PosthookDir(), "git-path") }
func ClaudeSettingsPath() string { return filepath.Join(Home(), ".claude", "settings.json") }
func CursorHooksPath() string    { return filepath.Join(Home(), ".cursor", "hooks.json") }
func CodexConfigPath() string    { return filepath.Join(Home(), ".codex", "config.toml") }

// CopilotDir is GitHub Copilot's user directory, shared by Copilot CLI and
// VS Code's Copilot session target. COPILOT_HOME relocates it, and Copilot
// reads hooks from <dir>/hooks/*.json.
func CopilotDir() string {
	if v := os.Getenv("COPILOT_HOME"); v != "" {
		return v
	}
	return filepath.Join(Home(), ".copilot")
}

// CopilotHooksPath is posthook's own hook file. Copilot merges every
// *.json in the hooks directory, so posthook keeps its entries in a file it
// owns instead of editing one the user wrote.
func CopilotHooksPath() string { return filepath.Join(CopilotDir(), "hooks", "posthook.json") }

// CopilotSessionStateDir holds one directory per Copilot CLI session, each
// with an events.jsonl transcript (model, token usage, prompts, tool calls).
func CopilotSessionStateDir() string { return filepath.Join(CopilotDir(), "session-state") }

// VSCodeUserDirs lists the VS Code user-data directories that can exist on
// this platform (stable and Insiders). Copilot Chat writes its per-workspace
// transcripts under <dir>/workspaceStorage/<hash>/GitHub.copilot-chat/ and
// its global state under <dir>/globalStorage/github.copilot-chat/.
func VSCodeUserDirs() []string {
	var bases []string
	switch runtime.GOOS {
	case "darwin":
		bases = []string{filepath.Join(Home(), "Library", "Application Support")}
	case "windows":
		if v := os.Getenv("APPDATA"); v != "" {
			bases = []string{v}
		}
	default:
		if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
			bases = []string{v}
		} else {
			bases = []string{filepath.Join(Home(), ".config")}
		}
	}
	var out []string
	for _, b := range bases {
		for _, product := range []string{"Code", "Code - Insiders"} {
			out = append(out, filepath.Join(b, product, "User"))
		}
	}
	return out
}

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
