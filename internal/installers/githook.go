package installers

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bilanc/posthook/internal/atomicfs"
	"github.com/bilanc/posthook/internal/gitx"
	"github.com/bilanc/posthook/internal/notes"
	"github.com/bilanc/posthook/internal/paths"
)

const hookMarker = "# posthook v3"

func hookScript(binaryPath string) string {
	// Fallback mode (no git shadow) can only observe commits, so the hook
	// configures the notes fetch refspec here: an ordinary `git fetch` then
	// brings teammates' notes down into the tracking ref, and `posthook
	// blame` reads that ref directly. Pushing notes in this mode is manual
	// (`posthook notes push`) — the shadow does it automatically.
	return "#!/bin/sh\n" +
		hookMarker + "\n" +
		"# Captures commit metadata after every successful commit. Safe to fail silently.\n" +
		`"` + binaryPath + `" ingest --kind git-commit --repo-root "$(git rev-parse --show-toplevel)" --sha "$(git rev-parse HEAD)" >/dev/null 2>&1 || true` + "\n" +
		`"` + binaryPath + `" notes configure "$(git rev-parse --show-toplevel)" >/dev/null 2>&1 || true` + "\n"
}

// ConfigureNotesTransport installs posthook's notes fetch refspec on origin
// and removes the legacy fetch/push refspecs. Returns true iff config was
// modified. See package notes for why fetch goes through a tracking ref.
func ConfigureNotesTransport(repoPath string) (bool, error) {
	return notes.EnsureRefspec(repoPath, "origin"), nil
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
