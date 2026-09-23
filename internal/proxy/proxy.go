// Package proxy implements the `git` shadow. When the posthook binary is
// invoked under the name "git" (via a symlink at ~/.local/bin/git), Run
// takes over: it spawns the real git as a child, forwards stdio and signals
// faithfully so the user sees identical behavior, and after success runs
// our capture logic for `commit` and `clone`, and our notes transport for
// `push`, `fetch` and `pull`.
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
	"github.com/bilanc/posthook/internal/notes"
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
	interceptable := !bypass && isInterceptable(subcommand)

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
		case "push":
			err = handlePush(realGit, subcommandArgs)
		case "fetch", "pull":
			err = handleFetch(realGit, subcommandArgs)
		}
		if err != nil {
			logx.Warnf("proxy capture failed for %s: %v", subcommand, err)
		}
	}
	os.Exit(code)
}

func isInterceptable(subcommand string) bool {
	switch subcommand {
	case "commit", "clone":
		return true
	case "push", "fetch", "pull":
		return notes.Enabled()
	}
	return false
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
	root := gitx.Canonicalize(repoRoot)
	if err := ingest.GitCommit(root, sha); err != nil {
		return err
	}
	if len(sha) >= 7 {
		logx.Debugf("proxy: captured commit %s", sha[:7])
	}
	if notes.Enabled() && notes.EnsureRefspec(root, "origin") {
		logx.Debugf("proxy: configured notes fetch refspec for origin")
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
	if notes.Enabled() {
		// The fresh clone has no refspec yet, so fetch notes explicitly once;
		// EnsureRefspec makes every later fetch/pull carry them for free.
		notes.EnsureRefspec(root, "origin")
		if err := notes.Fetch(root, "origin"); err == nil {
			logx.Debugf("proxy: fetched attribution notes for %s", root)
		}
	}
	return nil
}

// handlePush runs after a successful `git push`: it merges any notes the
// remote already has into ours, then pushes refs/notes/posthook so
// teammates can `posthook blame` the commits that just went up.
func handlePush(realGit string, args []string) error {
	if pushSkipsNotes(args) {
		return nil
	}
	root := runRealGit(realGit, "", "rev-parse", "--show-toplevel")
	if root == "" {
		return nil
	}
	root = gitx.Canonicalize(root)
	remote := remoteFromArgs(args, pushFlagsWithValues)
	if remote == "" {
		remote = defaultPushRemote(realGit, root)
	}
	if remote == "" {
		return nil
	}
	notes.EnsureRefspec(root, remote)
	if err := notes.Push(root, remote); err != nil {
		return err
	}
	logx.Debugf("proxy: pushed attribution notes to %s", remote)
	return nil
}

// handleFetch runs after a successful `git fetch` / `git pull`.
//
// A bare `git fetch` / `git pull` (no refspec on the command line) uses the
// configured refspecs, so the refspec installed by EnsureRefspec already
// brought the notes down into the tracking ref and only the local merge is
// left. With an explicit refspec (`git pull origin main`) git ignores the
// configured ones, so the notes are fetched explicitly — one extra
// single-ref round trip.
func handleFetch(realGit string, args []string) error {
	if hasFlag(args, "--dry-run") {
		return nil
	}
	root := runRealGit(realGit, "", "rev-parse", "--show-toplevel")
	if root == "" {
		return nil
	}
	root = gitx.Canonicalize(root)
	remote := remoteFromArgs(args, fetchFlagsWithValues)
	if remote == "" {
		remote = "origin"
	}
	refspecAdded := notes.EnsureRefspec(root, remote)
	explicit := fetchHasExplicitRefspec(args)
	if refspecAdded || explicit || !notes.RefExists(root, notes.TrackingRef) {
		logx.Debugf("proxy: fetching attribution notes from %s (refspec added=%v, explicit refspec=%v)", remote, refspecAdded, explicit)
		return notes.Fetch(root, remote)
	}
	logx.Debugf("proxy: merging attribution notes brought down by fetch")
	return notes.MergeTracking(root)
}

// fetchHasExplicitRefspec reports whether a fetch/pull invocation names
// refspecs after the remote (`git pull origin main`), in which case git does
// not apply the configured refspecs.
func fetchHasExplicitRefspec(args []string) bool {
	positional := 0
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positional += len(args) - i - 1
			break
		}
		if strings.HasPrefix(a, "-") {
			if fetchFlagsWithValues[a] && !strings.Contains(a, "=") {
				i++
			}
			continue
		}
		positional++
	}
	return positional >= 2
}

var pushFlagsWithValues = map[string]bool{
	"--repo": true, "-o": true, "--push-option": true,
	"--receive-pack": true, "--exec": true, "--recurse-submodules": true,
	"--signed": true, "--force-if-includes": false,
}

var fetchFlagsWithValues = map[string]bool{
	"--depth": true, "--deepen": true, "--shallow-since": true,
	"--shallow-exclude": true, "--upload-pack": true, "-o": true,
	"--server-option": true, "--negotiation-tip": true, "--refmap": true,
	"--recurse-submodules": true, "-j": true, "--jobs": true,
	"--filter": true, "-s": true, "--strategy": true, "-X": true,
	"--strategy-option": true, "--log": true, "--cleanup": true,
	"--gpg-sign": true, "-S": true,
}

// remoteFromArgs returns the repository argument of a push/fetch/pull
// invocation: --repo=<x> or --repo <x>, else the first positional. Empty
// when git will pick the remote itself.
func remoteFromArgs(args []string, flagsWithValues map[string]bool) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		}
		if strings.HasPrefix(a, "--repo=") {
			return strings.TrimPrefix(a, "--repo=")
		}
		if strings.HasPrefix(a, "-") {
			if flagsWithValues[a] && !strings.Contains(a, "=") {
				i++
			}
			continue
		}
		return a
	}
	return ""
}

// pushSkipsNotes reports pushes that should not trigger notes transport:
// dry runs, deletions, mirrors, and pushes where the user already handles
// the notes ref themselves.
func pushSkipsNotes(args []string) bool {
	for _, a := range args {
		switch a {
		case "--dry-run", "-n", "--delete", "-d", "--mirror", "--all", "--branches", "--tags", "--prune":
			return true
		}
		if strings.Contains(a, "refs/notes/") {
			return true
		}
	}
	return false
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// defaultPushRemote mirrors git's own choice for a bare `git push`:
// branch.<name>.pushRemote, remote.pushDefault, branch.<name>.remote, origin.
func defaultPushRemote(realGit, root string) string {
	branch := runRealGit(realGit, root, "symbolic-ref", "--quiet", "--short", "HEAD")
	if branch != "" {
		if r := runRealGit(realGit, root, "config", "--get", "branch."+branch+".pushRemote"); r != "" {
			return r
		}
	}
	if r := runRealGit(realGit, root, "config", "--get", "remote.pushDefault"); r != "" {
		return r
	}
	if branch != "" {
		if r := runRealGit(realGit, root, "config", "--get", "branch."+branch+".remote"); r != "" {
			return r
		}
	}
	if runRealGit(realGit, root, "config", "--get", "remote.origin.url") != "" {
		return "origin"
	}
	return ""
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
		"--jobs":               true, "-j": true,
		"--server-option":       true,
		"--separate-git-dir":    true,
		"--filter":              true,
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
