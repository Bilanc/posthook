package store

import (
	"database/sql"
	"regexp"
	"sort"
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

// commitRow is one recorded commit as attribution needs it.
type commitRow struct {
	id, sha, repoID, committedAt string
	// root is the working tree to ask git in. rowRoot is the checkout the
	// commit is recorded with, where the edits behind it were captured and
	// where the authorship map of its files is kept. They differ when a
	// commit is recorded from another checkout of its repository.
	root, rowRoot string
}

// carried is nil when it has to be worked out per commit; root is empty to
// use each commit's own repository row.
func (db *DB) refreshCommitAttributions(commitID, root string, carried []string) (int, error) {
	var commitArg any
	if commitID != "" {
		// What one commit is credited with is replaced in a single step
		// when its new attribution is written (writeAttribution).
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
	var commits []commitRow
	for rows.Next() {
		var c commitRow
		if err := rows.Scan(&c.id, &c.sha, &c.repoID, &c.committedAt, &c.rowRoot); err != nil {
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
		c.root = c.rowRoot
		if root != "" {
			c.root = root
		}
		n, err := db.attributeCommit(c, carried, commitID != "")
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
	// numbers and norms are the attributed lines themselves, in the commit's
	// line numbers, when they are known (commit_ai_lines). inherited is the
	// event count that came with lines inherited from a commit this one
	// replaces.
	numbers   []int
	norms     []uint64
	inherited int
}

func (f *fileAgg) touch(eventID, ts string) {
	if eventID != "" {
		if f.eventIDs == nil {
			f.eventIDs = map[string]bool{}
		}
		f.eventIDs[eventID] = true
	}
	if ts == "" {
		return
	}
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
	// byContent: at least one of the session's lines was matched by content.
	byContent bool
}

// attribution collects what one commit is credited with, by session and
// file, whichever way each file was matched.
type attribution struct {
	perSessionFile map[string]map[string]*fileAgg
	meta           map[string]*sessionMeta
	// byContent are the files matched by content in this pass. excluded are
	// the lines of those files that an ancestor commit has already counted.
	byContent map[string]bool
	excluded  map[string]map[int]bool
}

func newAttribution() *attribution {
	return &attribution{
		perSessionFile: map[string]map[string]*fileAgg{}, meta: map[string]*sessionMeta{},
		byContent: map[string]bool{}, excluded: map[string]map[int]bool{},
	}
}

// exclude notes that a line of file was counted in an ancestor commit.
func (a *attribution) exclude(file string, number int) {
	if a.excluded[file] == nil {
		a.excluded[file] = map[int]bool{}
	}
	a.excluded[file][number] = true
}

func (a *attribution) agg(sessionID, file string) *fileAgg {
	m := a.perSessionFile[sessionID]
	if m == nil {
		m = map[string]*fileAgg{}
		a.perSessionFile[sessionID] = m
	}
	fa := m[file]
	if fa == nil {
		fa = &fileAgg{}
		m[file] = fa
	}
	return fa
}

// session returns the session's entry, creating it and widening its time
// span to include ts.
func (a *attribution) session(sessionID, agent string, model sql.NullString, ts string) *sessionMeta {
	sm := a.meta[sessionID]
	if sm == nil {
		sm = &sessionMeta{agent: agent, model: model, firstTS: ts, lastTS: ts}
		a.meta[sessionID] = sm
	}
	if sm.agent == "" {
		sm.agent = agent
	}
	if !sm.model.Valid && model.Valid {
		sm.model = model
	}
	if ts != "" {
		if sm.firstTS == "" || ts < sm.firstTS {
			sm.firstTS = ts
		}
		if ts > sm.lastTS {
			sm.lastTS = ts
		}
	}
	return sm
}

// attributeCommit computes per-session AI attribution for one commit and
// writes commit_session_files, commit_sessions and commit_ai_lines rows.
// Returns the number of commit_sessions rows written.
//
// Each file the commit adds lines to is matched by content when it can be
// (contentattribution.go): the file has an authorship map, or a commit this
// one replaces has its attributed lines on record. Otherwise it is matched
// by line position, as every file was before schema v17: the file was last
// edited before the upgrade, or is too large to keep a map for.
func (db *DB) attributeCommit(c commitRow, carried []string, merge bool) (int, error) {
	files, err := db.commitFilesWithAdds(c.id)
	if err != nil {
		return 0, err
	}
	att := newAttribution()
	if len(files) == 0 {
		return db.writeAttribution(c.id, att, merge)
	}
	if carried == nil {
		carried = db.CarriedCommits(c.id, c.sha, c.repoID, c.committedAt, c.root)
	}

	// One read of the commit's diff serves every file that can be matched by
	// content. nil when there is none, when git cannot be asked, or for a
	// commit from before content matching.
	var adds map[string]*addedFile
	if c.root != "" && db.matchesByContent(c.committedAt) {
		adds = commitAdds(c.root, c.sha, db.contentFiles(c, files, carried))
	}
	content := &contentMatcher{db: db, c: c, carried: carried, ancestors: map[string]bool{}}
	var sources []EventSource
	for file, linesAdded := range files {
		af := adds[file]
		if af != nil && content.attributeFile(file, af, att) {
			att.byContent[file] = true
			continue
		}
		if sources == nil {
			sources = db.AttributionSources(c.repoID, c.committedAt, carried)
		}
		if err := db.attributeFileByPosition(c, file, linesAdded, af, sources, carried, att); err != nil {
			return 0, err
		}
	}
	return db.writeAttribution(c.id, att, merge)
}

// attributeFileByPosition is attribution as it was before the authorship
// map: an AI line range counts where the line number it covers is one the
// commit added, latest edit winning per line.
func (db *DB) attributeFileByPosition(c commitRow, file string, linesAdded int, af *addedFile, sources []EventSource, carried []string, att *attribution) error {
	var cands []candRange
	for _, src := range sources {
		more, err := db.candidateRanges(c.id, src.RepoID, file, src.Until, carried)
		if err != nil {
			return err
		}
		cands = append(cands, more...)
	}
	if len(sources) > 1 {
		// Latest edit wins per line, so the edits of several checkouts
		// have to be in one order of time.
		sort.SliceStable(cands, func(i, j int) bool { return eventTime(cands[i].ts).Before(eventTime(cands[j].ts)) })
	}
	if len(cands) == 0 {
		return nil
	}
	for _, cand := range cands {
		att.session(cand.sessionID, cand.agentSlug, cand.modelSlug, cand.ts)
	}

	var added map[int]bool
	haveHunks := false
	normAt := map[int]uint64{}
	if af != nil {
		added, haveHunks = make(map[int]bool, len(af.numbers)), true
		for k, n := range af.numbers {
			added[n] = true
			normAt[n] = af.norms[k]
		}
	} else {
		added, haveHunks = addedLineSet(c.root, c.sha, file)
	}
	if haveHunks {
		// Intersect AI ranges with the lines C actually added, de-duping
		// per line with latest-edit-wins (candidates arrive ts-ascending).
		owner := map[int]candRange{}
		for _, cand := range cands {
			for ln := cand.start; ln <= cand.end; ln++ {
				if added[ln] {
					owner[ln] = cand
				}
			}
		}
		lines := make([]int, 0, len(owner))
		for ln := range owner {
			lines = append(lines, ln)
		}
		sort.Ints(lines)
		for _, ln := range lines {
			cand := owner[ln]
			fa := att.agg(cand.sessionID, file)
			fa.lines++
			fa.touch(cand.eventID, cand.ts)
			if norm, known := normAt[ln]; known {
				fa.numbers = append(fa.numbers, ln)
				fa.norms = append(fa.norms, norm)
			}
		}
		return nil
	}
	// Fallback: no git access for hunks. Cap the per-file churn sum at
	// the recorded additions, split proportionally across sessions.
	raw := map[string]int{}
	rawTotal := 0
	for _, cand := range cands {
		raw[cand.sessionID] += cand.newLines
		rawTotal += cand.newLines
	}
	for _, cand := range cands {
		att.agg(cand.sessionID, file).touch(cand.eventID, cand.ts)
	}
	for sessionID, r := range raw {
		n := r
		if rawTotal > linesAdded && rawTotal > 0 {
			n = r * linesAdded / rawTotal
		}
		att.perSessionFile[sessionID][file].lines = n
		att.meta[sessionID].usedFallback = true
	}
	return nil
}

// contentConfidence is commit_sessions.confidence for a session whose lines
// in the commit were matched by content.
const contentConfidence = "line_content_match"

// previousSession is a session a commit was credited to before it is
// attributed again.
type previousSession struct {
	id, agent        string
	model            sql.NullString
	firstTS, lastTS  string
	source, confided string
}

// previousFile is one session's row for one file of a commit, as it stood.
type previousFile struct {
	firstTS, lastTS string
	events, lines   int
}

// queryer is what reading inside or outside a transaction have in common.
type queryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

// writeAttribution stores what a commit is credited with, replacing what was
// there in one transaction. Two processes attribute commits: the git shadow,
// the moment a commit is made, and the worker, when it learns something new
// about a recent one. Whichever writes last must not leave half of each.
//
// With merge, what the commit was credited before is carried over where this
// pass has nothing to say against it. A commit's content does not change,
// and neither does who wrote its lines; but the authorship map this pass
// read is the file as it is now, which may no longer hold lines the commit
// was rightly credited with when it was made (the agent has rewritten them
// since, the map has expired). So in a file matched by content, a line on
// record for the commit stays credited unless this pass credited that line
// itself or found it counted in an ancestor. It also means a pass that read
// the map a moment too early cannot undo one that read it later.
//
// Every session that was looked at gets a commit_sessions row, not just
// those that netted lines, and so does every session the commit was credited
// to before. The cloud landing zone is append-only with no tombstones:
// without a fresh row at zero, the earlier row would stay the latest one
// upstream.
func (db *DB) writeAttribution(commitID string, att *attribution, merge bool) (int, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var previous []previousSession
	if merge {
		// A write first, so the transaction holds the write lock before it
		// reads what it is about to replace.
		if _, err := tx.Exec(`UPDATE commits SET id = id WHERE id = ?`, commitID); err != nil {
			return 0, err
		}
		if previous, err = previousSessions(tx, commitID); err != nil {
			return 0, err
		}
		if err := carryRecordedLines(tx, commitID, att, previous); err != nil {
			return 0, err
		}
		for _, table := range []string{"commit_sessions", "commit_session_files", "commit_ai_lines"} {
			if _, err := tx.Exec(`DELETE FROM `+table+` WHERE commit_id = ?`, commitID); err != nil {
				return 0, err
			}
		}
	}

	recorded := map[string][]CommitLines{}
	written := 0
	for sessionID, sm := range att.meta {
		fileMap := att.perSessionFile[sessionID]
		var totalLines, eventCount, filesTouched int
		firstTS, lastTS := sm.firstTS, sm.lastTS
		for file, fa := range fileMap {
			if fa.lines <= 0 {
				continue
			}
			events := len(fa.eventIDs) + fa.inherited
			if _, err := tx.Exec(`
				INSERT INTO commit_session_files (
					commit_id, session_id, file_path, first_event_ts, last_event_ts,
					event_count, lines_attributed
				) VALUES (?, ?, ?, ?, ?, ?, ?)`,
				commitID, sessionID, file, fa.firstTS, fa.lastTS, events, fa.lines); err != nil {
				return written, err
			}
			if len(fa.numbers) == fa.lines {
				sortLines(fa)
				recorded[file] = append(recorded[file], CommitLines{SessionID: sessionID, Numbers: fa.numbers, Norms: fa.norms})
			}
			totalLines += fa.lines
			eventCount += events
			filesTouched++
		}
		source, confidence := "committed_diff", "line_range_committed_intersect"
		switch {
		case sm.usedFallback:
			confidence = "fallback_capped"
		case sm.byContent:
			source, confidence = "committed_content", contentConfidence
		}
		if _, err := tx.Exec(`
			INSERT INTO commit_sessions (
				commit_id, session_id, agent_slug, model_slug, first_event_ts, last_event_ts,
				event_count, files_touched, lines_attributed, attribution_source, confidence
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			commitID, sessionID, sm.agent, sm.model, firstTS, lastTS,
			eventCount, filesTouched, totalLines, source, confidence); err != nil {
			return written, err
		}
		written++
	}
	for _, p := range previous {
		if att.meta[p.id] != nil {
			continue
		}
		if _, err := tx.Exec(`
			INSERT INTO commit_sessions (
				commit_id, session_id, agent_slug, model_slug, first_event_ts, last_event_ts,
				event_count, files_touched, lines_attributed, attribution_source, confidence
			) VALUES (?, ?, ?, ?, ?, ?, 0, 0, 0, ?, ?)`,
			commitID, p.id, p.agent, p.model, p.firstTS, p.lastTS, p.source, p.confided); err != nil {
			return written, err
		}
		written++
	}
	for file, rows := range recorded {
		for _, r := range rows {
			if _, err := tx.Exec(`
				INSERT INTO commit_ai_lines (commit_id, file_path, session_id, lines, line_norms)
				VALUES (?, ?, ?, ?, ?)`,
				commitID, file, r.SessionID, FormatLineNumbers(r.Numbers), encodeHashes(r.Norms)); err != nil {
				return written, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return written, nil
}

func previousSessions(q queryer, commitID string) ([]previousSession, error) {
	rows, err := q.Query(`
		SELECT session_id, agent_slug, model_slug, first_event_ts, last_event_ts, attribution_source, confidence
		FROM commit_sessions WHERE commit_id = ?`, commitID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []previousSession
	for rows.Next() {
		var p previousSession
		if err := rows.Scan(&p.id, &p.agent, &p.model, &p.firstTS, &p.lastTS, &p.source, &p.confided); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// carryRecordedLines adds to att the lines the commit has on record that
// this pass neither credited nor found counted in an ancestor. Lines the
// commit gave up to a commit that replaced it (GiveUpAttribution, its file
// row at zero) are not carried.
func carryRecordedLines(q queryer, commitID string, att *attribution, previous []previousSession) error {
	sessions := map[string]previousSession{}
	for _, p := range previous {
		sessions[p.id] = p
	}
	files := map[[2]string]previousFile{}
	rows, err := q.Query(`
		SELECT session_id, file_path, first_event_ts, last_event_ts, event_count, lines_attributed
		FROM commit_session_files WHERE commit_id = ?`, commitID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var session, file string
		var f previousFile
		if err := rows.Scan(&session, &file, &f.firstTS, &f.lastTS, &f.events, &f.lines); err != nil {
			rows.Close()
			return err
		}
		files[[2]string{session, file}] = f
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// The lines this pass credited, per file, whoever to.
	credited := map[string]map[int]bool{}
	for _, fileMap := range att.perSessionFile {
		for file, fa := range fileMap {
			if credited[file] == nil {
				credited[file] = map[int]bool{}
			}
			for _, n := range fa.numbers {
				credited[file][n] = true
			}
		}
	}

	type record struct {
		file, session string
		numbers       []int
		norms         []uint64
	}
	var records []record
	rows, err = q.Query(`SELECT file_path, session_id, lines, line_norms FROM commit_ai_lines WHERE commit_id = ?`, commitID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var r record
		var lines string
		var norms []byte
		if err := rows.Scan(&r.file, &r.session, &lines, &norms); err != nil {
			rows.Close()
			return err
		}
		r.numbers, r.norms = ParseLineNumbers(lines), decodeHashes(norms)
		if len(r.numbers) == len(r.norms) {
			records = append(records, r)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// In a file this pass could only match by position, a record that came
	// from a match by content stands as it is, in place of what the
	// positions say: the pass ran without the file's map (the git shadow, a
	// moment ahead of the worker, or the map has expired), and positions are
	// the weaker evidence. A record that itself came from positions is
	// recomputed, as it always was.
	carried := func(r record) (previousFile, bool) {
		was, known := files[[2]string{r.session, r.file}]
		if !known || was.lines <= 0 {
			return was, false
		}
		return was, att.byContent[r.file] || sessions[r.session].confided == contentConfidence
	}
	for _, r := range records {
		if _, ok := carried(r); ok && !att.byContent[r.file] {
			for _, fileMap := range att.perSessionFile {
				delete(fileMap, r.file)
			}
			delete(credited, r.file)
		}
	}
	for _, r := range records {
		was, ok := carried(r)
		if !ok {
			continue
		}
		var fa *fileAgg
		for i, n := range r.numbers {
			if credited[r.file][n] || att.excluded[r.file][n] {
				continue
			}
			if fa == nil {
				p := sessions[r.session]
				sm := att.session(r.session, p.agent, p.model, was.firstTS)
				att.session(r.session, p.agent, p.model, was.lastTS)
				sm.byContent = true
				fa = att.agg(r.session, r.file)
				if fa.lines == 0 {
					fa.inherited += max(was.events, 1)
				}
				fa.touch("", was.firstTS)
				fa.touch("", was.lastTS)
			}
			fa.lines++
			fa.numbers = append(fa.numbers, n)
			fa.norms = append(fa.norms, r.norms[i])
		}
	}
	return nil
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
// too. Neither does a commit that has given the session's lines in the file
// up (ClosesWindow). commitID is the commit being attributed.
func (db *DB) candidateRanges(commitID, repoID, file, committedAt string, carried []string) ([]candRange, error) {
	notCarried, carriedArgs := NotInClause("c2.id", carried)
	args := []any{repoID, file, committedAt, committedAt}
	args = append(args, carriedArgs...)
	args = append(args, commitID)
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
			  AND `+ClosesWindow+`
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

// hunkHeaderCountsRE also captures the old side's line count.
var hunkHeaderCountsRE = regexp.MustCompile(`^@@ -\d+(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

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

// ClosesWindow is the SQL test, on an event e, a commit c2 and its file row
// cf2, that the commit closes the attribution window for that event. Its one
// argument is the id of the commit being attributed.
//
// An earlier commit on the file closes the window, except for a session
// whose lines in that file it has given up (GiveUpAttribution: its row for
// that session and file is at zero) once another commit in the same checkout
// has attribution rows for that session and file. That other commit is the
// replacement it gave the lines to, or a rewrite of that replacement, and it
// closes the window in its place. When the replacement is rewritten in turn,
// the next commit has to see past the first original too, and by then it
// cannot be compared any more: git drops a commit nothing refers to.
//
// The test is per session because giving up is: a rewrite can keep one
// session's lines in a file and drop another's, and the original then still
// stands in front of the edits whose lines it kept.
//
// A retired commit whose replacement is recorded with another checkout (a
// squash of commits from several worktrees) keeps closing the window in its
// own. So does a commit with no attribution row for the session and file:
// one made by hand closes the window like any other.
const ClosesWindow = `NOT (
	EXISTS (
		SELECT 1 FROM commit_session_files g
		WHERE g.commit_id = c2.id AND g.file_path = cf2.file_path
		  AND g.session_id = e.session_id AND g.lines_attributed = 0)
	AND EXISTS (
		SELECT 1 FROM commits c3
		JOIN commit_session_files g3 ON g3.commit_id = c3.id
		WHERE c3.repo_id = c2.repo_id
		  AND c3.id != c2.id
		  AND c3.id != ?
		  AND g3.file_path = cf2.file_path
		  AND g3.session_id = e.session_id
		  AND datetime(c3.committed_at) >= datetime(c2.committed_at)))`

// EventSource is one place a commit's edits were captured: a repository row,
// and the time up to which its edits count.
type EventSource struct{ RepoID, Until string }

// AttributionSources returns where to look for the edits of a commit: its own
// repository row, up to the commit, and the row of every commit it carries
// that was recorded in another checkout, up to that commit. A squash made in
// one checkout can carry commits from several worktrees, and each worktree
// holds only its own edits.
func (db *DB) AttributionSources(repoID, committedAt string, carried []string) []EventSource {
	sources := []EventSource{{repoID, committedAt}}
	seen := map[string]bool{repoID: true}
	// carried is newest first, so the first commit met in a row is its latest.
	for _, id := range carried {
		var row, at string
		if err := db.QueryRow(`SELECT repo_id, committed_at FROM commits WHERE id = ?`, id).Scan(&row, &at); err != nil {
			continue
		}
		if !seen[row] {
			seen[row] = true
			sources = append(sources, EventSource{row, at})
		}
	}
	return sources
}

// eventTime parses an event timestamp; one that does not parse sorts first.
func eventTime(ts string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return time.Time{}
	}
	return t
}

// CheckoutsOf is the subquery for the repository rows that are checkouts of
// one repository: the row itself (first argument) and every row that has its
// remote (second argument, the same id). Every working tree is a row of its
// own, so the worktrees of a repository, and separate clones of it, are
// several rows. A repository with no remote has only its own row.
const CheckoutsOf = `(
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
		WHERE c2.repo_id IN `+CheckoutsOf+`
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
		WITH RECURSIVE checkouts(id) AS `+CheckoutsOf+`,
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
	// Both tables change together or not at all, and the per-session totals
	// are brought in line with the file rows every time, not only when a
	// file row changed in this call: totals left behind by an earlier call
	// that was cut short would otherwise stay for good.
	tx, err := db.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`
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
	files, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	// The lines it has on record there go with them: they are the
	// replacement's now, and attributing this commit again must not bring
	// them back (writeAttribution).
	if _, err := tx.Exec(`
		DELETE FROM commit_ai_lines
		WHERE commit_id = ?
		  AND EXISTS (
			SELECT 1 FROM commit_session_files f
			WHERE f.commit_id = commit_ai_lines.commit_id
			  AND f.session_id = commit_ai_lines.session_id
			  AND f.file_path = commit_ai_lines.file_path
			  AND f.lines_attributed = 0
		  )`, retiredID); err != nil {
		return false, err
	}
	res, err = tx.Exec(`
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
		  AND EXISTS (
			SELECT 1 FROM commit_session_files f
			WHERE f.commit_id = commit_sessions.commit_id AND f.session_id = commit_sessions.session_id)
		  AND lines_attributed != (
			SELECT COALESCE(SUM(f.lines_attributed), 0) FROM commit_session_files f
			WHERE f.commit_id = commit_sessions.commit_id AND f.session_id = commit_sessions.session_id)`, retiredID)
	if err != nil {
		return false, err
	}
	sessions, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return files > 0 || sessions > 0, nil
}
