// Package proxy implements the `git` shadow. When the posthook binary is
// invoked under the name "git" (via a symlink at ~/.local/bin/git), Run
// takes over: it spawns the real git as a child, forwards stdio and signals
// faithfully so the user sees identical behavior, and after success runs
// our capture logic for `commit` and `clone`.
//
// Critical invariants:
//   - Exit with the child's exit code (or 128+N on signal termination) so
//     scripts and IDE integrations see the same outcome as plain git.
//   - Capture logic runs AFTER git succeeds, never before. Pre-hooks could
//     block legitimate work on a bug.
//   - Any failure in capture MUST NOT affect the user-visible exit code.
//   - POSTHOOK_BYPASS=1 disables capture entirely — set by our own internal
//     git calls to prevent recursion.
package proxy

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/bilanc/posthook/internal/gitx"
	"github.com/bilanc/posthook/internal/ingest"
	"github.com/bilanc/posthook/internal/logx"
)

// Run is the proxy entrypoint. Never returns; calls os.Exit with the
// appropriate code.
func Run(args []string) {
	realGit := ResolveRealGitPath()
	if realGit == "" {
		fmt.Fprintln(os.Stderr,
			"posthook: cannot find real git binary. Run `posthook install-shadow` to re-detect, or `posthook uninstall-shadow` to remove the proxy.")
		os.Exit(127)
	}

	bypass := os.Getenv("POSTHOOK_BYPASS") == "1"
	subcommand, subcommandArgs := splitGitInvocation(args)
	interceptable := !bypass && (subcommand == "commit" || subcommand == "clone")

	code := spawnPassthrough(realGit, args)

	if interceptable && code == 0 {
		defer func() {
			// Never let capture failures affect git's exit code.
			if r := recover(); r != nil {
				logx.Warnf("proxy capture panicked for %s: %v", subcommand, r)
			}
		}()
		var err error
		switch subcommand {
		case "commit":
			err = handleCommit(realGit)
		case "clone":
			err = handleClone(realGit, subcommandArgs)
		}
		if err != nil {
			logx.Warnf("proxy capture failed for %s: %v", subcommand, err)
		}
	}
	os.Exit(code)
}

func splitGitInvocation(args []string) (string, []string) {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-C", "-c", "--git-dir", "--work-tree", "--namespace", "--super-prefix", "--config-env":
			// Skip the option (for example, `-c`) and its value (for example,
			// `user.useConfigOnly=true`) so `commit` is recognized as the Git command.
			i++
			continue
		}
		if strings.HasPrefix(args[i], "-") {
			// Other global options are flags or carry their value after `=`.
			continue
		}
		// We found the Git command (for example, `commit`). Return it and only
		// the arguments that come after it.
		return args[i], args[i+1:]
	}
	return "", nil
}

func spawnPassthrough(realGit string, args []string) int {
	cmd := exec.Command(realGit, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Inherit env. Children that themselves invoke git (submodule updates,
	// etc.) re-enter our shadow, which is what we want — they become
	// independently captured. We control recursion via POSTHOOK_BYPASS.
	cmd.Env = os.Environ()

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "posthook: failed to spawn git: %v\n", err)
		return 127
	}

	// Signal forwarding. The shell sends signals to the foreground process
	// group; when our proxy is foreground we receive them and must relay.
	sigChan := make(chan os.Signal, 4)
	forwarded := []os.Signal{
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT,
		syscall.SIGHUP,
	}
	signal.Notify(sigChan, forwarded...)
	defer signal.Stop(sigChan)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case sig := <-sigChan:
				if cmd.Process != nil {
					_ = cmd.Process.Signal(sig)
				}
			case <-done:
				return
			}
		}
	}()

	err := cmd.Wait()
	close(done)
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
			if ws.Signaled() {
				// Convention: terminated by signal N → exit 128+N.
				return 128 + int(ws.Signal())
			}
			return ws.ExitStatus()
		}
		return ee.ExitCode()
	}
	fmt.Fprintf(os.Stderr, "posthook: git wait failed: %v\n", err)
	return 127
}

func handleCommit(realGit string) error {
	// After `git commit` succeeds, capture the new HEAD commit. We're
	// already in the user's cwd, so rev-parse works without --cwd plumbing.
	repoRoot := runRealGit(realGit, "", "rev-parse", "--show-toplevel")
	if repoRoot == "" {
		return nil
	}
	sha := runRealGit(realGit, "", "rev-parse", "HEAD")
	if sha == "" {
		return nil
	}
	if err := ingest.GitCommit(gitx.Canonicalize(repoRoot), sha); err != nil {
		return err
	}
	if len(sha) >= 7 {
		logx.Debugf("proxy: captured commit %s", sha[:7])
	}
	return nil
}

func handleClone(realGit string, args []string) error {
	dest := inferCloneDest(args)
	if dest == "" {
		return nil
	}
	root := gitx.Canonicalize(dest)
	sha := runRealGit(realGit, root, "rev-parse", "HEAD")
	if sha == "" {
		return nil
	}
	if err := ingest.GitCommit(root, sha); err != nil {
		return err
	}
	logx.Debugf("proxy: registered clone of %s", root)
	return nil
}

// inferCloneDest does a best-effort parse of `git clone` args. Skips known
// flag/value pairs, takes the first remaining positional as URL, the second
// (if present) as the destination. Falls back to URL-derived basename.
func inferCloneDest(args []string) string {
	flagsWithValues := map[string]bool{
		"--template": true, "-o": true, "--origin": true,
		"-b": true, "--branch": true,
		"-u": true, "--upload-pack": true,
		"--reference": true, "--reference-if-able": true,
		"--depth": true, "--shallow-since": true, "--shallow-exclude": true,
		"--recurse-submodules": true,
		"--jobs": true, "-j": true,
		"--server-option": true,
		"--separate-git-dir": true,
		"--filter": true,
		"--sparse-checkout-set": true,
	}
	var positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "--") && strings.Contains(a, "=") {
			continue
		}
		if flagsWithValues[a] {
			i++
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		positional = append(positional, a)
	}
	var url, explicit string
	if len(positional) > 0 {
		url = positional[0]
	}
	if len(positional) > 1 {
		explicit = positional[1]
	}
	if explicit != "" {
		if filepath.IsAbs(explicit) {
			return explicit
		}
		cwd, _ := os.Getwd()
		return filepath.Join(cwd, explicit)
	}
	if url == "" {
		return ""
	}
	// Derive directory from URL: foo.git → foo, trailing slashes trimmed.
	tail := strings.TrimRight(url, "/:")
	parts := strings.FieldsFunc(tail, func(r rune) bool { return r == '/' || r == ':' })
	if len(parts) == 0 {
		return ""
	}
	name := parts[len(parts)-1]
	name = strings.TrimSuffix(name, ".git")
	if name == "" {
		return ""
	}
	cwd, _ := os.Getwd()
	return filepath.Join(cwd, name)
}

// runRealGit runs the cached real git path (not the on-PATH `git`, which
// would loop back through our shadow). cwd may be empty for current dir.
func runRealGit(realGit, cwd string, args ...string) string {
	cmd := exec.Command(realGit, args...)
	if cwd != "" {
		cmd.Dir = cwd
	}
	cmd.Env = gitx.BypassEnv()
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
