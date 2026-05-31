package proxy

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/bilanc/posthook/internal/paths"
)

// Hardcoded fallbacks if PATH lookup doesn't find anything usable.
// /usr/bin/git is the macOS default; /opt/homebrew is brew on Apple Silicon;
// /usr/local is brew on Intel + many Linux setups.
var fallbackPaths = []string{
	"/usr/bin/git",
	"/opt/homebrew/bin/git",
	"/usr/local/bin/git",
}

// LoadRealGitPath reads the cached real-git path from ~/.posthook/git-path.
// Returns "" if unset or the saved path no longer exists.
func LoadRealGitPath() string {
	data, err := os.ReadFile(paths.GitPathFile())
	if err != nil {
		return ""
	}
	p := strings.TrimSpace(string(data))
	if p == "" {
		return ""
	}
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// SaveRealGitPath persists the real-git path so the proxy can find it
// without re-scanning PATH on every invocation.
func SaveRealGitPath(path string) error {
	if err := os.MkdirAll(paths.PosthookDir(), 0o755); err != nil {
		return err
	}
	return os.WriteFile(paths.GitPathFile(), []byte(path+"\n"), 0o644)
}

// DetectRealGitPath locates the real git binary, skipping any candidate that
// resolves to our own posthook binary (the shadow symlink) or any other
// wrapper whose post-symlink-resolution basename isn't "git".
//
// posthookExecPath is the absolute path to the posthook binary. We compare
// EvalSymlinks(posthookExecPath) against each candidate so a symlink at
// /Users/.../git pointing back at posthook is detected and skipped.
func DetectRealGitPath(posthookExecPath string) string {
	ourReal := safeRealpath(posthookExecPath)

	candidates := whichAll("git")
	candidates = append(candidates, fallbackPaths...)

	seen := map[string]bool{}
	for _, c := range candidates {
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		if _, err := os.Stat(c); err != nil {
			continue
		}
		real := safeRealpath(c)
		if real == ourReal {
			continue
		}
		// A real git binary's basename after symlink resolution is "git".
		// Wrappers like git-ai resolve to ".../git-ai/bin/git-ai" — filter
		// those out and fall through to system paths.
		if filepath.Base(real) != "git" {
			continue
		}
		if !isExecutableFile(real) {
			continue
		}
		return real
	}
	return ""
}

// whichAll mirrors `which -a <name>` — returns every PATH match in order.
func whichAll(name string) []string {
	cmd := exec.Command("which", "-a", name)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var lines []string
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		l := strings.TrimSpace(scanner.Text())
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

func safeRealpath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

func isExecutableFile(p string) bool {
	st, err := os.Stat(p)
	if err != nil {
		return false
	}
	return st.Mode().IsRegular()
}

var (
	cachedReal    string
	cachedRealMu  sync.Mutex
	cachedRealSet bool
)

// ResolveRealGitPath returns the cached path, falling back to detection
// using the current executable's path if nothing is saved.
func ResolveRealGitPath() string {
	cachedRealMu.Lock()
	defer cachedRealMu.Unlock()
	if cachedRealSet {
		return cachedReal
	}
	if saved := LoadRealGitPath(); saved != "" {
		cachedReal = saved
		cachedRealSet = true
		return cachedReal
	}
	exe, err := os.Executable()
	if err != nil {
		cachedRealSet = true
		return ""
	}
	cachedReal = DetectRealGitPath(exe)
	cachedRealSet = true
	return cachedReal
}
