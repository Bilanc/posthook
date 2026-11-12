package store

import (
	"database/sql"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/bilanc/posthook/internal/gitx"
)

// RefreshCommitAttributions rebuilds commit_sessions and commit_session_files
// for one commit (when commitID != "") or all commits.
//
// Accuracy model (see SPEC_accurate_attribution.md): for each commit C and file
// F, an AI line range only counts if the line it covers was actually ADDED by C
// (it falls inside a `git diff` added hunk, in C's committed coordinates). Lines
// are de-duplicated across edits with latest-edit-wins, so re-editing the same
// line counts once and AI churn that never landed in the commit is dropped. The
// result is bounded by the file's additions, so downstream
// `AI lines / additions` ratios sit in [0,1] without an artificial 100% cap.
//
// The previous implementation summed every AI edit's new_text_lines over a time
// window with no intersection against the committed diff, which measured edit
// churn (~1.9x the real additions, up to 14x on iteration-heavy commits).
//
// When the local git repo is unavailable (e.g. backfilling on a machine where
// the working copy moved), we fall back to capping the per-file churn sum at the
// commit's recorded lines_added so the bound still holds.
func (db *DB) RefreshCommitAttributions(commitID string) (int, error) {
	if commitID == "" {
		// The whole-database pass (the one-time repair of a database from
		// before schema v12) judges "previous commit" by time alone, as it
		// always has. It has no step that retires a replaced commit, so
		// crediting a replacement here would count its lines twice.
		return db.refreshCommitAttributions("", "", []string{})
	}
	return db.refreshCommitAttributions(commitID, "", nil)
}

// RefreshCommitAttributionCarrying is RefreshCommitAttributions for one commit
// whose carried commits (see CarriedCommits) the caller has already worked
// out, so the git calls behind that list are made once per commit.
//
// root is the working tree to ask git in. The commit's own repository row
// may name a checkout that is gone (a removed worktree) while the commit is
// being recorded from another checkout of the same repository.
func (db *DB) RefreshCommitAttributionCarrying(commitID, root string, carried []string) (int, error) {
	if commitID == "" {
		return 0, nil
	}
	if carried == nil {
		carried = []string{}
	}
	return db.refreshCommitAttributions(commitID, root, carried)
}

// carried is nil when it has to be worked out per commit; root is empty to
// use each commit's own repository row.
func (db *DB) refreshCommitAttributions(commitID, root string, carried []string) (int, error) {
	var commitArg any
	if commitID != "" {
		if _, err := db.Exec(`DELETE FROM commit_sessions WHERE commit_id = ?`, commitID); err != nil {
			return 0, err
		}
		if _, err := db.Exec(`DELETE FROM commit_session_files WHERE commit_id = ?`, commitID); err != nil {
			return 0, err
		}
		commitArg = commitID
	} else {
		if _, err := db.Exec(`DELETE FROM commit_sessions`); err != nil {
			return 0, err
		}
		if _, err := db.Exec(`DELETE FROM commit_session_files`); err != nil {
			return 0, err
		}
		commitArg = nil
	}

	rows, err := db.Query(`
		SELECT c.id, c.sha, c.repo_id, c.committed_at, COALESCE(r.root_path, '')
		FROM commits c
		LEFT JOIN repositories r ON r.id = c.repo_id
		WHERE (? IS NULL OR c.id = ?)`,
		commitArg, commitArg)
	if err != nil {
		return 0, err
	}
	type commitRow struct {
		id, sha, repoID, committedAt, root string
	}
	var commits []commitRow
	for rows.Next() {
		var c commitRow
		if err := rows.Scan(&c.id, &c.sha, &c.repoID, &c.committedAt, &c.root); err != nil {
			rows.Close()
			return 0, err
		}
		commits = append(commits, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	total := 0
	for _, c := range commits {
		if root != "" {
			c.root = root
		}
		n, err := db.attributeCommit(c.id, c.sha, c.repoID, c.committedAt, c.root, carried)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// candRange is one captured AI line range that is in-window for a commit/file.
type candRange struct {
	eventID   string
	sessionID string
	agentSlug string
	modelSlug sql.NullString
	ts        string
	start     int
	end       int
	newLines  int
}

// fileAgg accumulates one session's attributed lines within a single file.
type fileAgg struct {
	lines    int
	eventIDs map[string]bool
	firstTS  string
	lastTS   string
}

func (f *fileAgg) touch(eventID, ts string) {
	if f.eventIDs == nil {
		f.eventIDs = map[string]bool{}
	}
	f.eventIDs[eventID] = true
	if f.firstTS == "" || ts < f.firstTS {
		f.firstTS = ts
	}
	if ts > f.lastTS {
		f.lastTS = ts
	}
}

type sessionMeta struct {
	agent        string
	model        sql.NullString
	firstTS      string
	lastTS       string
	usedFallback bool
}

// attributeCommit computes accurate per-session AI attribution for one commit
// and writes commit_session_files + commit_sessions rows. Returns the number of
// commit_sessions rows written.
func (db *DB) attributeCommit(commitID, sha, repoID, committedAt, root string, carried []string) (int, error) {
	files, err := db.commitFilesWithAdds(commitID)
	if err != nil {
		return 0, err
	}
	if len(files) == 0 {
		return 0, nil
	}
	if carried == nil {
		carried = db.CarriedCommits(commitID, sha, repoID, committedAt, root)
	}

	// sessionID -> file -> agg
	perSessionFile := map[string]map[string]*fileAgg{}
	meta := map[string]*sessionMeta{}

	getAgg := func(sessionID, file string) *fileAgg {
		m := perSessionFile[sessionID]
		if m == nil {
			m = map[string]*fileAgg{}
			perSessionFile[sessionID] = m
		}
		fa := m[file]
		if fa == nil {
			fa = &fileAgg{}
			m[file] = fa
		}
		return fa
	}
	recordMeta := func(c candRange) {
		sm := meta[c.sessionID]
		if sm == nil {
			sm = &sessionMeta{agent: c.agentSlug, model: c.modelSlug, firstTS: c.ts, lastTS: c.ts}
			meta[c.sessionID] = sm
		}
		if !sm.model.Valid && c.modelSlug.Valid {
			sm.model = c.modelSlug
		}
		if c.ts < sm.firstTS {
			sm.firstTS = c.ts
		}
		if c.ts > sm.lastTS {
			sm.lastTS = c.ts
		}
	}

	for file, linesAdded := range files {
		cands, err := db.candidateRanges(repoID, file, committedAt, carried)
		if err != nil {
			return 0, err
		}
		if len(cands) == 0 {
			continue
		}
		for _, c := range cands {
			recordMeta(c)
		}

		added, haveHunks := addedLineSet(root, sha, file)
		if haveHunks {
			// Intersect AI ranges with the lines C actually added, de-duping
			// per line with latest-edit-wins (candidates arrive ts-ascending).
			owner := map[int]candRange{}
			for _, c := range cands {
				for ln := c.start; ln <= c.end; ln++ {
					if added[ln] {
						owner[ln] = c
					}
				}
			}
			for _, c := range owner {
				fa := getAgg(c.sessionID, file)
				fa.lines++
				fa.touch(c.eventID, c.ts)
			}
		} else {
			// Fallback: no git access for hunks. Cap the per-file churn sum at
			// the recorded additions, split proportionally across sessions.
			raw := map[string]int{}
			rawTotal := 0
			for _, c := range cands {
				raw[c.sessionID] += c.newLines
				rawTotal += c.newLines
			}
			for _, c := range cands {
				fa := getAgg(c.sessionID, file)
				fa.touch(c.eventID, c.ts)
			}
			for sessionID, r := range raw {
				n := r
				if rawTotal > linesAdded && rawTotal > 0 {
					n = r * linesAdded / rawTotal
				}
				perSessionFile[sessionID][file].lines = n
				meta[sessionID].usedFallback = true
			}
		}
	}

	// Drive the write off every session that had in-window candidates — not just
	// those that netted lines. Sessions that now attribute to zero still get a
	// 0-line commit_sessions row: the cloud landing zone is append-only with no
	// tombstones, so without a fresh row the previous (inflated) blob would stay
	// "latest" in the dbt dedup. A 0-line row re-syncs and overwrites it.
	written := 0
	for sessionID, sm := range meta {
		fileMap := perSessionFile[sessionID]
		var totalLines, eventCount, filesTouched int
		firstTS, lastTS := sm.firstTS, sm.lastTS
		for file, fa := range fileMap {
			if fa.lines <= 0 {
				continue
			}
			if _, err := db.Exec(`
				INSERT INTO commit_session_files (
					commit_id, session_id, file_path, first_event_ts, last_event_ts,
					event_count, lines_attributed
				) VALUES (?, ?, ?, ?, ?, ?, ?)`,
				commitID, sessionID, file, fa.firstTS, fa.lastTS, len(fa.eventIDs), fa.lines); err != nil {
				return written, err
			}
			totalLines += fa.lines
			eventCount += len(fa.eventIDs)
			filesTouched++
		}
		confidence := "line_range_committed_intersect"
		if sm.usedFallback {
			confidence = "fallback_capped"
		}
		if _, err := db.Exec(`
			INSERT INTO commit_sessions (
				commit_id, session_id, agent_slug, model_slug, first_event_ts, last_event_ts,
				event_count, files_touched, lines_attributed, attribution_source, confidence
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			commitID, sessionID, sm.agent, sm.model, firstTS, lastTS,
			eventCount, filesTouched, totalLines, "committed_diff", confidence); err != nil {
			return written, err
		}
		written++
	}
	return written, nil
}

// commitFilesWithAdds returns file_path -> lines_added for files the commit
// added text to.
func (db *DB) commitFilesWithAdds(commitID string) (map[string]int, error) {
	rows, err := db.Query(`
		SELECT file_path, lines_added
		FROM commit_files
		WHERE commit_id = ? AND lines_added > 0`, commitID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var path string
		var added int
		if err := rows.Scan(&path, &added); err != nil {
			return nil, err
		}
		out[path] = added
	}
	return out, rows.Err()
}

// candidateRanges returns the AI line ranges that fall in a commit's attribution
// window for one file: edits on the same repo+file that happened before the
// commit, with no intervening commit touching the file (the next-file-commit
// boundary). Ordered ts-ascending so latest-edit-wins de-dup works downstream.
//
// carried are the recorded commits whose work this commit carries (see
// CarriedCommits). They do not close the window: an edit that the original of
// a rebase, an amend, a squash or a cherry-pick took is this commit's to take
// too.
func (db *DB) candidateRanges(repoID, file, committedAt string, carried []string) ([]candRange, error) {
	notCarried, carriedArgs := NotInClause("c2.id", carried)
	args := []any{repoID, file, committedAt, committedAt}
	args = append(args, carriedArgs...)
	rows, err := db.Query(`
		SELECT elr.event_id, e.session_id, e.agent_slug,
		       COALESCE(s.model_slug, json_extract(e.payload, '$.model')) AS model_slug,
		       e.ts, elr.start_line, elr.end_line, elr.new_text_lines
		FROM event_line_ranges elr
		JOIN events e ON e.id = elr.event_id
		LEFT JOIN sessions s ON s.id = e.session_id
		WHERE e.repo_id = ?
		  AND elr.rel_file_path = ?
		  AND e.session_id IS NOT NULL
		  AND datetime(e.ts) <= datetime(?)
		  AND NOT EXISTS (
			SELECT 1
			FROM commits c2
			JOIN commit_files cf2 ON cf2.commit_id = c2.id
			WHERE c2.repo_id = e.repo_id
			  AND cf2.file_path = elr.rel_file_path
			  AND datetime(c2.committed_at) >= datetime(e.ts)
			  AND datetime(c2.committed_at) < datetime(?)`+notCarried+`
		  )
		ORDER BY datetime(e.ts) ASC`,
		args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []candRange
	for rows.Next() {
		var c candRange
		var sess sql.NullString
		if err := rows.Scan(&c.eventID, &sess, &c.agentSlug, &c.modelSlug,
			&c.ts, &c.start, &c.end, &c.newLines); err != nil {
			return nil, err
		}
		if !sess.Valid {
			continue
		}
		c.sessionID = sess.String
		out = append(out, c)
	}
	return out, rows.Err()
}

var hunkHeaderRE = regexp.MustCompile(`@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// addedLineSet returns the set of line numbers (in the commit's coordinates)
// that the commit added to the file, parsed from `git show --unified=0`. The
// second return is false when git is unavailable so the caller can fall back.
func addedLineSet(root, sha, file string) (map[int]bool, bool) {
	if root == "" {
		return nil, false
	}
	out, err := gitx.Output(root, "show", "--unified=0", "--format=", "--no-color", sha, "--", file)
	if err != nil {
		return nil, false
	}
	set := map[int]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, "@@") {
			continue
		}
		m := hunkHeaderRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		start, _ := strconv.Atoi(m[1])
		count := 1
		if m[2] != "" {
			count, _ = strconv.Atoi(m[2])
		}
		for i := 0; i < count; i++ {
			set[start+i] = true
		}
	}
	return set, true
}

// maxCarriedCandidates and maxCarriedLines bound the one diff CarriedCommits
// reads: the newest commits that could be carried, and the lines they and the
// commit itself add to the files in question.
const (
	maxCarriedCandidates = 50
	maxCarriedLines      = 100000
)

// maxParentChain is how far back CarriedCommits follows a commit's recorded
// parents.
const maxParentChain = 500

// checkoutsOf is the subquery for the repository rows that are checkouts of
// one repository: the row itself (first argument) and every row that has its
// remote (second argument, the same id). Every working tree is a row of its
// own, so the worktrees of a repository, and separate clones of it, are
// several rows. A repository with no remote has only its own row.
const checkoutsOf = `(
	SELECT id FROM repositories
	WHERE id = ?
	   OR (COALESCE(remote_url, '') != ''
	       AND remote_url = (SELECT remote_url FROM repositories WHERE id = ?)))`

// CarriedCommits returns the ids of recorded commits whose work this commit
// carries: the originals of a rebase, an amend, a squash or a cherry-pick.
//
// The attribution window for a file closes at the previous recorded commit
// that touched it, judged by time. For a rewritten commit that is wrong: the
// commit it replaced closes the window, and the replacement finds every edit
// already taken and is attributed nothing. A carried commit therefore does
// not close the window, and once no branch holds it any more it gives its
// attribution up to this commit (GiveUpAttribution).
//
// A recorded commit is carried when all of these hold:
//
//   - it is in the same repository, was committed no later than this commit,
//     and adds lines to a file this commit adds lines to. The same
//     repository is this commit's row or another checkout of it (a row with
//     the same remote): a branch made in a worktree is often amended, rebased
//     or cherry-picked from the main checkout;
//   - it is not one of this commit's ancestors;
//   - more than half of the lines it adds to those files are lines this
//     commit adds too, to the same file. Lines are compared as text without
//     their surrounding whitespace, and a line with no letter or digit in it
//     is not counted.
//
// The last test is what tells a rewrite from a commit that only touches the
// same file: work on another branch, a branch merged by squash on the server
// and then deleted, a branch that was abandoned. Those close the window as
// any earlier commit does, and nothing of theirs is given up.
//
// In a history posthook recorded as it was made, the earlier commits on the
// same files are this commit's recorded parents, and the store alone settles
// that: no git call. Only earlier commits that are not on that chain cost
// one `git log -p`, and each one whose lines match costs a `git merge-base`.
// With no access to the repository the answer is none: every earlier commit
// closes the window.
func (db *DB) CarriedCommits(commitID, sha, repoID, committedAt, root string) []string {
	if root == "" || sha == "" {
		return nil
	}
	// "No later than", not "earlier than": a commit's time has one-second
	// resolution, and an amend often lands in the second of its original.
	rows, err := db.Query(`
		SELECT c2.id, c2.sha, CAST(strftime('%s', c2.committed_at) AS INTEGER),
		       SUM(cf2.lines_added)
		FROM commits c2
		JOIN commit_files cf2 ON cf2.commit_id = c2.id
		WHERE c2.repo_id IN `+checkoutsOf+`
		  AND c2.id != ?
		  AND datetime(c2.committed_at) <= datetime(?)
		  AND cf2.lines_added > 0
		  AND cf2.file_path IN (
			SELECT file_path FROM commit_files WHERE commit_id = ? AND lines_added > 0
		  )
		GROUP BY c2.id, c2.sha, c2.committed_at
		ORDER BY 3 DESC`, repoID, repoID, commitID, committedAt, commitID)
	if err != nil {
		return nil
	}
	type earlier struct {
		id, sha string
		adds    int
	}
	var earlierCommits []earlier
	for rows.Next() {
		var e earlier
		var at sql.NullInt64
		if err := rows.Scan(&e.id, &e.sha, &at, &e.adds); err != nil {
			rows.Close()
			return nil
		}
		earlierCommits = append(earlierCommits, e)
	}
	rows.Close()
	if len(earlierCommits) == 0 {
		return nil
	}

	// This commit's recorded first-parent chain. Whatever is on it is an
	// ancestor for certain; what is not may still be one (through a merge, or
	// behind a commit that was never recorded), which git settles below for
	// the few that get that far.
	onChain := map[string]bool{}
	rows, err = db.Query(`
		WITH RECURSIVE checkouts(id) AS `+checkoutsOf+`,
		chain(sha, depth) AS (
			SELECT parent_sha, 1 FROM commits WHERE id = ?
			UNION
			SELECT c.parent_sha, chain.depth + 1
			FROM chain JOIN commits c ON c.sha = chain.sha AND c.repo_id IN (SELECT id FROM checkouts)
			WHERE chain.depth < ?
		)
		SELECT sha FROM chain WHERE sha IS NOT NULL`, repoID, repoID, commitID, maxParentChain)
	if err != nil {
		return nil
	}
	for rows.Next() {
		var parent string
		if err := rows.Scan(&parent); err != nil {
			rows.Close()
			return nil
		}
		onChain[parent] = true
	}
	rows.Close()

	files, err := db.commitFilesWithAdds(commitID)
	if err != nil {
		return nil
	}
	budget := maxCarriedLines
	var paths []string
	for path, adds := range files {
		budget -= adds
		// A path git printed as a rename ("a => b") or with a newline in it
		// cannot be named to git here; such a file is left out of the
		// comparison.
		if !strings.Contains(path, " => ") && !strings.ContainsAny(path, "\n\x00") {
			paths = append(paths, path)
		}
	}
	// Off the chain, newest first, as many as the line budget allows.
	var candidates []earlier
	for _, e := range earlierCommits {
		if onChain[e.sha] {
			continue
		}
		if budget -= e.adds; budget < 0 || len(candidates) == maxCarriedCandidates {
			break
		}
		candidates = append(candidates, e)
	}
	if len(candidates) == 0 || len(paths) == 0 {
		return nil
	}

	// One diff of this commit and the candidates, limited to this commit's
	// files. Revisions and paths go in on stdin so their number is not
	// limited by the command line. A candidate from another checkout that
	// this one does not have the objects of (a separate clone) is skipped.
	var stdin strings.Builder
	stdin.WriteString(sha + "\n")
	for _, e := range candidates {
		stdin.WriteString(e.sha + "\n")
	}
	stdin.WriteString("--\n")
	for _, path := range paths {
		stdin.WriteString(path + "\n")
	}
	out, err := gitx.OutputWithInput(root, stdin.String(),
		"--literal-pathspecs", "log", "--ignore-missing", "--no-walk=unsorted", "--format=%x00%H",
		"-p", "--unified=0", "--no-color", "--no-ext-diff", "--no-textconv", "--no-renames",
		"--stdin")
	if err != nil {
		return nil
	}
	added := addedLinesByCommit(out)
	mine := added[sha]
	var carried []string
	for _, e := range candidates {
		if !carries(mine, added[e.sha]) {
			continue
		}
		// An ancestor whose lines this commit adds again (a revert that was
		// reverted) is history, not an original. If git cannot say, it is
		// left as any earlier commit.
		if ancestor, err := gitx.IsAncestor(root, e.sha, sha); err != nil || ancestor {
			continue
		}
		carried = append(carried, e.id)
	}
	return carried
}

// addedLine is one line a commit adds: the file it is added to, as the diff
// names it, and its text.
type addedLine struct{ file, text string }

// addedLinesByCommit reads the output of `git log --format=%x00%H -p
// --unified=0` and returns, per commit, the lines it adds to each file and
// how often: trimmed of surrounding whitespace, and without the lines that
// have no letter or digit in them (a lone brace, a blank line), which say
// nothing about whose work a commit carries.
func addedLinesByCommit(out []byte) map[string]map[addedLine]int {
	added := map[string]map[addedLine]int{}
	var current map[addedLine]int
	file := ""
	inHunk := false
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "\x00"):
			current = map[addedLine]int{}
			added[strings.TrimSpace(line[1:])] = current
			file, inHunk = "", false
		case strings.HasPrefix(line, "diff --git "):
			// Inside a hunk every line starts with "+", "-" or "\", so this
			// is the next file's header and not content. The header names
			// the file the same way in every commit of one output.
			file, inHunk = strings.TrimPrefix(line, "diff --git "), false
		case strings.HasPrefix(line, "@@"):
			inHunk = true
		case inHunk && current != nil && strings.HasPrefix(line, "+"):
			text := strings.TrimSpace(line[1:])
			if strings.IndexFunc(text, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) >= 0 {
				current[addedLine{file, text}]++
			}
		}
	}
	return added
}

// carries reports whether more than half of the lines theirs adds are lines
// mine adds to the same file, each line counted as often as it occurs. The
// same text in another file does not count: a rewrite puts a commit's lines
// back where they were.
func carries(mine, theirs map[addedLine]int) bool {
	total, shared := 0, 0
	for line, n := range theirs {
		total += n
		if m := mine[line]; m < n {
			shared += m
		} else {
			shared += n
		}
	}
	return total > 0 && shared*2 > total
}

// NotInClause renders " AND <column> NOT IN (?, ?, …)" and its arguments, or
// nothing for an empty list.
func NotInClause(column string, values []string) (string, []any) {
	if len(values) == 0 {
		return "", nil
	}
	args := make([]any, len(values))
	for i, v := range values {
		args[i] = v
	}
	return " AND " + column + " NOT IN (?" + strings.Repeat(", ?", len(values)-1) + ")", args
}

// InstalledAt is when this database was created: the first schema version's
// timestamp. Commits made before it were made before posthook was on the
// machine.
func (db *DB) InstalledAt() (time.Time, bool) {
	var v sql.NullString
	if err := db.QueryRow(`SELECT MIN(applied_at) FROM schema_version`).Scan(&v); err != nil || !v.Valid {
		return time.Time{}, false
	}
	t, err := time.Parse("2006-01-02 15:04:05", v.String)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// GiveUpAttribution takes from a retired commit the lines its replacement is
// attributed too: for every session and file the replacement holds lines for,
// the retired commit's count goes to zero, and its per-session totals follow.
// Used for a carried commit (CarriedCommits) that no branch holds any more;
// counting both would double the lines.
//
// It works per session and file, not per line. A file the rewrite dropped,
// or one where attribution by position found none of the session's lines on
// the replacement, stays with the retired commit. Where the replacement
// holds some of a session's lines in a file, the retired commit gives up all
// it had there: the lines the replacement was not credited cannot be told
// from lines the rewrite removed, and keeping the difference would count
// removed lines for good, once more for every further rewrite of the commit.
//
// The rows are kept at zero rather than deleted because the cloud landing
// zone has no tombstones. It reports whether anything changed.
func (db *DB) GiveUpAttribution(retiredID, replacementID string) (bool, error) {
	res, err := db.Exec(`
		UPDATE commit_session_files SET lines_attributed = 0, synced_at = NULL
		WHERE commit_id = ? AND lines_attributed != 0
		  AND EXISTS (
			SELECT 1 FROM commit_session_files n
			WHERE n.commit_id = ?
			  AND n.session_id = commit_session_files.session_id
			  AND n.file_path = commit_session_files.file_path
			  AND n.lines_attributed > 0
		  )`, retiredID, replacementID)
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return false, err
	}
	_, err = db.Exec(`
		UPDATE commit_sessions SET
		  lines_attributed = (
			SELECT COALESCE(SUM(f.lines_attributed), 0) FROM commit_session_files f
			WHERE f.commit_id = commit_sessions.commit_id AND f.session_id = commit_sessions.session_id),
		  files_touched = (
			SELECT COUNT(*) FROM commit_session_files f
			WHERE f.commit_id = commit_sessions.commit_id AND f.session_id = commit_sessions.session_id
			  AND f.lines_attributed > 0),
		  synced_at = NULL
		WHERE commit_id = ?
		  AND lines_attributed != (
			SELECT COALESCE(SUM(f.lines_attributed), 0) FROM commit_session_files f
			WHERE f.commit_id = commit_sessions.commit_id AND f.session_id = commit_sessions.session_id)`, retiredID)
	return true, err
}
