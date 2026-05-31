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
	"github.com/bilanc/posthook/internal/paths"
	"github.com/bilanc/posthook/internal/store"
	"github.com/bilanc/posthook/internal/transcript"

	"github.com/spf13/cobra"
)

const zeroSha = "0000000000000000000000000000000000000000"

func newBlameCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "blame <file>",
		Short: "Show per-line AI attribution for a file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBlame(args[0])
		},
	}
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
	eventID    string
	sessionID  sql.NullString
	agentSlug  string
	modelSlug  sql.NullString
	eventTS    string
	startLine  int
	endLine    int
}

type rangeRow struct {
	eventID    string
	sessionID  sql.NullString
	agentSlug  string
	modelSlug  sql.NullString
	eventTS    string
	startLine  int
	endLine    int
	commitSHA  sql.NullString
}

func runBlame(file string) error {
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
	if err != nil {
		return fmt.Errorf("git blame failed: %w", err)
	}

	lines := parsePorcelain(string(out))
	matches, err := lookupRanges(repoRoot, relPath, lines)
	if err != nil {
		return err
	}
	return printBlame(relPath, lines, matches)
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

func readNoteForCommit(repoRoot, sha string) *noteBody {
	cmd := exec.Command("git", "notes", "--ref="+paths.NotesRef, "show", sha)
	cmd.Dir = repoRoot
	cmd.Env = gitx.BypassEnv()
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil
	}
	var n noteBody
	if err := json.Unmarshal(out.Bytes(), &n); err != nil {
		return nil
	}
	return &n
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

	// Note-fallback for commits without local ranges (e.g. teammate ran
	// blame after cloning).
	local := map[string]bool{}
	for _, r := range rows {
		if r.commitSHA.Valid {
			local[r.commitSHA.String] = true
		}
	}
	uniqueCommits := map[string]bool{}
	for _, l := range lines {
		if l.sha != zeroSha {
			uniqueCommits[l.sha] = true
		}
	}
	for sha := range uniqueCommits {
		if local[sha] {
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
	return matches, nil
}

func printBlame(relPath string, lines []blameLine, matches map[int]matchedRange) error {
	db, err := store.Open()
	if err != nil {
		return err
	}
	prompts := resolvePromptsForEvents(db, matches)

	fmt.Printf("posthook blame %s\n\n", relPath)
	lineWidth := len(strconv.Itoa(len(lines)))
	tagWidth := 28
	prevEventID := ""

	for _, l := range lines {
		match, hasMatch := matches[l.finalLine]
		var tag string
		if hasMatch {
			if match.eventID != prevEventID {
				if prompt := prompts[match.eventID]; prompt != "" {
					fmt.Printf("  %s  ┌─ %s\n", strings.Repeat(" ", lineWidth), formatPrompt(prompt))
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
			tag = fmt.Sprintf("AI %s %s %s", compactModel(match.modelSlug), ts, session)
		} else {
			prevEventID = ""
			if l.sha == zeroSha {
				tag = "uncommitted"
			} else {
				author := "?"
				if l.author != "" {
					author = strings.SplitN(l.author, " ", 2)[0]
				}
				tag = "human · " + author
			}
		}
		fmt.Printf("  %*d  %-*s  %s\n", lineWidth, l.finalLine, tagWidth, tag, l.content)
	}
	fmt.Println()
	aiCount := len(matches)
	total := len(lines)
	pct := 0.0
	if total > 0 {
		pct = float64(aiCount) / float64(total) * 100
	}
	fmt.Printf("  %d/%d lines AI-authored (%.1f%%)\n", aiCount, total, pct)
	return nil
}

func resolvePromptsForEvents(db *store.DB, matches map[int]matchedRange) map[string]string {
	out := map[string]string{}
	if len(matches) == 0 {
		return out
	}
	idSet := map[string]bool{}
	for _, m := range matches {
		idSet[m.eventID] = true
	}
	if len(idSet) == 0 {
		return out
	}

	placeholders := make([]string, 0, len(idSet))
	params := make([]any, 0, len(idSet))
	for id := range idSet {
		placeholders = append(placeholders, "?")
		params = append(params, id)
	}
	q := fmt.Sprintf(`
		SELECT id, ts, json_extract(payload, '$.transcript_path') AS path
		FROM events
		WHERE id IN (%s)`, strings.Join(placeholders, ","))
	rows, err := db.Query(q, params...)
	if err != nil {
		return out
	}
	defer rows.Close()

	cache := map[string]map[string]string{}
	for rows.Next() {
		var id, ts string
		var path sql.NullString
		if err := rows.Scan(&id, &ts, &path); err != nil {
			return out
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
