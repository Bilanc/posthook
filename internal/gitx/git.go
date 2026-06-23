package gitx

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// FindRepoRoot walks up from start looking for a .git entry. Returns the
// canonicalized repo root or empty string if none found. Handles both .git
// directories and .git files (worktrees/submodules). Canonicalizing via
// filepath.EvalSymlinks ensures /var/folders/... and /private/var/... on macOS
// resolve to the same physical identity — otherwise the same repo gets
// registered under two keys and ingest/blame fail to join.
func FindRepoRoot(start string) string {
	cur := start
	for i := 0; i < 64; i++ {
		if _, err := os.Stat(filepath.Join(cur, ".git")); err == nil {
			if real, err := filepath.EvalSymlinks(cur); err == nil {
				return real
			}
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return ""
		}
		cur = parent
	}
	return ""
}

// Canonicalize resolves a path through symlinks. Returns the input on failure
// so callers can still compare strings.
func Canonicalize(path string) string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	return path
}

// RelPathInRepo returns abs relative to repoRoot, or empty string if abs is
// outside the repo.
func RelPathInRepo(repoRoot, abs string) string {
	r, err := filepath.Rel(repoRoot, abs)
	if err != nil {
		return ""
	}
	if r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return ""
	}
	return r
}

// BypassEnv returns the current environment with POSTHOOK_BYPASS=1 added.
// Used for any exec.Cmd that runs `git` so our shadow proxy (if installed)
// passes straight through — otherwise our ingest path would recurse through
// the proxy every time it queries git.
func BypassEnv() []string {
	env := os.Environ()
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		if !strings.HasPrefix(e, "POSTHOOK_BYPASS=") {
			out = append(out, e)
		}
	}
	out = append(out, "POSTHOOK_BYPASS=1")
	return out
}

// Run runs `git <args>` in cwd with POSTHOOK_BYPASS=1 and returns trimmed
// stdout or empty string on failure.
func Run(cwd string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	cmd.Env = BypassEnv()
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Output runs `git <args>` in cwd with POSTHOOK_BYPASS=1 and returns the raw,
// untrimmed stdout. Unlike Run, it preserves exact bytes (trailing newlines,
// leading whitespace) — required when parsing diff hunks or hashing blob
// content, where trimming would change line counts and content hashes.
func Output(cwd string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	cmd.Env = BypassEnv()
	return cmd.Output()
}
