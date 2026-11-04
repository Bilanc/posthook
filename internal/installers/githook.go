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

// hookMarkerPrefix matches the header line every version of the hook has
// carried ("# posthook v1" … "# posthook v3"), so hooks left by older
// releases are recognised as ours too — for overwriting on upgrade and for
// removal on uninstall.
const hookMarkerPrefix = "# posthook v"

// isPosthookHook reports whether a post-commit hook's contents were written
// by posthook (any version).
func isPosthookHook(content string) bool {
	return strings.Contains(content, hookMarkerPrefix)
}

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
		if !isPosthookHook(string(existing)) && strings.TrimSpace(string(existing)) != "" {
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

// GlobalGitTemplateActive reports whether init.templateDir currently points
// at posthook's template dir.
func GlobalGitTemplateActive() bool {
	return gitx.Run("", "config", "--global", "--get", "init.templateDir") == paths.GitTemplateDir()
}

// UninstallGlobalGitTemplate unsets init.templateDir when it points at
// posthook's template dir, so `git init` stops copying our hook into new
// repos. A templateDir pointing anywhere else is the user's and is left
// alone. The template dir itself is removed along with ~/.posthook.
func UninstallGlobalGitTemplate() (Result, error) {
	current := gitx.Run("", "config", "--global", "--get", "init.templateDir")
	switch {
	case current == "":
		return Result{Message: "Git template: init.templateDir not set"}, nil
	case current != paths.GitTemplateDir():
		return Result{Message: fmt.Sprintf("Git template: init.templateDir points at %s, not posthook's — left as is", current)}, nil
	}
	if err := gitx.Exec("", "config", "--global", "--unset", "init.templateDir"); err != nil {
		return Result{}, err
	}
	return Result{Changed: true, Message: "Git template: init.templateDir unset"}, nil
}

// RemoveRepoHook deletes the per-repo post-commit hook from repoPath when it
// is one posthook wrote (any version). A hook posthook didn't write, or a
// path that isn't a git repo, is left untouched. Both the repo's own git dir
// and the common dir are checked, so a worktree is cleaned too. Returns true
// iff a hook was removed.
func RemoveRepoHook(repoPath string) (bool, error) {
	removed := false
	seen := map[string]bool{}
	for _, flag := range []string{"--git-dir", "--git-common-dir"} {
		gitDir := gitx.Run(repoPath, "rev-parse", flag)
		if gitDir == "" {
			continue
		}
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(repoPath, gitDir)
		}
		hookPath := filepath.Join(gitDir, "hooks", "post-commit")
		if seen[hookPath] {
			continue
		}
		seen[hookPath] = true
		existing, err := os.ReadFile(hookPath)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return removed, err
		}
		if !isPosthookHook(string(existing)) {
			continue
		}
		if err := os.Remove(hookPath); err != nil {
			return removed, err
		}
		removed = true
	}
	return removed, nil
}
