package store

import (
	"database/sql"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bilanc/posthook/internal/gitx"
	"github.com/bilanc/posthook/internal/lineranges"
)

// Attribution by content.
//
// A line a commit adds is credited to an agent session when it is a line
// that session wrote, wherever it now sits and whatever was written around
// it since. Two records say which lines those are:
//
//   - the file's authorship map (filemap.go): the file as posthook last saw
//     it in the checkout the commit belongs to, with an author per line. It
//     is lined up with the committed file, and an added line that lines up
//     with a line that has an author is that author's. A line that was
//     changed, or inserted by hand, lines up with nothing and is credited to
//     nobody. This covers the ordinary commit, and a rewrite made in the
//     same checkout: a rebased, amended, squashed or cherry-picked commit
//     adds the same lines, and the map still holds them.
//   - the lines on record for the commits this one carries (CarriedCommits,
//     commitlines.go). They are what a rewrite inherits when the map no
//     longer holds the lines, or never did: a rewrite made from another
//     checkout, an original whose working tree has moved on.
//
// A map line remembers the commit it was last counted in. When a commit adds
// such a line again and that commit is one of its ancestors, the line has
// been counted in this history already and is not counted twice (a revert
// that is reverted). When that commit is not an ancestor it is the commit
// this one replaces, or work on another branch, and the line counts here.
//
// A file is matched by position instead (attributeFileByPosition) when
// neither record exists for it: it was last edited before schema v17, or it
// is too large to keep a map for. So is every commit made before v17 was
// applied, whenever it is looked at again.

// addedFile is what a commit adds to one file.
type addedFile struct {
	// blob is the id of the file's content in the commit.
	blob string
	// numbers are the 1-based numbers of the added lines in the committed
	// file, ascending; norms[i] is the whitespace-insensitive fingerprint of
	// line numbers[i].
	numbers []int
	norms   []uint64
}

// matchesByContent reports whether a commit made at committedAt is matched
// by content: it was made no earlier than this database began to.
func (db *DB) matchesByContent(committedAt string) bool {
	since, ok := db.ContentAttributionSince()
	if !ok {
		return false
	}
	at, err := time.Parse(time.RFC3339, committedAt)
	if err != nil {
		return false
	}
	return !at.Before(since)
}

// commitAdds reads the lines a commit adds to the named files, in one git
// call. nil when git cannot be asked or no file is named. A file whose path
// git prints quoted (a control character or a double quote in its name) is
// left out, and is matched by position.
//
// The a/ and b/ prefixes are asked for by name: a user's diff.noprefix or
// diff.dstPrefix would otherwise change the paths this reads.
//
// Only files that can be matched by content are asked for: a commit that
// vendors a dependency adds a great many lines no record says anything
// about, and its whole diff is not worth reading. Revision and paths go in
// on stdin, so their number is not limited by the command line.
func commitAdds(root, sha string, files []string) map[string]*addedFile {
	if root == "" || sha == "" || len(files) == 0 {
		return nil
	}
	var stdin strings.Builder
	stdin.WriteString(sha + "\n--\n")
	named := 0
	for _, file := range files {
		// A path git printed as a rename ("a => b") or with a newline in it
		// cannot be named to git here.
		if strings.Contains(file, " => ") || strings.ContainsAny(file, "\n\x00") {
			continue
		}
		stdin.WriteString(file + "\n")
		named++
	}
	if named == 0 {
		return nil
	}
	out, err := gitx.OutputWithInput(root, stdin.String(),
		"--literal-pathspecs", "-c", "core.quotePath=false", "log", "--no-walk=unsorted",
		"--format=", "-p", "--no-color", "--no-ext-diff", "--no-textconv", "--no-renames",
		"--src-prefix=a/", "--dst-prefix=b/", "--unified=0", "--full-index", "--stdin")
	if err != nil {
		return nil
	}
	return parseCommitAdds(string(out))
}

// contentFiles returns the files of a commit that can be matched by
// content: those that have an authorship map in the checkout the commit is
// recorded with, and those a commit it carries has lines on record for.
func (db *DB) contentFiles(c commitRow, files map[string]int, carried []string) []string {
	want := map[string]bool{}
	if c.rowRoot != "" {
		for file := range files {
			var one int
			if db.QueryRow(`SELECT 1 FROM file_snapshots WHERE file_path = ? AND line_owners IS NOT NULL`,
				filepath.Join(c.rowRoot, filepath.FromSlash(file))).Scan(&one) == nil {
				want[file] = true
			}
		}
	}
	if len(carried) > 0 {
		in, args := NotInClause("commit_id", carried)
		in = strings.Replace(in, " NOT IN ", " IN ", 1)
		rows, err := db.Query(`SELECT DISTINCT file_path FROM commit_ai_lines WHERE 1 = 1`+in, args...)
		if err == nil {
			for rows.Next() {
				var file string
				if rows.Scan(&file) == nil {
					if _, added := files[file]; added {
						want[file] = true
					}
				}
			}
			rows.Close()
		}
	}
	out := make([]string, 0, len(want))
	for file := range want {
		out = append(out, file)
	}
	sort.Strings(out)
	return out
}

// parseCommitAdds reads a unified diff with no context lines.
func parseCommitAdds(diff string) map[string]*addedFile {
	files := map[string]*addedFile{}
	var cur *addedFile
	blob := ""
	oldLeft, newLeft, lineNo := 0, 0, 0
	for _, line := range strings.Split(diff, "\n") {
		if oldLeft > 0 || newLeft > 0 {
			// Inside a hunk the header said how many lines of each side
			// follow, so a line of content that reads like a header is not
			// taken for one.
			switch {
			case newLeft > 0 && strings.HasPrefix(line, "+"):
				if cur != nil {
					cur.numbers = append(cur.numbers, lineNo)
					cur.norms = append(cur.norms, NormHash(line[1:]))
				}
				lineNo++
				newLeft--
				continue
			case oldLeft > 0 && strings.HasPrefix(line, "-"):
				oldLeft--
				continue
			case strings.HasPrefix(line, "\\"):
				continue
			}
			oldLeft, newLeft = 0, 0
		}
		switch {
		case strings.HasPrefix(line, "diff --git "):
			cur, blob = nil, ""
		case strings.HasPrefix(line, "index "):
			if _, after, ok := strings.Cut(strings.TrimPrefix(line, "index "), ".."); ok {
				blob, _, _ = strings.Cut(after, " ")
			}
		case strings.HasPrefix(line, "+++ "):
			cur = nil
			path := strings.TrimSuffix(strings.TrimPrefix(line, "+++ "), "\t")
			if path == "/dev/null" || strings.HasPrefix(path, `"`) {
				continue
			}
			cur = &addedFile{blob: blob}
			files[strings.TrimPrefix(path, "b/")] = cur
		case strings.HasPrefix(line, "@@ "):
			oldLeft, newLeft, lineNo = parseHunkHeader(line)
		case strings.HasPrefix(line, "\\"):
			// "\ No newline at end of file" after the last line of a hunk.
		}
	}
	return files
}

// parseHunkHeader reads "@@ -a,b +c,d @@": the number of old lines and of new
// lines that follow, and the number of the first new line.
func parseHunkHeader(line string) (oldCount, newCount, newStart int) {
	m := hunkHeaderCountsRE.FindStringSubmatch(line)
	if m == nil {
		return 0, 0, 0
	}
	oldCount, newCount = 1, 1
	if m[1] != "" {
		oldCount, _ = strconv.Atoi(m[1])
	}
	newStart, _ = strconv.Atoi(m[2])
	if m[3] != "" {
		newCount, _ = strconv.Atoi(m[3])
	}
	return oldCount, newCount, newStart
}

// contentMatcher attributes the files of one commit by content.
type contentMatcher struct {
	db      *DB
	c       commitRow
	carried []string
	// ancestors remembers, per commit sha, whether it is an ancestor of c.
	ancestors map[string]bool
}

// attributeFile credits the lines c adds to file by content and reports
// whether it could. False means neither record exists for the file, and it
// is left to be matched by position.
func (x *contentMatcher) attributeFile(file string, af *addedFile, att *attribution) bool {
	db := x.db
	path := ""
	var m FileMap
	haveMap := false
	if x.c.rowRoot != "" {
		path = filepath.Join(x.c.rowRoot, filepath.FromSlash(file))
		if got, ok := db.FileMap(path); ok && got.HasAuthors() {
			m, haveMap = got, true
		}
	}

	type original struct {
		commitID string
		rows     []CommitLines
	}
	var originals []original
	for _, id := range x.carried {
		rows := db.CommitLines(id, file)
		if len(rows) > 0 {
			originals = append(originals, original{id, rows})
			continue
		}
		if db.creditedWithoutLines(id, file) {
			// A commit this one replaces was credited in this file by
			// position, with no lines on record to inherit: one from before
			// v17. Its replacement is matched the same way.
			return false
		}
	}
	if !haveMap && len(originals) == 0 {
		return false
	}

	settled := make([]bool, len(af.numbers))
	if haveMap {
		// lined is the map in the committed file's line numbers; from says
		// which line of the stored map each of its lines is.
		lined, from := m, []int(nil)
		if m.Blob == "" || m.Blob != af.blob {
			content, err := gitx.Output(x.c.root, "cat-file", "blob", af.blob)
			if err != nil {
				if len(originals) == 0 {
					return false
				}
				haveMap = false
			} else {
				lined, from = m.Carry(lineranges.Lines(string(content)))
			}
		}
		if haveMap {
			marked := false
			for k, n := range af.numbers {
				author, ok := lined.AuthorOf(n - 1)
				if !ok {
					continue
				}
				settled[k] = true
				if counted := lined.CommitOf(n - 1); counted != "" && counted != x.c.sha && x.isAncestor(counted) {
					att.exclude(file, n)
					continue
				}
				x.credit(att, file, n, af.norms[k], author)
				at := n - 1
				if from != nil {
					at = from[n-1]
				}
				if at >= 0 && m.CommitOf(at) != x.c.sha {
					m.SetCommit(at, x.c.sha)
					marked = true
				}
			}
			if marked {
				// Best effort: if the worker has rewritten the row since, its
				// lines carry the marks they had.
				_, _ = db.UpdateFileMapMarks(path, m)
			}
		}
	}

	for _, orig := range originals {
		var rest []int
		for k := range af.numbers {
			if !settled[k] {
				rest = append(rest, k)
			}
		}
		if len(rest) == 0 {
			break
		}
		type line struct {
			number  int
			norm    uint64
			session string
		}
		var theirs []line
		for _, r := range orig.rows {
			for i, n := range r.Numbers {
				theirs = append(theirs, line{n, r.Norms[i], r.SessionID})
			}
		}
		sort.Slice(theirs, func(i, j int) bool { return theirs[i].number < theirs[j].number })
		a := make([]uint64, len(theirs))
		for i, l := range theirs {
			a[i] = l.norm
		}
		b := make([]uint64, len(rest))
		for i, k := range rest {
			b[i] = af.norms[k]
		}
		inherited := map[string]bool{}
		for i, j := range lineranges.AlignHashes(a, b) {
			if j < 0 {
				continue
			}
			k := rest[j]
			settled[k] = true
			session := theirs[i].session
			fa := att.agg(session, file)
			fa.lines++
			fa.numbers = append(fa.numbers, af.numbers[k])
			fa.norms = append(fa.norms, af.norms[k])
			if !inherited[session] {
				inherited[session] = true
				x.inherit(att, fa, orig.commitID, session, file)
			}
		}
	}
	return true
}

// credit gives one added line to the session of the event that wrote it.
func (x *contentMatcher) credit(att *attribution, file string, number int, norm uint64, author LineAuthor) {
	sm := att.meta[author.SessionID]
	if sm == nil {
		sm = att.session(author.SessionID, author.Agent, x.db.sessionModel(author.SessionID, author.EventID), author.TS)
		if sm.agent == "" {
			sm.agent = x.db.sessionAgent(author.SessionID)
		}
	} else {
		att.session(author.SessionID, author.Agent, sql.NullString{}, author.TS)
	}
	sm.byContent = true
	fa := att.agg(author.SessionID, file)
	fa.lines++
	fa.numbers = append(fa.numbers, number)
	fa.norms = append(fa.norms, norm)
	fa.touch(author.EventID, author.TS)
}

// inherit carries over what a replaced commit recorded about a session's
// work in a file: the agent and model, the span of the edits and how many
// there were.
func (x *contentMatcher) inherit(att *attribution, fa *fileAgg, originalID, sessionID, file string) {
	var agent string
	var model sql.NullString
	var first, last sql.NullString
	var events sql.NullInt64
	_ = x.db.QueryRow(`
		SELECT cs.agent_slug, cs.model_slug, f.first_event_ts, f.last_event_ts, f.event_count
		FROM commit_sessions cs
		LEFT JOIN commit_session_files f
		  ON f.commit_id = cs.commit_id AND f.session_id = cs.session_id AND f.file_path = ?
		WHERE cs.commit_id = ? AND cs.session_id = ?`, file, originalID, sessionID).Scan(&agent, &model, &first, &last, &events)
	if agent == "" {
		agent = x.db.sessionAgent(sessionID)
	}
	if !model.Valid {
		model = x.db.sessionModel(sessionID, "")
	}
	sm := att.session(sessionID, agent, model, first.String)
	att.session(sessionID, agent, model, last.String)
	sm.byContent = true
	fa.touch("", first.String)
	fa.touch("", last.String)
	if events.Valid && events.Int64 > 0 {
		fa.inherited += int(events.Int64)
	} else {
		fa.inherited++
	}
}

// isAncestor reports whether the commit sha is an ancestor of the commit
// being attributed. When git cannot say (the commit is gone) it is not: a
// commit nothing refers to any more is one that was replaced.
func (x *contentMatcher) isAncestor(sha string) bool {
	if known, ok := x.ancestors[sha]; ok {
		return known
	}
	ancestor, err := gitx.IsAncestor(x.c.root, sha, x.c.sha)
	x.ancestors[sha] = err == nil && ancestor
	return x.ancestors[sha]
}

// creditedWithoutLines reports whether a commit is credited lines in a file
// and has none on record for it. A commit that gave its lines in the file up
// is credited none there, and has no record of them either.
func (db *DB) creditedWithoutLines(commitID, file string) bool {
	var one int
	if err := db.QueryRow(`
		SELECT 1 FROM commit_session_files f
		WHERE f.commit_id = ? AND f.file_path = ? AND f.lines_attributed > 0
		  AND NOT EXISTS (SELECT 1 FROM commit_ai_lines l WHERE l.commit_id = f.commit_id AND l.file_path = f.file_path)
		LIMIT 1`, commitID, file).Scan(&one); err != nil {
		return false
	}
	return true
}

// sessionModel is the model a session ran, from the session row or, failing
// that, from the payload of one of its events.
func (db *DB) sessionModel(sessionID, eventID string) sql.NullString {
	var model sql.NullString
	_ = db.QueryRow(`SELECT model_slug FROM sessions WHERE id = ?`, sessionID).Scan(&model)
	if !model.Valid && eventID != "" {
		_ = db.QueryRow(`SELECT json_extract(payload, '$.model') FROM events WHERE id = ?`, eventID).Scan(&model)
	}
	return model
}

func (db *DB) sessionAgent(sessionID string) string {
	var agent sql.NullString
	_ = db.QueryRow(`SELECT agent_slug FROM sessions WHERE id = ?`, sessionID).Scan(&agent)
	return agent.String
}

// sortLines puts a file's attributed lines in ascending order; lines taken
// from the map and lines inherited from an original arrive in two passes.
func sortLines(fa *fileAgg) {
	order := make([]int, len(fa.numbers))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool { return fa.numbers[order[i]] < fa.numbers[order[j]] })
	numbers := make([]int, len(order))
	norms := make([]uint64, len(order))
	for i, at := range order {
		numbers[i], norms[i] = fa.numbers[at], fa.norms[at]
	}
	fa.numbers, fa.norms = numbers, norms
}

// AuthoredRange is a line range an agent event recorded, with its author.
type AuthoredRange struct {
	Start, End int
	Author     LineAuthor
}

// UncommittedRanges returns the line ranges recorded for a file since the
// last commit that touched it, oldest first: the edits attribution by
// position would offer the file's next commit. root is the checkout the file
// is in and rel its path there.
func (db *DB) UncommittedRanges(root, rel string) []AuthoredRange {
	var repoID string
	if err := db.QueryRow(`SELECT id FROM repositories WHERE root_path = ?`, root).Scan(&repoID); err != nil {
		return nil
	}
	cands, err := db.candidateRanges("", repoID, rel, time.Now().UTC().Format(time.RFC3339Nano), nil)
	if err != nil {
		return nil
	}
	out := make([]AuthoredRange, 0, len(cands))
	for _, c := range cands {
		out = append(out, AuthoredRange{Start: c.start, End: c.end, Author: LineAuthor{
			EventID: c.eventID, SessionID: c.sessionID, Agent: c.agentSlug, TS: c.ts,
		}})
	}
	return out
}
