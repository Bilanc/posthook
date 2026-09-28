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
	//
	// It is a pattern (trailing *) on purpose. Git aborts a fetch with
	// "fatal: couldn't find remote ref" when a configured *exact* refspec
	// names a ref the remote does not have, which is every repo until the
	// first notes push. A pattern that matches nothing is skipped silently,
	// and the * still matches the empty string, so refs/notes/posthook lands
	// in refs/notes/posthook-remote exactly as before.
	FetchRefspec = "+" + paths.NotesRef + "*:" + TrackingRef + "*"

	// exactRefspec is the non-pattern form earlier releases wrote. It broke
	// bare `git fetch` / `git pull` in every repo whose remote had no notes
	// ref yet, so it is replaced by FetchRefspec wherever it is found.
	exactRefspec = "+" + paths.NotesRef + ":" + TrackingRef

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
// that the exact and legacy refspecs are gone (the legacy one from push
// too). Returns true iff git config was modified. Purely local; never
// touches the network.
func EnsureRefspec(repoRoot, remote string) bool {
	if remote == "" {
		remote = "origin"
	}
	if gitx.Run(repoRoot, "config", "--get", "remote."+remote+".url") == "" {
		return false
	}
	changed := RepairRefspec(repoRoot, remote)
	fetchKey := "remote." + remote + ".fetch"
	if !hasConfigValue(repoRoot, fetchKey, FetchRefspec) {
		gitx.Run(repoRoot, "config", "--add", fetchKey, FetchRefspec)
		changed = true
	}
	return changed
}

// RepairRefspec removes the refspecs earlier releases wrote that break the
// user's own git commands: the exact and legacy fetch refspecs make a bare
// `git fetch` / `git pull` abort with "couldn't find remote ref" while the
// remote has no notes ref yet, and the legacy push refspec makes a bare
// `git push` fail while the local notes ref does not exist. A remote whose
// fetch refspec was broken gets FetchRefspec in its place, so transport
// stays configured; a remote that never had one is left alone.
//
// remote == "" repairs every remote. Only the repository's local config is
// read and written: that is the only scope posthook ever wrote to, and
// reading the effective (system + global + local) config would report a
// repair for a refspec inherited from a scope this never edits. Cheap — a
// single `git config` read when there is nothing to fix — which is why the
// git shadow can afford to run it before every push, fetch and pull.
// Returns true iff git config was modified.
func RepairRefspec(repoRoot, remote string) bool {
	keyPattern := `^remote\..*\.(fetch|push)$`
	if remote != "" {
		keyPattern = "^" + regexpQuote("remote."+remote) + `\.(fetch|push)$`
	}
	type remoteConfig struct {
		brokenFetch, brokenPush []string
		hasCurrent              bool
	}
	byRemote := map[string]*remoteConfig{}
	var order []string
	for _, line := range strings.Split(gitx.Run(repoRoot, "config", "--local", "--get-regexp", keyPattern), "\n") {
		key, value, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		name, kind := splitRemoteKey(key)
		if name == "" {
			continue
		}
		rc := byRemote[name]
		if rc == nil {
			rc = &remoteConfig{}
			byRemote[name] = rc
			order = append(order, name)
		}
		switch {
		case kind == "fetch" && (value == exactRefspec || value == legacyRefspec):
			rc.brokenFetch = appendUnique(rc.brokenFetch, value)
		case kind == "fetch" && value == FetchRefspec:
			rc.hasCurrent = true
		case kind == "push" && value == legacyRefspec:
			rc.brokenPush = appendUnique(rc.brokenPush, value)
		}
	}
	changed := false
	for _, name := range order {
		rc := byRemote[name]
		fetchKey := "remote." + name + ".fetch"
		for _, v := range rc.brokenFetch {
			gitx.Run(repoRoot, "config", "--local", "--unset-all", fetchKey, "^"+regexpQuote(v)+"$")
			changed = true
		}
		if len(rc.brokenFetch) > 0 && !rc.hasCurrent {
			gitx.Run(repoRoot, "config", "--local", "--add", fetchKey, FetchRefspec)
		}
		for _, v := range rc.brokenPush {
			gitx.Run(repoRoot, "config", "--local", "--unset-all", "remote."+name+".push", "^"+regexpQuote(v)+"$")
			changed = true
		}
	}
	return changed
}

// splitRemoteKey turns "remote.<name>.fetch" into ("<name>", "fetch").
func splitRemoteKey(key string) (name, kind string) {
	rest, ok := strings.CutPrefix(key, "remote.")
	if !ok {
		return "", ""
	}
	i := strings.LastIndex(rest, ".")
	if i <= 0 {
		return "", ""
	}
	return rest[:i], rest[i+1:]
}

func appendUnique(list []string, v string) []string {
	for _, have := range list {
		if have == v {
			return list
		}
	}
	return append(list, v)
}

// Fetch pulls the remote's notes into the tracking ref and merges them into
// the local notes ref. A remote with no notes ref yet is not an error: the
// pattern refspec simply matches nothing and no refs are created (check
// RefExists(TrackingRef) to tell the two apart). An error means the fetch
// itself failed, e.g. the remote was unreachable.
func Fetch(repoRoot, remote string) error {
	if remote == "" {
		remote = "origin"
	}
	if _, err := runTimed(repoRoot, "fetch", "--quiet", "--no-tags", remote, FetchRefspec); err != nil {
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
	// Best-effort: an unreachable remote will fail the push below anyway.
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
