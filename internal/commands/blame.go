package commands

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/bilanc/posthook/internal/gitx"
	"github.com/bilanc/posthook/internal/logx"
	"github.com/bilanc/posthook/internal/notes"
	"github.com/bilanc/posthook/internal/paths"
	"github.com/bilanc/posthook/internal/store"
	"github.com/bilanc/posthook/internal/transcript"

	"github.com/spf13/cobra"
)

const zeroSha = "0000000000000000000000000000000000000000"

func newBlameCmd() *cobra.Command {
	var colorMode string
	cmd := &cobra.Command{
		Use:   "blame <file>",
		Short: "Show per-line AI attribution for a file",
		Long: `Show per-line AI attribution for a file.

Every line is tagged human (from git blame), uncommitted, or AI — with the
model, time and session that wrote it, and the prompt that produced it shown
above the first line of each edit. Lines from the same agent session share a
colour. The footer lists each session with a link into the local dashboard
when it is running.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBlame(args[0], colorMode)
		},
	}
	cmd.Flags().StringVar(&colorMode, "color", "auto", "Colour output: auto (only on a terminal, honours NO_COLOR), always, never")
	return cmd
}

type blameLine struct {
	sha        string
	origLine   int
	finalLine  int
	content    string
	author     string
	authorTime int
	summary    string
}

type matchedRange struct {
	eventID   string
	sessionID sql.NullString
	agentSlug string
	modelSlug sql.NullString
	eventTS   string
	startLine int
	endLine   int
}

type rangeRow struct {
	eventID   string
	sessionID sql.NullString
	agentSlug string
	modelSlug sql.NullString
	eventTS   string
	startLine int
	endLine   int
	commitSHA sql.NullString
}

func runBlame(file, colorMode string) error {
	rawAbs, err := filepath.Abs(file)
	if err != nil {
		return err
	}
	if _, err := os.Stat(rawAbs); err != nil {
		return fmt.Errorf("file not found: %s", rawAbs)
	}
	absPath := rawAbs
	if real, err := filepath.EvalSymlinks(rawAbs); err == nil {
		absPath = real
	}
	repoRoot := gitx.FindRepoRoot(absPath)
	if repoRoot == "" {
		return fmt.Errorf("not inside a git repo: %s", absPath)
	}
	relPath := gitx.RelPathInRepo(repoRoot, absPath)
	if relPath == "" {
		return errors.New("file is outside repo root")
	}

	cmd := exec.Command("git", "blame", "--porcelain", relPath)
	cmd.Dir = repoRoot
	cmd.Env = gitx.BypassEnv()
	out, err := cmd.Output()
	var lines []blameLine
	if err != nil {
		// git blame refuses a file git has never seen (untracked, not yet
		// staged). Every line of such a file is uncommitted, which is
		// exactly what a zero-sha blame line means — so synthesise those
		// and let AI ranges match them as usual.
		if !isUntracked(repoRoot, relPath) {
			return fmt.Errorf("git blame failed: %w", err)
		}
		lines, err = uncommittedLines(absPath)
		if err != nil {
			return err
		}
	} else {
		lines = parsePorcelain(string(out))
	}
	matches, err := lookupRanges(repoRoot, relPath, lines)
	if err != nil {
		return err
	}
	return printBlame(relPath, lines, matches, newPalette(colorMode))
}

// isUntracked reports whether relPath exists on disk but is unknown to git.
func isUntracked(repoRoot, relPath string) bool {
	cmd := exec.Command("git", "ls-files", "--error-unmatch", "--", relPath)
	cmd.Dir = repoRoot
	cmd.Env = gitx.BypassEnv()
	return cmd.Run() != nil
}

// uncommittedLines builds a blame for a file git has never seen: every line
// carries the zero sha, exactly as git reports staged-but-uncommitted lines.
func uncommittedLines(absPath string) ([]blameLine, error) {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, err
	}
	text := strings.TrimSuffix(string(data), "\n")
	if text == "" {
		return nil, nil
	}
	var lines []blameLine
	for i, content := range strings.Split(text, "\n") {
		lines = append(lines, blameLine{sha: zeroSha, origLine: i + 1, finalLine: i + 1, content: content})
	}
	return lines, nil
}

var porcelainHeaderRE = regexp.MustCompile(`^([0-9a-f]{40}) (\d+) (\d+)(?: (\d+))?$`)

func parsePorcelain(out string) []blameLine {
	var result []blameLine
	rawLines := strings.Split(out, "\n")
	type commitMeta struct {
		author     string
		authorTime int
		summary    string
	}
	meta := map[string]commitMeta{}

	i := 0
	for i < len(rawLines) {
		if rawLines[i] == "" {
			i++
			continue
		}
		m := porcelainHeaderRE.FindStringSubmatch(rawLines[i])
		if m == nil {
			i++
			continue
		}
		sha := m[1]
		origLine, _ := strconv.Atoi(m[2])
		finalLine, _ := strconv.Atoi(m[3])
		var author, summary string
		var authorTime int
		i++
		for i < len(rawLines) && !strings.HasPrefix(rawLines[i], "\t") {
			cur := rawLines[i]
			sp := strings.IndexByte(cur, ' ')
			k := cur
			v := ""
			if sp != -1 {
				k = cur[:sp]
				v = cur[sp+1:]
			}
			switch k {
			case "author":
				author = v
			case "author-time":
				authorTime, _ = strconv.Atoi(v)
			case "summary":
				summary = v
			}
			i++
		}
		if i < len(rawLines) && strings.HasPrefix(rawLines[i], "\t") {
			cached := meta[sha]
			if author != "" {
				cached.author = author
			}
			if authorTime != 0 {
				cached.authorTime = authorTime
			}
			if summary != "" {
				cached.summary = summary
			}
			meta[sha] = cached
			result = append(result, blameLine{
				sha:        sha,
				origLine:   origLine,
				finalLine:  finalLine,
				content:    rawLines[i][1:],
				author:     cached.author,
				authorTime: cached.authorTime,
				summary:    cached.summary,
			})
			i++
		}
	}
	return result
}

type noteEntry struct {
	Lines   string `json:"lines"`
	Agent   string `json:"agent,omitempty"`
	Session string `json:"session,omitempty"`
	Model   string `json:"model,omitempty"`
	TS      string `json:"ts,omitempty"`
}
type noteBody struct {
	V      int                    `json:"v"`
	Commit string                 `json:"commit"`
	Files  map[string][]noteEntry `json:"files"`
}

// readNoteForCommit reads the attribution note for sha, first from the
// local notes ref and then from the tracking ref that plain `git fetch`
// populates — so blame works right after a fetch even before the shadow (or
// `posthook notes fetch`) has merged the two.
func readNoteForCommit(repoRoot, sha string) *noteBody {
	for _, ref := range []string{paths.NotesRef, notes.TrackingRef} {
		cmd := exec.Command("git", "notes", "--ref="+ref, "show", sha)
		cmd.Dir = repoRoot
		cmd.Env = gitx.BypassEnv()
		var out bytes.Buffer
		cmd.Stdout = &out
		if err := cmd.Run(); err != nil {
			continue
		}
		var n noteBody
		if err := json.Unmarshal(out.Bytes(), &n); err != nil {
			continue
		}
		return &n
	}
	return nil
}

var lineRangeRE = regexp.MustCompile(`^(\d+)(?:-(\d+))?$`)

func parseLineRange(spec string) (int, int, bool) {
	m := lineRangeRE.FindStringSubmatch(spec)
	if m == nil {
		return 0, 0, false
	}
	start, _ := strconv.Atoi(m[1])
	end := start
	if m[2] != "" {
		end, _ = strconv.Atoi(m[2])
	}
	return start, end, true
}

func lookupRanges(repoRoot, relPath string, lines []blameLine) (map[int]matchedRange, error) {
	db, err := store.Open()
	if err != nil {
		return nil, err
	}

	var repoID sql.NullString
	_ = db.QueryRow(`SELECT id FROM repositories WHERE root_path = ?`, repoRoot).Scan(&repoID)

	var rows []rangeRow
	if repoID.Valid {
		queryRows, err := db.Query(`
			WITH file_ranges AS (
				SELECT elr.event_id, elr.start_line, elr.end_line,
				       e.session_id, e.agent_slug, e.ts AS event_ts,
				       s.model_slug
				FROM event_line_ranges elr
				JOIN events e ON e.id = elr.event_id
				LEFT JOIN sessions s ON s.id = e.session_id
				WHERE e.repo_id = ? AND elr.rel_file_path = ?
			)
			SELECT fr.event_id, fr.session_id, fr.agent_slug, fr.model_slug, fr.event_ts,
			       fr.start_line, fr.end_line,
			       (SELECT c.sha FROM commits c
				WHERE c.repo_id = ?
				  AND datetime(c.committed_at) >= datetime(fr.event_ts)
				ORDER BY datetime(c.committed_at) ASC LIMIT 1) AS commit_sha
			FROM file_ranges fr
			ORDER BY datetime(fr.event_ts) ASC`,
			repoID.String, relPath, repoID.String)
		if err != nil {
			return nil, err
		}
		for queryRows.Next() {
			var r rangeRow
			if err := queryRows.Scan(&r.eventID, &r.sessionID, &r.agentSlug, &r.modelSlug,
				&r.eventTS, &r.startLine, &r.endLine, &r.commitSHA); err != nil {
				queryRows.Close()
				return nil, err
			}
			rows = append(rows, r)
		}
		queryRows.Close()
	}

	uniqueCommits := map[string]bool{}
	for _, l := range lines {
		if l.sha != zeroSha {
			uniqueCommits[l.sha] = true
		}
	}

	// A commit whose credited lines are on record is settled by that record:
	// those lines are the agent's, in the commit's own line numbers, and no
	// other line of the commit is. The ranges of the edits before it, which
	// say where the lines were when they were written, are not consulted.
	settled := map[string]bool{}
	var exact []rangeRow
	for sha := range uniqueCommits {
		recorded, known := recordedRanges(db, repoID.String, sha, relPath)
		if !known {
			continue
		}
		settled[sha] = true
		exact = append(exact, recorded...)
	}
	if len(settled) > 0 {
		kept := rows[:0]
		for _, r := range rows {
			if !r.commitSHA.Valid || !settled[r.commitSHA.String] {
				kept = append(kept, r)
			}
		}
		rows = append(kept, exact...)
	}

	// Note-fallback for commits without local ranges (e.g. teammate ran
	// blame after cloning). A note's ranges are matched like any other, by
	// the line's number in the commit that introduced it: exact for a v2
	// note, the edit-time position for a v1 note.
	local := map[string]bool{}
	for _, r := range rows {
		if r.commitSHA.Valid {
			local[r.commitSHA.String] = true
		}
	}
	for sha := range uniqueCommits {
		if local[sha] || settled[sha] {
			continue
		}
		note := readNoteForCommit(repoRoot, sha)
		if note == nil || note.Files == nil {
			continue
		}
		entries := note.Files[relPath]
		for _, entry := range entries {
			s, e, ok := parseLineRange(entry.Lines)
			if !ok {
				continue
			}
			r := rangeRow{
				eventID:   fmt.Sprintf("note:%s:%s:%d-%d", sha, entry.Session, s, e),
				agentSlug: entry.Agent,
				eventTS:   entry.TS,
				startLine: s,
				endLine:   e,
				commitSHA: sql.NullString{String: sha, Valid: true},
			}
			if entry.Session != "" {
				r.sessionID = sql.NullString{String: entry.Session, Valid: true}
			}
			if entry.Model != "" {
				r.modelSlug = sql.NullString{String: entry.Model, Valid: true}
			}
			if r.agentSlug == "" {
				r.agentSlug = "unknown"
			}
			rows = append(rows, r)
		}
	}

	if os.Getenv("POSTHOOK_DEBUG") == "1" {
		logx.Debugf("blame: %d candidate range(s)", len(rows))
		for _, r := range rows {
			shaShort := "null"
			if r.commitSHA.Valid && len(r.commitSHA.String) >= 7 {
				shaShort = r.commitSHA.String[:7]
			}
			logx.Debugf("  range lines=%d-%d ts=%s commit=%s",
				r.startLine, r.endLine, r.eventTS, shaShort)
		}
	}

	// For each blame line, find the most recent range that fits AND whose
	// commit_sha matches the blame's SHA. Uncommitted (zero-sha) lines match
	// ranges with no commit_sha yet.
	matches := map[int]matchedRange{}
	for _, line := range lines {
		wantCommitted := line.sha != zeroSha
		var pick *rangeRow
		for i := range rows {
			r := &rows[i]
			if r.startLine > line.origLine || r.endLine < line.origLine {
				continue
			}
			if wantCommitted {
				if !r.commitSHA.Valid || r.commitSHA.String != line.sha {
					continue
				}
			} else {
				if r.commitSHA.Valid {
					continue
				}
			}
			pick = r // last winning row in ASC order = most recent
		}
		if pick != nil {
			matches[line.finalLine] = matchedRange{
				eventID:   pick.eventID,
				sessionID: pick.sessionID,
				agentSlug: pick.agentSlug,
				modelSlug: pick.modelSlug,
				eventTS:   pick.eventTS,
				startLine: pick.startLine,
				endLine:   pick.endLine,
			}
		}
	}
	applyFileMap(db, repoRoot, relPath, lines, matches)
	return matches, nil
}

// recordedRanges returns the lines of file a locally recorded commit is
// credited with, as ranges in the commit's line numbers. known is false when
// the record cannot settle the file: the commit is not recorded here, or its
// lines in the file were matched by position with nothing on record.
//
// repoID is the row of the checkout being blamed, empty when it has none. A
// commit is looked for under that row and under the other checkouts of the
// same repository (a rewrite is recorded with the checkout of its original),
// which is also what lets the lookup use the (repo_id, sha) index.
func recordedRanges(db *store.DB, repoID, sha, file string) (out []rangeRow, known bool) {
	if repoID == "" {
		return nil, false
	}
	var commitID, committedAt string
	if err := db.QueryRow(`
		SELECT id, committed_at FROM commits
		WHERE sha = ? AND repo_id IN `+store.CheckoutsOf+`
		ORDER BY (repo_id = ?) DESC LIMIT 1`, sha, repoID, repoID, repoID).Scan(&commitID, &committedAt); err != nil {
		return nil, false
	}
	for _, r := range db.CommitLineRecords(commitID) {
		if r.File != file {
			continue
		}
		for _, run := range store.LineRuns(r.Numbers) {
			row := rangeRow{
				eventID:   fmt.Sprintf("record:%s:%s:%d-%d", sha, r.SessionID, run[0], run[1]),
				sessionID: sql.NullString{String: r.SessionID, Valid: r.SessionID != ""},
				agentSlug: r.Agent,
				eventTS:   r.TS,
				startLine: run[0],
				endLine:   run[1],
				commitSHA: sql.NullString{String: sha, Valid: true},
			}
			if r.Model != "" {
				row.modelSlug = sql.NullString{String: r.Model, Valid: true}
			}
			if row.agentSlug == "" {
				row.agentSlug = "unknown"
			}
			out = append(out, row)
		}
	}
	if len(out) > 0 {
		return out, true
	}
	var credited int
	_ = db.QueryRow(`SELECT COALESCE(SUM(lines_attributed), 0) FROM commit_session_files WHERE commit_id = ? AND file_path = ?`, commitID, file).Scan(&credited)
	if credited > 0 {
		return nil, false
	}
	// Credited nothing in this file. For a commit matched by content that is
	// the answer; an older commit never said which lines it was credited.
	return nil, db.MatchesByContent(committedAt)
}

// applyFileMap brings in what the file's authorship map knows, which is more
// than any record of a commit: the edit that wrote each line, and the lines
// that are in no commit yet.
//
// The map is lined up with the file as it is being shown. An uncommitted
// line is the agent's exactly when the map says an edit wrote it. A
// committed line takes its edit from the map when the map counted that line
// in the same commit git blames it on, so the prompt shown above it is the
// one that produced it and not just one from the same session.
func applyFileMap(db *store.DB, repoRoot, relPath string, lines []blameLine, matches map[int]matchedRange) {
	m, ok := db.FileMap(filepath.Join(repoRoot, filepath.FromSlash(relPath)))
	if !ok || !m.HasAuthors() || len(lines) == 0 {
		return
	}
	texts := make([]string, len(lines))
	for i, l := range lines {
		texts[i] = l.content
	}
	lined, _ := m.Carry(texts)
	models := map[string]sql.NullString{}
	for i, l := range lines {
		author, written := lined.AuthorOf(i)
		switch {
		case l.sha == zeroSha:
			delete(matches, l.finalLine)
			if !written {
				continue
			}
		case !written || lined.CommitOf(i) != l.sha:
			continue
		}
		model, seen := models[author.SessionID]
		if !seen {
			_ = db.QueryRow(`SELECT model_slug FROM sessions WHERE id = ?`, author.SessionID).Scan(&model)
			models[author.SessionID] = model
		}
		agent := author.Agent
		if agent == "" {
			agent = "unknown"
		}
		matches[l.finalLine] = matchedRange{
			eventID:   author.EventID,
			sessionID: sql.NullString{String: author.SessionID, Valid: author.SessionID != ""},
			agentSlug: agent,
			modelSlug: model,
			eventTS:   author.TS,
			startLine: l.finalLine,
			endLine:   l.finalLine,
		}
	}
}

// palette holds the ANSI sequences used by blame; every field is empty when
// colour is off so the printing code needs no branches.
type palette struct {
	on       bool
	reset    string
	dim      string
	bold     string
	human    string
	pending  string
	sessions []string
}

func newPalette(mode string) palette {
	on := false
	switch mode {
	case "always":
		on = true
	case "never":
		on = false
	default:
		on = stdoutIsTerminal() && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
	}
	if !on {
		return palette{}
	}
	return palette{
		on:      true,
		reset:   "\x1b[0m",
		dim:     "\x1b[2m",
		bold:    "\x1b[1m",
		human:   "\x1b[2m",
		pending: "\x1b[33m",
		// One colour per AI session, in order of first appearance, so edits
		// from the same session read as a block.
		sessions: []string{"\x1b[36m", "\x1b[35m", "\x1b[32m", "\x1b[34m", "\x1b[96m", "\x1b[95m", "\x1b[92m", "\x1b[94m"},
	}
}

func stdoutIsTerminal() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func (p palette) paint(color, text string) string {
	if !p.on || color == "" {
		return text
	}
	return color + text + p.reset
}

// sessionSummary is one row of the footer: an AI session that owns at least
// one line of the file.
type sessionSummary struct {
	key      string // session id, or agent slug when the session is unknown
	id       sql.NullString
	agent    string
	model    sql.NullString
	lines    int
	firstSeq int
	color    string
}

func printBlame(relPath string, lines []blameLine, matches map[int]matchedRange, pal palette) error {
	db, err := store.Open()
	if err != nil {
		return err
	}
	prompts := resolvePromptsForEvents(db, matches)

	// Assign each session a stable colour in order of first appearance.
	sessions := map[string]*sessionSummary{}
	var order []*sessionSummary
	for _, l := range lines {
		m, ok := matches[l.finalLine]
		if !ok {
			continue
		}
		key := m.agentSlug
		if m.sessionID.Valid {
			key = m.sessionID.String
		}
		ss, seen := sessions[key]
		if !seen {
			ss = &sessionSummary{key: key, id: m.sessionID, agent: m.agentSlug, model: m.modelSlug, firstSeq: len(order)}
			if len(pal.sessions) > 0 {
				ss.color = pal.sessions[len(order)%len(pal.sessions)]
			}
			sessions[key] = ss
			order = append(order, ss)
		}
		ss.lines++
		if !ss.model.Valid && m.modelSlug.Valid {
			ss.model = m.modelSlug
		}
	}

	fmt.Printf("%s %s\n\n", pal.paint(pal.bold, "posthook blame"), relPath)
	lineWidth := len(strconv.Itoa(len(lines)))
	tagWidth := 28
	prevEventID := ""
	// One prompt usually produces many separate edits to a file; the header
	// is only worth printing when the prompt actually changes.
	lastPrompt := ""

	for _, l := range lines {
		match, hasMatch := matches[l.finalLine]
		num := pal.paint(pal.dim, fmt.Sprintf("%*d", lineWidth, l.finalLine))
		var tag string
		if hasMatch {
			key := match.agentSlug
			if match.sessionID.Valid {
				key = match.sessionID.String
			}
			color := sessions[key].color
			if match.eventID != prevEventID {
				if prompt := prompts[match.eventID]; prompt != "" && prompt != lastPrompt {
					fmt.Printf("  %s  %s\n", strings.Repeat(" ", lineWidth),
						pal.paint(color+pal.bold, "┌─ "+formatPrompt(prompt)))
					lastPrompt = prompt
				}
			}
			prevEventID = match.eventID
			ts := match.eventTS
			if len(ts) >= 16 {
				ts = ts[11:16]
			}
			session := "?"
			if match.sessionID.Valid && len(match.sessionID.String) >= 8 {
				session = match.sessionID.String[:8]
			} else if match.sessionID.Valid {
				session = match.sessionID.String
			}
			plain := fmt.Sprintf("AI %s %s %s", compactModel(match.modelSlug), ts, session)
			tag = pal.paint(color, fmt.Sprintf("%-*s", tagWidth, plain))
		} else {
			prevEventID = ""
			var plain, color string
			if l.sha == zeroSha {
				plain, color = "uncommitted", pal.pending
			} else {
				author := "?"
				if l.author != "" {
					author = strings.SplitN(l.author, " ", 2)[0]
				}
				plain, color = "human · "+author, pal.human
			}
			tag = pal.paint(color, fmt.Sprintf("%-*s", tagWidth, plain))
		}
		fmt.Printf("  %s  %s  %s\n", num, tag, l.content)
	}
	fmt.Println()
	aiCount := len(matches)
	total := len(lines)
	pct := 0.0
	if total > 0 {
		pct = float64(aiCount) / float64(total) * 100
	}
	fmt.Printf("  %s\n", pal.paint(pal.bold, fmt.Sprintf("%d/%d lines AI-authored (%.1f%%)", aiCount, total, pct)))

	if len(order) > 0 {
		printSessionFooter(db, order, pal)
	}
	return nil
}

// printSessionFooter lists every AI session that owns lines in the file,
// with a link into the local dashboard for sessions this machine has
// captured (a teammate's session, known only from the git note, gets no link).
func printSessionFooter(db *store.DB, order []*sessionSummary, pal palette) {
	dash := resolveDashConfig()
	dashUp := portOpen(dash.addr())

	fmt.Println()
	for _, ss := range order {
		id := "?"
		if ss.id.Valid && len(ss.id.String) >= 8 {
			id = ss.id.String[:8]
		} else if ss.id.Valid {
			id = ss.id.String
		}
		who := ss.agent
		if ss.model.Valid && ss.model.String != "" {
			who += " · " + compactModel(ss.model)
		}
		unit := "lines"
		if ss.lines == 1 {
			unit = "line"
		}
		row := fmt.Sprintf("  %s %s  %-34s %4d %s", pal.paint(ss.color, "■"), pal.paint(ss.color+pal.bold, id), who, ss.lines, unit)
		if dashUp && ss.id.Valid && sessionKnownLocally(db, ss.id.String) {
			row += "  " + pal.paint(pal.dim, dash.url()+"/sessions/"+ss.id.String)
		}
		fmt.Println(row)
	}
}

func sessionKnownLocally(db *store.DB, sessionID string) bool {
	var one int
	return db.QueryRow(`SELECT 1 FROM sessions WHERE id = ?`, sessionID).Scan(&one) == nil
}

// resolvePromptsForEvents finds, for each matched event, the prompt the user
// typed before it: first from the Claude Code transcript on disk, then from
// the prompts posthook stored at session end (transcripts are pruned by the
// agents after a while; the stored copy is not).
func resolvePromptsForEvents(db *store.DB, matches map[int]matchedRange) map[string]string {
	out := map[string]string{}
	if len(matches) == 0 {
		return out
	}
	byID := map[string]matchedRange{}
	for _, m := range matches {
		byID[m.eventID] = m
	}

	placeholders := make([]string, 0, len(byID))
	params := make([]any, 0, len(byID))
	for id := range byID {
		placeholders = append(placeholders, "?")
		params = append(params, id)
	}
	q := fmt.Sprintf(`
		SELECT id, ts, json_extract(payload, '$.transcript_path') AS path
		FROM events
		WHERE id IN (%s)`, strings.Join(placeholders, ","))
	rows, err := db.Query(q, params...)
	if err == nil {
		cache := map[string]map[string]string{}
		for rows.Next() {
			var id, ts string
			var path sql.NullString
			if err := rows.Scan(&id, &ts, &path); err != nil {
				break
			}
			if !path.Valid {
				continue
			}
			perTranscript, ok := cache[path.String]
			if !ok {
				perTranscript = map[string]string{}
				cache[path.String] = perTranscript
			}
			prompt, found := perTranscript[ts]
			if !found {
				prompt = transcript.FindPromptBefore(path.String, ts)
				perTranscript[ts] = prompt
			}
			if prompt != "" {
				out[id] = prompt
			}
		}
		rows.Close()
	}

	for id, m := range byID {
		if out[id] != "" || !m.sessionID.Valid || m.eventTS == "" {
			continue
		}
		var text string
		err := db.QueryRow(`
			SELECT prompt_text FROM session_prompts
			WHERE session_id = ? AND ts IS NOT NULL AND ts < ?
			ORDER BY ts DESC LIMIT 1`, m.sessionID.String, m.eventTS).Scan(&text)
		if err == nil && strings.TrimSpace(text) != "" {
			out[id] = text
		} else if err != nil {
			logx.Debugf("blame: stored-prompt lookup for session %s before %s: %v", m.sessionID.String, m.eventTS, err)
		}
	}
	return out
}

func formatPrompt(text string) string {
	cleaned := strings.Join(strings.Fields(text), " ")
	const limit = 96
	if len(cleaned) > limit {
		return cleaned[:limit-1] + "…"
	}
	return cleaned
}

var versionSuffixRE = regexp.MustCompile(`-\d{8,}$`)

func compactModel(m sql.NullString) string {
	if !m.Valid || m.String == "" {
		return "?"
	}
	trimmed := versionSuffixRE.ReplaceAllString(m.String, "")
	if len(trimmed) > 16 {
		return trimmed[:16]
	}
	return trimmed
}
