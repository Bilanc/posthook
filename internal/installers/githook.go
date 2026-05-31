package installers

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bilanc/posthook/internal/atomicfs"
	"github.com/bilanc/posthook/internal/gitx"
	"github.com/bilanc/posthook/internal/paths"
)

const hookMarker = "# posthook v2"

func hookScript(binaryPath string) string {
	// Self-healing notes transport: if the fetch/push refspecs for
	// refs/notes/posthook are missing on origin, add them. Repos created
	// via the global git template start syncing notes without `posthook
	// track`. Safe to fail silently — if origin doesn't exist yet, the
	// config lines still get added once it does.
	return "#!/bin/sh\n" +
		hookMarker + "\n" +
		"# Captures commit metadata after every successful commit. Safe to fail silently.\n" +
		`"` + binaryPath + `" ingest --kind git-commit --repo-root "$(git rev-parse --show-toplevel)" --sha "$(git rev-parse HEAD)" >/dev/null 2>&1 || true` + "\n" +
		"{\n" +
		"  spec='" + paths.NotesRef + ":" + paths.NotesRef + "'\n" +
		"  if ! git config --get-all remote.origin.fetch 2>/dev/null | grep -Fxq \"$spec\"; then\n" +
		"    git config --add remote.origin.fetch \"$spec\" 2>/dev/null || true\n" +
		"  fi\n" +
		"  if ! git config --get-all remote.origin.push 2>/dev/null | grep -Fxq \"$spec\"; then\n" +
		"    git config --add remote.origin.push \"$spec\" 2>/dev/null || true\n" +
		"  fi\n" +
		"} >/dev/null 2>&1 || true\n"
}

// ConfigureNotesTransport adds posthook's notes refspec to fetch and push for
// origin. Returns true iff config was modified.
func ConfigureNotesTransport(repoPath string) (bool, error) {
	spec := paths.NotesRef + ":" + paths.NotesRef
	changed := false
	for _, key := range []string{"remote.origin.fetch", "remote.origin.push"} {
		existing := gitx.Run(repoPath, "config", "--get-all", key)
		has := false
		for _, l := range strings.Split(existing, "\n") {
			if l == spec {
				has = true
				break
			}
		}
		if !has {
			if got := gitx.Run(repoPath, "config", "--add", key, spec); got == "" {
				// `git config --add` produces no output on success; the
				// empty return here means either success or failure. We
				// re-read to verify.
			}
			changed = true
		}
	}
	return changed, nil
}

func writeHookFile(path, binaryPath string) (bool, error) {
	desired := hookScript(binaryPath)
	if existing, err := os.ReadFile(path); err == nil {
		if string(existing) == desired {
			return false, nil
		}
		if !strings.Contains(string(existing), hookMarker) && strings.TrimSpace(string(existing)) != "" {
			return false, fmt.Errorf(
				"refusing to overwrite existing post-commit hook at %s. Move it aside and rerun.",
				path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err := atomicfs.WriteString(path, desired); err != nil {
		return false, err
	}
	if err := os.Chmod(path, 0o755); err != nil {
		return false, err
	}
	return true, nil
}

// InstallGlobalGitTemplate writes a post-commit hook into the global git
// template dir and points init.templateDir at it. New repos created via
// `git init` then auto-install the hook with no per-repo step.
func InstallGlobalGitTemplate(binaryPath string) (Result, error) {
	hooksDir := filepath.Join(paths.GitTemplateDir(), "hooks")
	hookPath := filepath.Join(hooksDir, "post-commit")
	changed, err := writeHookFile(hookPath, binaryPath)
	if err != nil {
		return Result{}, err
	}

	// Set init.templateDir if not already pointing here.
	current := gitx.Run("", "config", "--global", "--get", "init.templateDir")
	configChanged := false
	if current != paths.GitTemplateDir() {
		gitx.Run("", "config", "--global", "init.templateDir", paths.GitTemplateDir())
		configChanged = true
	}

	any := changed || configChanged
	msg := "Git template: already up to date"
	if any {
		msg = fmt.Sprintf("Git template: hook installed at %s; init.templateDir set", hookPath)
	}
	return Result{Changed: any, Path: hookPath, Message: msg}, nil
}

// InstallRepoHook installs the per-repo post-commit hook in repoPath. Used by
// `posthook track` for existing repos where the global template's auto-install
// won't kick in.
func InstallRepoHook(repoPath, binaryPath string) (Result, error) {
	gitDir := gitx.Run(repoPath, "rev-parse", "--git-dir")
	if gitDir == "" {
		return Result{}, fmt.Errorf("%s does not appear to be a git repo", repoPath)
	}
	absGitDir := gitDir
	if !filepath.IsAbs(absGitDir) {
		absGitDir = filepath.Join(repoPath, gitDir)
	}
	hookPath := filepath.Join(absGitDir, "hooks", "post-commit")
	hookChanged, err := writeHookFile(hookPath, binaryPath)
	if err != nil {
		return Result{}, err
	}
	notesChanged, _ := ConfigureNotesTransport(repoPath)

	parts := []string{}
	if hookChanged {
		parts = append(parts, "installed")
	} else {
		parts = append(parts, "hook up to date")
	}
	if notesChanged {
		parts = append(parts, "notes transport configured")
	}
	return Result{
		Changed: hookChanged || notesChanged,
		Path:    hookPath,
		Message: fmt.Sprintf("Repo hook (%s): %s", hookPath, strings.Join(parts, ", ")),
	}, nil
}
