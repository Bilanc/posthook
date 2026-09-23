// Package notes moves posthook's line-attribution git notes
// (refs/notes/posthook) between a repo and its remote.
//
// Notes are a shared ref, so two machines that both commit will diverge.
// A bare `git push` of a diverged notes ref is rejected as non-fast-forward
// and a plain `git fetch` into the same ref prints a rejection on every
// fetch. Both are avoided here by never fetching straight into the local
// notes ref:
//
//   - remote notes are fetched (forced) into a tracking ref,
//     refs/notes/posthook-remote, which can never conflict with local writes;
//   - the tracking ref is then merged into refs/notes/posthook with
//     `git notes merge`. Notes for different commits merge trivially; on the
//     rare same-commit conflict the local note wins (-s ours) and no merge
//     state is left behind;
//   - only after that merge is the local ref pushed, so the push is always a
//     fast-forward.
//
// Every operation is best-effort and time-boxed: a slow or unreachable
// remote must never stall the user's own git command for long, and a
// failure is only ever reported at debug level.
package notes

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/bilanc/posthook/internal/gitx"
	"github.com/bilanc/posthook/internal/logx"
	"github.com/bilanc/posthook/internal/paths"
)

const (
	// TrackingRef is where remote notes land before being merged locally.
	TrackingRef = paths.NotesRef + "-remote"

	// FetchRefspec is the refspec added to remote.<name>.fetch so a bare
	// `git fetch` / `git pull` carries teammates' notes down at no extra
	// network cost. (Git ignores configured refspecs when the command names
	// its own, e.g. `git pull origin main`; the shadow fetches notes
	// explicitly in that case.) Forced (+) because the tracking ref is only
	// ever written by fetch, so overwriting it is always correct.
	FetchRefspec = "+" + paths.NotesRef + ":" + TrackingRef

	// legacyRefspec is what posthook <= 0.2.x added to both fetch and push.
	// It made bare `git push` fail and `git fetch` warn once notes diverged,
	// so it is removed wherever it is found.
	legacyRefspec = paths.NotesRef + ":" + paths.NotesRef

	networkTimeout = 20 * time.Second
)

// Enabled reports whether notes transport is on. POSTHOOK_NOTES_SYNC=0 turns
// it off for people who want attribution to stay on their own machine.
func Enabled() bool {
	return os.Getenv("POSTHOOK_NOTES_SYNC") != "0"
}

// EnsureRefspec makes sure remote.<remote>.fetch carries FetchRefspec and
// that the legacy refspec is gone from both fetch and push. Returns true iff
// git config was modified. Purely local; never touches the network.
func EnsureRefspec(repoRoot, remote string) bool {
	if remote == "" {
		remote = "origin"
	}
	if gitx.Run(repoRoot, "config", "--get", "remote."+remote+".url") == "" {
		return false
	}
	changed := false
	fetchKey := "remote." + remote + ".fetch"
	pushKey := "remote." + remote + ".push"

	if hasConfigValue(repoRoot, fetchKey, legacyRefspec) {
		gitx.Run(repoRoot, "config", "--unset-all", fetchKey, "^"+regexpQuote(legacyRefspec)+"$")
		changed = true
	}
	if hasConfigValue(repoRoot, pushKey, legacyRefspec) {
		gitx.Run(repoRoot, "config", "--unset-all", pushKey, "^"+regexpQuote(legacyRefspec)+"$")
		changed = true
	}
	if !hasConfigValue(repoRoot, fetchKey, FetchRefspec) {
		gitx.Run(repoRoot, "config", "--add", fetchKey, FetchRefspec)
		changed = true
	}
	return changed
}

// Fetch pulls the remote's notes into the tracking ref and merges them into
// the local notes ref. Safe when either side has no notes yet.
func Fetch(repoRoot, remote string) error {
	if remote == "" {
		remote = "origin"
	}
	if _, err := runTimed(repoRoot, "fetch", "--quiet", "--no-tags", remote, FetchRefspec); err != nil {
		// Most likely the remote simply has no notes ref yet.
		logx.Debugf("notes: fetch from %s: %v", remote, err)
		return err
	}
	return MergeTracking(repoRoot)
}

// MergeTracking folds refs/notes/posthook-remote into refs/notes/posthook.
// It is a local operation, so it is also run after an ordinary fetch/pull
// that carried the tracking ref down via FetchRefspec. A no-op when the
// tracking ref is absent.
func MergeTracking(repoRoot string) error {
	if !RefExists(repoRoot, TrackingRef) {
		return nil
	}
	if !RefExists(repoRoot, paths.NotesRef) {
		_, err := runLocal(repoRoot, "update-ref", paths.NotesRef, TrackingRef)
		return err
	}
	if gitx.Run(repoRoot, "rev-parse", "--verify", "--quiet", paths.NotesRef) ==
		gitx.Run(repoRoot, "rev-parse", "--verify", "--quiet", TrackingRef) {
		return nil
	}
	if _, err := runLocal(repoRoot, "notes", "--ref="+paths.NotesRef, "merge", "--quiet", "-s", "ours", TrackingRef); err != nil {
		// Leave no half-merge behind.
		_, _ = runLocal(repoRoot, "notes", "--ref="+paths.NotesRef, "merge", "--abort")
		return err
	}
	return nil
}

// Push merges the remote's current notes into the local ref and then pushes
// the local ref. A no-op when there are no local notes.
func Push(repoRoot, remote string) error {
	if remote == "" {
		remote = "origin"
	}
	if !RefExists(repoRoot, paths.NotesRef) {
		return nil
	}
	// Ignore fetch errors: a remote with no notes yet is the common first case.
	_ = Fetch(repoRoot, remote)
	if _, err := runTimed(repoRoot, "push", "--quiet", remote, paths.NotesRef+":"+paths.NotesRef); err != nil {
		logx.Debugf("notes: push to %s: %v", remote, err)
		return err
	}
	return nil
}

// RefExists reports whether ref resolves in repoRoot.
func RefExists(repoRoot, ref string) bool {
	return gitx.Run(repoRoot, "rev-parse", "--verify", "--quiet", ref) != ""
}

func hasConfigValue(repoRoot, key, value string) bool {
	for _, l := range strings.Split(gitx.Run(repoRoot, "config", "--get-all", key), "\n") {
		if l == value {
			return true
		}
	}
	return false
}

// regexpQuote escapes value for use as a git-config value regex.
func regexpQuote(value string) string {
	var b strings.Builder
	for _, r := range value {
		if strings.ContainsRune(`.+*?()[]{}|^$\`, r) {
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func runLocal(repoRoot string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = repoRoot
	cmd.Env = gitx.BypassEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", wrap(args, out, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func runTimed(repoRoot string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), networkTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = repoRoot
	cmd.Env = append(gitx.BypassEnv(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", wrap(args, out, err)
	}
	return strings.TrimSpace(string(out)), nil
}

type gitError struct {
	args []string
	out  string
	err  error
}

func (e *gitError) Error() string {
	msg := "git " + strings.Join(e.args, " ") + ": " + e.err.Error()
	if e.out != "" {
		msg += ": " + e.out
	}
	return msg
}

func (e *gitError) Unwrap() error { return e.err }

func wrap(args []string, out []byte, err error) error {
	return &gitError{args: args, out: strings.TrimSpace(string(out)), err: err}
}
