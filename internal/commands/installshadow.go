package commands

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/bilanc/posthook/internal/logx"
	"github.com/bilanc/posthook/internal/proxy"

	"github.com/spf13/cobra"
)

func newInstallShadowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install-shadow",
		Short: "Install the git shadow (idempotent)",
		Long:  "Install posthook as a `git` shadow: a symlink alongside the posthook binary named `git`, so any `git` command on PATH hits us first.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInstallShadow()
		},
	}
}

func newUninstallShadowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall-shadow",
		Short: "Remove the git shadow",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUninstallShadow()
		},
	}
}

// ShadowHealth describes the state of the git shadow installation.
type ShadowHealth struct {
	Attempted     bool
	ShadowPath    string
	SymlinkExists bool
	SymlinkValid  bool
	WhichGit      string
	Winning       bool
	SavedRealGit  string
}

func CheckShadowHealth() ShadowHealth {
	posthookPath := resolvePosthookBinaryPath()
	var shadowPath string
	if posthookPath != "" {
		shadowPath = filepath.Join(filepath.Dir(posthookPath), "git")
	}

	exists := false
	valid := false
	if shadowPath != "" {
		if st, err := os.Lstat(shadowPath); err == nil {
			exists = true
			if st.Mode()&os.ModeSymlink != 0 {
				target, _ := os.Readlink(shadowPath)
				if posthookPath != "" && (target == posthookPath || sameFile(target, posthookPath)) {
					valid = true
				}
			}
		}
	}

	whichGit := resolveWhichGit()
	winning := shadowPath != "" && whichGit != "" && sameFile(whichGit, shadowPath)
	saved := proxy.LoadRealGitPath()

	return ShadowHealth{
		Attempted:     exists || saved != "",
		ShadowPath:    shadowPath,
		SymlinkExists: exists,
		SymlinkValid:  valid,
		WhichGit:      whichGit,
		Winning:       winning,
		SavedRealGit:  saved,
	}
}

func runInstallShadow() error {
	posthookPath := resolvePosthookBinaryPath()
	if posthookPath == "" {
		return errors.New("cannot determine posthook binary path. Set POSTHOOK_BIN to an absolute path and retry.")
	}
	installDir := filepath.Dir(posthookPath)
	shadowPath := filepath.Join(installDir, "git")

	// Detect real git BEFORE we create the symlink — otherwise `which -a git`
	// would include our shadow and we'd have to filter it out anyway.
	realGit := proxy.DetectRealGitPath(posthookPath)
	if realGit == "" {
		return errors.New("no real git binary found on PATH or in fallback locations. Install git first (e.g. via Xcode CLT or Homebrew), then re-run.")
	}

	created, err := ensureSymlink(shadowPath, posthookPath)
	if err != nil {
		return err
	}
	if err := proxy.SaveRealGitPath(realGit); err != nil {
		return err
	}

	whichGit := resolveWhichGit()
	winning := whichGit != "" && sameFile(whichGit, shadowPath)

	if created {
		logx.Infof("Shadow installed: %s → %s", shadowPath, posthookPath)
	} else {
		logx.Infof("Shadow already in place: %s", shadowPath)
	}
	logx.Infof("Real git saved:    %s", realGit)
	switch {
	case winning:
		logx.Infof("PATH check:        `which git` → %s ✓", shadowPath)
	case whichGit != "":
		logx.Warnf(
			"PATH check: `which git` → %s, not our shadow at %s.\n  Add this to your shell rc so the shadow wins:\n    export PATH=\"%s:$PATH\"\n  Then open a new shell and re-run `posthook install-shadow` to verify.",
			whichGit, shadowPath, installDir)
	default:
		logx.Warnf("PATH check: `which git` returned nothing. Ensure %s is on PATH.", installDir)
	}
	return nil
}

func runUninstallShadow() error {
	posthookPath := resolvePosthookBinaryPath()
	if posthookPath == "" {
		return errors.New("cannot determine posthook binary path.")
	}
	installDir := filepath.Dir(posthookPath)
	shadowPath := filepath.Join(installDir, "git")

	st, err := os.Lstat(shadowPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			logx.Infof("No shadow found at %s.", shadowPath)
			return nil
		}
		return err
	}
	if st.Mode()&os.ModeSymlink == 0 {
		logx.Warnf("Refusing to remove %s: not a symlink. Inspect it manually before deleting.", shadowPath)
		return nil
	}
	target, _ := os.Readlink(shadowPath)
	if !sameFile(target, posthookPath) && target != posthookPath {
		logx.Warnf("Refusing to remove %s: symlink target is %s, not our posthook binary at %s. Inspect manually.",
			shadowPath, target, posthookPath)
		return nil
	}
	if err := os.Remove(shadowPath); err != nil {
		return err
	}
	logx.Infof("Shadow removed: %s", shadowPath)
	if proxy.LoadRealGitPath() != "" {
		logx.Info("Saved real-git path is preserved at ~/.posthook/git-path (safe to delete if no longer wanted).")
	}
	return nil
}

func resolvePosthookBinaryPath() string {
	if v := os.Getenv("POSTHOOK_BIN"); v != "" {
		if _, err := os.Stat(v); err == nil {
			return v
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	// Refuse to use a `go run` temp binary path that doesn't survive.
	return exe
}

func ensureSymlink(path, target string) (bool, error) {
	if st, err := os.Lstat(path); err == nil {
		if st.Mode()&os.ModeSymlink == 0 {
			return false, fmt.Errorf(
				"%s exists and is not a symlink. Move or delete it manually before installing the shadow.",
				path)
		}
		current, _ := os.Readlink(path)
		if current == target || sameFile(current, target) {
			return false, nil
		}
		if err := os.Remove(path); err != nil {
			return false, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	if err := os.Symlink(target, path); err != nil {
		return false, err
	}
	return true, nil
}

func sameFile(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		return false
	}
	return ra == rb
}

func resolveWhichGit() string {
	cmd := exec.Command("which", "git")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
