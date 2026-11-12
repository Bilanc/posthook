package ingest

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bilanc/posthook/internal/gitx"
	"github.com/bilanc/posthook/internal/logx"
	"github.com/bilanc/posthook/internal/notes"
	"github.com/bilanc/posthook/internal/paths"
	"github.com/bilanc/posthook/internal/spool"
	"github.com/bilanc/posthook/internal/store"
)

// Catch-up: recording commits posthook did not see being made.
//
// A commit is normally recorded the moment it is made, by the git shadow
// (`git commit` through ~/.local/bin/git) or by the per-repo post-commit
// hook. Both can be missing: an IDE runs its own git, a tool calls git by
// absolute path, ~/.local/bin is not first on PATH, an older checkout or a
// worktree has no hook. And neither fires for commits that `git rebase`,
// `git merge`, `git pull`, `git cherry-pick` or `git revert` create. A commit
// nobody recorded has no attribution, and a merge request made only of such
// commits is invisible.
//
// So at two moments posthook looks for what it missed in a repository:
//
//   - when an agent turn ends (Stop), in every repository the session worked
//     in;
//   - when the shadow sees `git push`, for the repository being pushed (the
//     shadow lists the commits and the worker does the rest).
//
// "Missed" means: on the branch checked out in that working tree, on no
// remote, made after posthook was installed, and not in the store.
//
//   - "On no remote" is what tells a commit made on this machine from one
//     that was fetched: everyone else's commits arrive through a
//     remote-tracking ref. It also means the list has to be taken before a
//     push, not after.
//   - Only the checked-out branch, not every local branch: worktrees of one
//     repository share their branches, and each is its own repository to
//     posthook, with its own edits. A commit recorded from the wrong
//     worktree would be attributed against edits that are not there. For the
//     same reason a rewrite made in another checkout is recorded with the
//     checkout its original was recorded in (homeRepository).
//
// What this cannot see: a commit made and pushed by a git that never goes
// through the shadow, with no agent turn ending in between.
//
// A commit found this way is often the result of a rewrite (a rebase, an
// amend, a squash, a cherry-pick) whose original was recorded when it was
// made. store.CarriedCommits says how the two are told to be the same work,
// and retireCarried what the original then gives up.

// maxCatchUpCommits bounds one look at a repository.
const maxCatchUpCommits = 200

// catchUpLookback is how far back a look goes. A checkout can hold years of
// local branches whose commits are on no remote any more (merged by squash,
// branch deleted); the commits worth recording are the recent ones.
const catchUpLookback = "30.days.ago"

// catchUpInterval is how often one repository is looked at on Stop events.
const catchUpInterval = time.Minute

var (
	catchUpMu   sync.Mutex
	lastCatchUp = map[string]time.Time{}
)

// UnpushedCommitsArgs are the git arguments that list the commits on the
// checked-out branch that no remote has and that were made within
// catchUpLookback, oldest first, newest maxCatchUpCommits.
func UnpushedCommitsArgs() []string {
	return []string{"rev-list", "HEAD", "--not", "--remotes",
		"--since=" + catchUpLookback,
		"--max-count=" + strconv.Itoa(maxCatchUpCommits), "--reverse"}
}

// ProcessCommitJob handles one job the git shadow queued at a push.
//
// The shadow sent the notes it had right after the push, before this job
// could write the ones for the commits it records. When the job changes the
// notes, they are sent to the same remote again, so a teammate who fetches
// the pushed commits gets their notes without waiting for the next push.
// Best-effort, like all notes transport: a failure leaves them for that next
// push.
func ProcessCommitJob(job spool.CommitJob) error {
	before := notesTip(job.RepoRoot)
	_, err := RecordMissingCommits(job.RepoRoot, job.SHAs)
	if job.NotesRemote != "" && notes.Enabled() {
		if after := notesTip(job.RepoRoot); after != "" && after != before {
			if perr := notes.Push(job.RepoRoot, job.NotesRemote); perr != nil {
				logx.Debugf("commit catch-up: sending notes to %s failed: %v", job.NotesRemote, perr)
			}
		}
	}
	return err
}

// notesTip is what the local notes ref points at, empty when there is none.
func notesTip(repoRoot string) string {
	if repoRoot == "" {
		return ""
	}
	return gitx.Run(repoRoot, "rev-parse", "--verify", "--quiet", paths.NotesRef)
}

// notedCommits returns the commits that have a posthook note in this
// repository.
func notedCommits(repoRoot string) map[string]bool {
	noted := map[string]bool{}
	for _, line := range nonEmptyLines(gitx.Run(repoRoot, "notes", "--ref="+paths.NotesRef, "list")) {
		if _, commit, ok := strings.Cut(line, " "); ok {
			noted[strings.TrimSpace(commit)] = true
		}
	}
	return noted
}

// catchUpSessionRepos looks for missed commits in the repositories a session
// has events in, and in the repository its last event resolved to. Fail-soft:
// a session's Stop must not fail because of this.
func catchUpSessionRepos(db *store.DB, sessionID, repoRoot string) {
	roots := []string{}
	seen := map[string]bool{}
	add := func(root string) {
		if root != "" && !seen[root] {
			seen[root] = true
			roots = append(roots, root)
		}
	}
	add(repoRoot)
	rows, err := db.Query(`
		SELECT DISTINCT r.root_path
		FROM events e JOIN repositories r ON r.id = e.repo_id
		WHERE e.session_id = ?
		LIMIT 20`, sessionID)
	if err == nil {
		for rows.Next() {
			var root string
			if rows.Scan(&root) == nil {
				add(root)
			}
		}
		rows.Close()
	}
	for _, root := range roots {
		if _, err := CatchUpRepo(root); err != nil {
			logx.Warnf("commit catch-up failed in %s: %v", root, err)
		}
	}
}

// CatchUpRepo records the missed commits of one repository. It looks at most
// once per catchUpInterval; a call inside that window does nothing.
func CatchUpRepo(root string) (int, error) {
	root = gitx.Canonicalize(root)
	catchUpMu.Lock()
	if last, ok := lastCatchUp[root]; ok && time.Since(last) < catchUpInterval {
		catchUpMu.Unlock()
		return 0, nil
	}
	lastCatchUp[root] = time.Now()
	catchUpMu.Unlock()

	if gitx.FindRepoRoot(root) == "" {
		return 0, nil
	}
	out, err := gitx.Output(root, UnpushedCommitsArgs()...)
	if err != nil {
		return 0, nil // not a repository any more, or an empty one
	}
	return RecordMissingCommits(root, nonEmptyLines(string(out)))
}

// RecordMissingCommits records those of shas (oldest first) that the store
// does not hold and that were made after posthook was installed, and returns
// how many it recorded. Then it settles what the new commits change: a later
// commit on the same files may have taken edits that belong to an earlier
// one that was only just recorded, and a commit whose work one of these
// carries may be on no branch any more.
func RecordMissingCommits(repoRoot string, shas []string) (int, error) {
	if repoRoot == "" || len(shas) == 0 {
		return 0, nil
	}
	repoRoot = gitx.Canonicalize(repoRoot)
	if gitx.FindRepoRoot(repoRoot) == "" {
		return 0, nil
	}
	if len(shas) > maxCatchUpCommits {
		shas = shas[len(shas)-maxCatchUpCommits:]
	}
	db, err := store.Open()
	if err != nil {
		return 0, err
	}
	repoID, err := upsertRepositoryByCwd(db, repoRoot)
	if err != nil {
		return 0, err
	}

	known, err := recordedSHAs(db, shas)
	if err != nil {
		return 0, err
	}
	var missing []string
	for _, sha := range shas {
		if !known[sha] {
			missing = append(missing, sha)
		}
	}

	recorded := 0
	if len(missing) > 0 {
		made := committerTimes(repoRoot, missing)
		installedAt, haveInstall := db.InstalledAt()
		for _, sha := range missing {
			at, ok := made[sha]
			if !ok {
				continue // git no longer has it
			}
			if haveInstall && at < installedAt.Unix() {
				continue // made before posthook was on this machine
			}
			if err := GitCommit(repoRoot, sha); err != nil {
				return recorded, err
			}
			recorded++
		}
		if recorded > 0 {
			logx.Debugf("commit catch-up: recorded %d commit(s) in %s", recorded, repoRoot)
		}
	}

	// Every commit in the list, new or not, is looked at once more. A commit
	// recorded when it was made can since have been replaced (the push that
	// queued this job may itself be the force-push of a rebase), and one
	// recorded just now may sit before a commit that was attributed without
	// it.
	settleCommits(db, repoRoot, repoID, shas, recorded > 0)
	return recorded, nil
}

// settleCommits re-attributes the listed commits where a newly recorded,
// earlier commit can have changed their window, and retires the attribution
// of commits they have replaced.
func settleCommits(db *store.DB, repoRoot, repoID string, shas []string, reattribute bool) {
	var noted map[string]bool
	if reattribute {
		noted = notedCommits(repoRoot)
	}
	for _, sha := range shas {
		// The row the commit is recorded under, which is this checkout's
		// unless it was recorded from another checkout of the repository or
		// moved to one (homeRepository). A commit recorded only in an
		// unrelated repository is not this one's to settle.
		commitID, commitRepo, ok := recordedCommit(db, repoID, sha)
		if !ok {
			continue
		}
		if reattribute {
			attributeAndNote(db, repoRoot, commitRepo, sha, commitID, noted[sha])
		} else {
			retireCarried(db, repoRoot, sha, commitID, carriedBy(db, repoRoot, commitRepo, sha, commitID))
		}
	}
}

// retireCarried has the commits this one carries (store.CarriedCommits) give
// up the lines it is attributed too, once no branch holds them any more: the
// commits a rebase, an amend or a squash replaced. Counting both would double
// the lines. A carried commit that is still on a branch (a cherry-pick's
// original) keeps its attribution.
func retireCarried(db *store.DB, repoRoot, sha, commitID string, carried []string) {
	for _, carriedID := range carried {
		var carriedSHA, recordedAt string
		if err := db.QueryRow(`
			SELECT c.sha, COALESCE(r.root_path, '')
			FROM commits c LEFT JOIN repositories r ON r.id = c.repo_id
			WHERE c.id = ?`, carriedID).Scan(&carriedSHA, &recordedAt); err != nil {
			continue
		}
		if mayHold(repoRoot, carriedSHA) {
			continue
		}
		// A commit recorded in another checkout can be on a branch only that
		// checkout has: a separate clone keeps its own branches. (Worktrees
		// share theirs, and a checkout that is gone has none.)
		if recordedAt != "" && recordedAt != repoRoot && checkoutMayHold(recordedAt, carriedSHA) {
			continue
		}
		changed, err := db.GiveUpAttribution(carriedID, commitID)
		if err != nil {
			logx.Warnf("retiring a replaced commit failed: %v", err)
		} else if changed && len(carriedSHA) >= 7 && len(sha) >= 7 {
			logx.Debugf("commit %s is on no branch any more; %s carries its lines", carriedSHA[:7], sha[:7])
		}
	}
}

// carriedBy is store.CarriedCommits for a recorded commit.
func carriedBy(db *store.DB, repoRoot, repoID, sha, commitID string) []string {
	var committedAt string
	if err := db.QueryRow(`SELECT committed_at FROM commits WHERE id = ?`, commitID).Scan(&committedAt); err != nil {
		return nil
	}
	return db.CarriedCommits(commitID, sha, repoID, committedAt, repoRoot)
}

// branchHolds reports whether a local or remote-tracking branch of the
// checkout at root holds the commit, and whether that could be established.
// A commit git no longer has is on no branch. Any other git failure (a lock,
// a permission problem, not a repository just now) says nothing about the
// branches.
func branchHolds(root, sha string) (held, known bool) {
	out, err := gitx.Output(root, "branch", "--all", "--contains", sha, "--format=%(refname)")
	if err == nil {
		return strings.TrimSpace(string(out)) != "", true
	}
	// git fails here for a commit it does not have too, so ask it that.
	if have, err := gitx.HasObject(root, sha); err == nil && !have {
		return false, true
	}
	return false, false
}

// mayHold reports whether a branch of the checkout at root holds the commit,
// or may. Giving a commit's lines up cannot be taken back, so it happens
// only when git has said that no branch holds the commit.
func mayHold(root, sha string) bool {
	held, known := branchHolds(root, sha)
	return held || !known
}

// checkoutMayHold is mayHold for a checkout that may be gone: one that no
// longer exists holds nothing.
func checkoutMayHold(root, sha string) bool {
	if _, err := os.Stat(root); err != nil {
		return !os.IsNotExist(err)
	}
	return mayHold(root, sha)
}

// recordedSHAs returns which of shas the store holds, under any repository:
// a commit recorded in one worktree is the same commit in another, and
// recording it twice would only add a copy with nothing attributed.
func recordedSHAs(db *store.DB, shas []string) (map[string]bool, error) {
	known := map[string]bool{}
	args := make([]any, 0, len(shas))
	for _, sha := range shas {
		args = append(args, sha)
	}
	rows, err := db.Query(`SELECT DISTINCT sha FROM commits WHERE sha IN (?`+
		strings.Repeat(", ?", len(shas)-1)+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			return nil, err
		}
		known[sha] = true
	}
	return known, rows.Err()
}

// committerTimes returns each commit's committer time (unix seconds) in one
// git call. A rebase, an amend and a cherry-pick all set it to when they ran,
// which is what "made on this machine after posthook was installed" needs.
func committerTimes(repoRoot string, shas []string) map[string]int64 {
	args := append([]string{"log", "--no-walk=unsorted", "--format=%H %ct"}, shas...)
	out, err := gitx.Output(repoRoot, args...)
	if err != nil {
		return nil
	}
	times := map[string]int64{}
	for _, line := range nonEmptyLines(string(out)) {
		sha, at, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		if n, err := strconv.ParseInt(at, 10, 64); err == nil {
			times[sha] = n
		}
	}
	return times
}

func nonEmptyLines(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}
