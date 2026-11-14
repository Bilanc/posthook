package store

import (
	"sort"
	"strconv"
	"strings"
)

// commit_ai_lines: which lines of a commit were attributed, and to which
// session.
//
// commit_session_files says how many lines of a file a session is credited
// with in a commit. This says which: their numbers in the committed file,
// and the whitespace-insensitive fingerprint of each. Two things need that.
// A commit that rewrites this one (a rebase, an amend, a squash, a
// cherry-pick) can inherit the lines by content when the working tree no
// longer holds them, or never did because the rewrite was made in another
// checkout. And the git note and `posthook blame` can name the exact lines.
//
// It holds fingerprints of file content, so it is not in SyncableTables.

// CommitLines is one session's attributed lines in one file of a commit.
type CommitLines struct {
	SessionID string
	// Numbers are 1-based line numbers in the committed file, ascending.
	// Norms[i] is the fingerprint of line Numbers[i].
	Numbers []int
	Norms   []uint64
}

// SaveCommitLines replaces what is recorded for one file of a commit.
func (db *DB) SaveCommitLines(commitID, file string, rows []CommitLines) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM commit_ai_lines WHERE commit_id = ? AND file_path = ?`, commitID, file); err != nil {
		return err
	}
	for _, r := range rows {
		if len(r.Numbers) == 0 || len(r.Numbers) != len(r.Norms) {
			continue
		}
		if _, err := tx.Exec(`
			INSERT INTO commit_ai_lines (commit_id, file_path, session_id, lines, line_norms)
			VALUES (?, ?, ?, ?, ?)`,
			commitID, file, r.SessionID, FormatLineNumbers(r.Numbers), encodeHashes(r.Norms)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteCommitLines forgets every recorded line of a commit.
func (db *DB) DeleteCommitLines(commitID string) error {
	_, err := db.Exec(`DELETE FROM commit_ai_lines WHERE commit_id = ?`, commitID)
	return err
}

// CommitLines returns what is recorded for one file of a commit, one entry
// per session.
func (db *DB) CommitLines(commitID, file string) []CommitLines {
	rows, err := db.Query(`
		SELECT session_id, lines, line_norms FROM commit_ai_lines
		WHERE commit_id = ? AND file_path = ? ORDER BY session_id`, commitID, file)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []CommitLines
	for rows.Next() {
		var r CommitLines
		var lines string
		var norms []byte
		if rows.Scan(&r.SessionID, &lines, &norms) != nil {
			continue
		}
		r.Numbers = ParseLineNumbers(lines)
		r.Norms = decodeHashes(norms)
		if len(r.Numbers) == 0 || len(r.Numbers) != len(r.Norms) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// CommitLineRecord is one session's credited lines in one file of a commit,
// with what a git note says about them.
type CommitLineRecord struct {
	File, SessionID, Agent, Model string
	// TS is when the session last edited the file before the commit.
	TS      string
	Numbers []int
}

// CommitLineRecords returns the lines a commit is credited with, by file and
// session. Lines the commit has since given up to a commit that replaced it
// (GiveUpAttribution) are not among them.
func (db *DB) CommitLineRecords(commitID string) []CommitLineRecord {
	rows, err := db.Query(`
		SELECT l.file_path, l.session_id, l.lines,
		       COALESCE(cs.agent_slug, ''), COALESCE(cs.model_slug, ''),
		       COALESCE(f.last_event_ts, cs.last_event_ts, '')
		FROM commit_ai_lines l
		JOIN commit_session_files f
		  ON f.commit_id = l.commit_id AND f.session_id = l.session_id AND f.file_path = l.file_path
		LEFT JOIN commit_sessions cs ON cs.commit_id = l.commit_id AND cs.session_id = l.session_id
		WHERE l.commit_id = ? AND f.lines_attributed > 0
		ORDER BY l.file_path, l.session_id`, commitID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []CommitLineRecord
	for rows.Next() {
		var r CommitLineRecord
		var lines string
		if rows.Scan(&r.File, &r.SessionID, &lines, &r.Agent, &r.Model, &r.TS) != nil {
			continue
		}
		if r.Numbers = ParseLineNumbers(lines); len(r.Numbers) > 0 {
			out = append(out, r)
		}
	}
	return out
}

// LineRuns groups ascending line numbers into runs of consecutive lines,
// each as its first and last line.
func LineRuns(numbers []int) [][2]int {
	var runs [][2]int
	for i := 0; i < len(numbers); {
		j := i
		for j+1 < len(numbers) && numbers[j+1] == numbers[j]+1 {
			j++
		}
		runs = append(runs, [2]int{numbers[i], numbers[j]})
		i = j + 1
	}
	return runs
}

// FormatLineNumbers renders ascending line numbers as ranges: "3-9,14".
func FormatLineNumbers(numbers []int) string {
	var b strings.Builder
	for i := 0; i < len(numbers); {
		j := i
		for j+1 < len(numbers) && numbers[j+1] == numbers[j]+1 {
			j++
		}
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Itoa(numbers[i]))
		if j > i {
			b.WriteByte('-')
			b.WriteString(strconv.Itoa(numbers[j]))
		}
		i = j + 1
	}
	return b.String()
}

// ParseLineNumbers reads what FormatLineNumbers wrote. A part that does not
// parse is skipped.
func ParseLineNumbers(spec string) []int {
	var out []int
	for _, part := range strings.Split(spec, ",") {
		first, last, isRange := strings.Cut(strings.TrimSpace(part), "-")
		start, err := strconv.Atoi(first)
		if err != nil || start < 1 {
			continue
		}
		end := start
		if isRange {
			if end, err = strconv.Atoi(last); err != nil || end < start {
				continue
			}
		}
		for n := start; n <= end; n++ {
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out
}
