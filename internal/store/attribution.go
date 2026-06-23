package store

import (
	"database/sql"
	"regexp"
	"strconv"
	"strings"

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
		n, err := db.attributeCommit(c.id, c.sha, c.repoID, c.committedAt, c.root)
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
func (db *DB) attributeCommit(commitID, sha, repoID, committedAt, root string) (int, error) {
	files, err := db.commitFilesWithAdds(commitID)
	if err != nil {
		return 0, err
	}
	if len(files) == 0 {
		return 0, nil
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
		cands, err := db.candidateRanges(repoID, file, committedAt)
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
func (db *DB) candidateRanges(repoID, file, committedAt string) ([]candRange, error) {
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
			  AND datetime(c2.committed_at) < datetime(?)
		  )
		ORDER BY datetime(e.ts) ASC`,
		repoID, file, committedAt, committedAt)
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
