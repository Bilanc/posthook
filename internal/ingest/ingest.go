// Package ingest is the core data-capture path: it consumes agent hook
// payloads (from stdin) and git-commit metadata (from arguments), writes
// events + sessions + commits + line ranges to the store, and produces the
// refs/notes/posthook payload that ships attribution across clones.
//
// Lives in its own package so both the CLI (commands.IngestAgentEvent et al.)
// and the git shadow proxy can call it without an import cycle.
package ingest

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bilanc/posthook/internal/config"
	"github.com/bilanc/posthook/internal/gitx"
	"github.com/bilanc/posthook/internal/lineranges"
	"github.com/bilanc/posthook/internal/logx"
	"github.com/bilanc/posthook/internal/paths"
	"github.com/bilanc/posthook/internal/store"
	"github.com/bilanc/posthook/internal/transcript"
	"github.com/google/uuid"
)

var editTools = map[string]bool{
	"Edit": true, "Write": true, "MultiEdit": true,
}

// AgentEvent reads an agent hook payload from stdin and ingests it.
func AgentEvent(agentSlug string) error {
	db, err := store.Open()
	if err != nil {
		return err
	}
	raw, err := readStdin()
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(raw)) == "" {
		// Hook fired with no payload — usually a misconfigured agent or a
		// hook invoked outside its expected flow. Record it so `posthook
		// status` surfaces data-quality issues.
		if err := recordHookMisfire(db, agentSlug); err != nil {
			return err
		}
		logx.Debugf("ingest --agent %s: no stdin payload, recorded misfire", agentSlug)
		return nil
	}

	// Some agents send non-JSON payloads or wrap them. Store raw text under
	// `raw` if parsing fails — beats dropping the event.
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		payload = map[string]any{"raw": string(raw)}
	}

	id := uuid.NewString()
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	rawEventType := pickString(payload, "hook_event_name", "event", "type")
	if rawEventType == "" {
		rawEventType = "unknown"
	}
	eventType := normalizeEventType(rawEventType)
	rawSessionID := pickString(payload, "session_id", "sessionId")
	sessionID := ""
	if rawSessionID != "" {
		sessionID = resolveSessionID(db, agentSlug, rawSessionID)
	}
	cwd := pickString(payload, "cwd")
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	workspaceRoots := extractWorkspaceRoots(payload)
	applyPatchFiles := applyPatchFilesForPayload(payload)
	filePath := extractFilePath(payload)
	if filePath == "" && len(applyPatchFiles) > 0 {
		filePath = applyPatchFiles[0].FilePath
	}
	var resolvedFilePath, canonicalFilePath string
	if filePath != "" {
		resolvedFilePath = resolveEventFilePath(filePath, cwd, workspaceRoots)
		canonicalFilePath = gitx.Canonicalize(resolvedFilePath)
	}
	model := pickString(payload, "model", "model_id")

	patchLinesAdded := sql.NullInt64{}
	patchLinesRemoved := sql.NullInt64{}
	if len(applyPatchFiles) > 0 {
		a, r := 0, 0
		for _, f := range applyPatchFiles {
			a += f.LinesAdded
			r += f.LinesRemoved
		}
		patchLinesAdded = sql.NullInt64{Int64: int64(a), Valid: true}
		patchLinesRemoved = sql.NullInt64{Int64: int64(r), Valid: true}
	}

	repoRoot := findEventRepoRoot(cwd, canonicalFilePath, workspaceRoots)
	var repoID sql.NullString
	var relFilePath sql.NullString
	if repoRoot != "" {
		id, err := upsertRepositoryByCwd(db, repoRoot)
		if err != nil {
			return err
		}
		repoID = sql.NullString{String: id, Valid: true}
		if canonicalFilePath != "" {
			if rel := gitx.RelPathInRepo(repoRoot, canonicalFilePath); rel != "" {
				relFilePath = sql.NullString{String: rel, Valid: true}
			}
		}
	}

	// Engineer identity: the confirmed identity in ~/.posthook/config.json wins
	// (set via `posthook identity --setup`); per-repo git config is the
	// fallback — it's often unset, or a personal address on side repos. A third
	// fallback (commit-author matching) runs later in backfill.
	engineerEmail := ""
	engineerName := ""
	if cfg, err := config.Load(); err == nil {
		engineerEmail = cfg.Engineer.Email
		engineerName = cfg.Engineer.Name
	}
	if repoRoot != "" {
		if engineerEmail == "" {
			engineerEmail = gitx.Run(repoRoot, "config", "--get", "user.email")
		}
		if engineerName == "" {
			engineerName = gitx.Run(repoRoot, "config", "--get", "user.name")
		}
	}

	if sessionID != "" {
		_, err := db.Exec(`
			INSERT INTO sessions (
				id, org_id, agent_slug, model_slug, repo_id, cwd, started_at,
				engineer_email, engineer_name
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				model_slug     = COALESCE(excluded.model_slug, sessions.model_slug),
				repo_id        = COALESCE(sessions.repo_id, excluded.repo_id),
				cwd            = COALESCE(sessions.cwd, excluded.cwd),
				engineer_email = COALESCE(sessions.engineer_email, excluded.engineer_email),
				engineer_name  = COALESCE(sessions.engineer_name, excluded.engineer_name),
				synced_at      = NULL`,
			sessionID, store.LocalOrgID, agentSlug, nullableString(model), repoID,
			cwd, ts, nullableString(engineerEmail), nullableString(engineerName))
		if err != nil {
			return err
		}
	}

	finalFilePath := canonicalFilePath
	if finalFilePath == "" {
		finalFilePath = resolvedFilePath
	}
	if finalFilePath == "" {
		finalFilePath = filePath
	}

	var sessIDArg sql.NullString
	if sessionID != "" {
		sessIDArg = sql.NullString{String: sessionID, Valid: true}
	}

	_, err = db.Exec(`
		INSERT INTO events (
			id, org_id, session_id, ts, event_type, agent_slug, cwd,
			file_path, repo_id, rel_file_path, lines_added, lines_removed, payload
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, store.LocalOrgID, sessIDArg, ts, eventType, agentSlug, cwd,
		nullableString(finalFilePath), repoID, relFilePath,
		patchLinesAdded, patchLinesRemoved, string(raw))
	if err != nil {
		return err
	}
	logx.Debugf("ingested %s event %s (session=%s)", agentSlug, eventType, sessionID)

	// Line-range capture. Fail-soft: never block ingest on these.
	if eventType == "PostToolUse" && len(applyPatchFiles) > 0 {
		if err := captureApplyPatchLineRanges(db, id, applyPatchFiles, cwd, workspaceRoots); err != nil {
			logx.Warnf("line-range capture failed: %v", err)
		}
	} else {
		if toolName := lineRangeToolForEvent(agentSlug, eventType, payload); toolName != "" && canonicalFilePath != "" {
			if err := captureLineRanges(db, id, canonicalFilePath, relFilePath, toolName, payload); err != nil {
				logx.Warnf("line-range capture failed: %v", err)
			}
		}
	}

	// Stop event: parse the Claude transcript to backfill model + span.
	if eventType == "Stop" && sessionID != "" {
		transcriptPath := pickString(payload, "transcript_path", "transcriptPath")
		if transcriptPath != "" {
			if summary := transcript.ParseFile(transcriptPath); summary != nil {
				if _, err := db.Exec(`
					UPDATE sessions SET
						model_slug = COALESCE(?, model_slug),
						started_at = COALESCE(?, started_at),
						ended_at   = COALESCE(?, ended_at),
						synced_at  = NULL
					WHERE id = ?`,
					nullableString(summary.Model),
					nullableString(summary.FirstTS),
					nullableString(summary.LastTS),
					sessionID); err != nil {
					logx.Warnf("transcript update failed: %v", err)
				} else {
					logx.Debugf("updated session %s from transcript: model=%s messages=%d",
						sessionID, summary.Model, summary.AssistantMessageCount)
				}
			}
		}
	}

	return nil
}

func recordHookMisfire(db *store.DB, agentSlug string) error {
	cwd, _ := os.Getwd()
	root := gitx.FindRepoRoot(cwd)
	var repoID sql.NullString
	if root != "" {
		id, err := upsertRepositoryByCwd(db, root)
		if err != nil {
			return err
		}
		repoID = sql.NullString{String: id, Valid: true}
	}
	_, err := db.Exec(`
		INSERT INTO events (
			id, org_id, session_id, ts, event_type, agent_slug, cwd,
			file_path, repo_id, rel_file_path, payload
		) VALUES (?, ?, NULL, ?, 'hook_misfire', ?, ?, NULL, ?, NULL, NULL)`,
		uuid.NewString(), store.LocalOrgID, time.Now().UTC().Format(time.RFC3339Nano),
		agentSlug, cwd, repoID)
	return err
}

// GitCommit records a single commit identified by repoRoot + sha. Called by
// the post-commit hook, the git shadow proxy, and the curl-pipe ingest.
func GitCommit(repoRoot, sha string) error {
	db, err := store.Open()
	if err != nil {
		return err
	}
	repoRoot = gitx.Canonicalize(repoRoot)

	const fmtStr = "%H%x1f%P%x1f%ae%x1f%an%x1f%cI%x1f%s"
	cmd := exec.Command("git", "log", "-1", "--format="+fmtStr, sha)
	cmd.Dir = repoRoot
	cmd.Env = gitx.BypassEnv()
	out, err := cmd.Output()
	if err != nil {
		logx.Warnf("git log failed for %s: %v", sha, err)
		return nil
	}
	line := strings.TrimSpace(string(out))
	parts := strings.SplitN(line, "\x1f", 6)
	for len(parts) < 6 {
		parts = append(parts, "")
	}
	parents := parts[1]
	email := parts[2]
	name := parts[3]
	committedAt := parts[4]
	subject := parts[5]

	parentSha := ""
	if parents != "" {
		parentSha = strings.Fields(parents)[0]
	}
	branch := gitx.Run(repoRoot, "rev-parse", "--abbrev-ref", "HEAD")
	remoteURL := gitx.Run(repoRoot, "config", "--get", "remote.origin.url")
	repoName := filepath.Base(repoRoot)
	if remoteURL != "" {
		repoName = guessRepoNameFromRemote(remoteURL)
	}
	repoID, err := upsertRepository(db, repoRoot, remoteURL, repoName)
	if err != nil {
		return err
	}

	// Per-file numstat for totals.
	numstat := gitx.Run(repoRoot, "show", "--numstat", "--format=", sha)
	added, removed, filesChanged := 0, 0, 0
	type fileRow struct {
		path    string
		added   int
		removed int
	}
	var perFile []fileRow
	for _, row := range strings.Split(numstat, "\n") {
		if strings.TrimSpace(row) == "" {
			continue
		}
		cols := strings.SplitN(row, "\t", 3)
		if len(cols) < 3 {
			continue
		}
		a := 0
		r := 0
		if cols[0] != "-" {
			a, _ = strconv.Atoi(cols[0])
		}
		if cols[1] != "-" {
			r, _ = strconv.Atoi(cols[1])
		}
		added += a
		removed += r
		filesChanged++
		perFile = append(perFile, fileRow{path: cols[2], added: a, removed: r})
	}

	commitID := uuid.NewString()
	if committedAt == "" {
		committedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	_, err = db.Exec(`
		INSERT INTO commits (
			id, org_id, repo_id, sha, parent_sha, author_email, author_name,
			committed_at, branch, message, lines_added, lines_removed, files_changed
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(repo_id, sha) DO NOTHING`,
		commitID, store.LocalOrgID, repoID, sha, nullableString(parentSha),
		nullableString(email), nullableString(name), committedAt,
		nullableString(branch), nullableString(subject),
		added, removed, filesChanged)
	if err != nil {
		logx.Warnf("commit insert failed: %v", err)
		return nil
	}

	var finalCommitID string
	if err := db.QueryRow(`SELECT id FROM commits WHERE repo_id = ? AND sha = ?`, repoID, sha).Scan(&finalCommitID); err != nil {
		finalCommitID = commitID
	}

	for _, f := range perFile {
		if _, err := db.Exec(`
			INSERT INTO commit_files (commit_id, file_path, lines_added, lines_removed)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(commit_id, file_path) DO NOTHING`,
			finalCommitID, f.path, f.added, f.removed); err != nil {
			logx.Warnf("commit_files insert failed: %v", err)
		}
	}

	attributedSessions, err := db.RefreshCommitAttributions(finalCommitID)
	if err != nil {
		logx.Warnf("refresh attributions failed: %v", err)
	}
	if len(sha) >= 7 {
		logx.Debugf("ingested commit %s in %s (+%d/-%d)", sha[:7], repoName, added, removed)
	}
	if attributedSessions > 0 && len(sha) >= 7 {
		logx.Debugf("attributed commit %s to %d session(s)", sha[:7], attributedSessions)
	}

	if err := writeNoteForCommit(db, repoRoot, repoID, sha, finalCommitID); err != nil {
		if len(sha) >= 7 {
			logx.Warnf("note write failed for %s: %v", sha[:7], err)
		}
	}
	return nil
}

func writeNoteForCommit(db *store.DB, repoRoot, repoID, sha, commitID string) error {
	rows, err := db.Query(`
		SELECT elr.rel_file_path, e.session_id, e.agent_slug, s.model_slug,
		       e.ts AS event_ts, elr.start_line, elr.end_line
		FROM commits c
		JOIN commit_files cf ON cf.commit_id = c.id
		JOIN event_line_ranges elr ON elr.rel_file_path = cf.file_path
		JOIN events e ON e.id = elr.event_id
		LEFT JOIN sessions s ON s.id = e.session_id
		WHERE c.id = ?
		  AND c.repo_id = ?
		  AND e.repo_id = c.repo_id
		  AND elr.rel_file_path IS NOT NULL
		  AND datetime(c.committed_at) >= datetime(e.ts)
		  AND NOT EXISTS (
			SELECT 1
			FROM commits c2
			JOIN commit_files cf2 ON cf2.commit_id = c2.id
			WHERE c2.repo_id = c.repo_id
			  AND cf2.file_path = cf.file_path
			  AND datetime(c2.committed_at) >= datetime(e.ts)
			  AND datetime(c2.committed_at) < datetime(c.committed_at)
		  )
		ORDER BY elr.rel_file_path, elr.start_line`,
		commitID, repoID)
	if err != nil {
		return err
	}
	defer rows.Close()

	type entry struct {
		Lines   string `json:"lines"`
		Agent   string `json:"agent"`
		Session string `json:"session,omitempty"`
		Model   string `json:"model,omitempty"`
		TS      string `json:"ts"`
	}
	files := map[string][]entry{}
	count := 0
	for rows.Next() {
		var rel, agent, ts string
		var session, model sql.NullString
		var start, end int
		if err := rows.Scan(&rel, &session, &agent, &model, &ts, &start, &end); err != nil {
			return err
		}
		lineRange := fmt.Sprintf("%d", start)
		if start != end {
			lineRange = fmt.Sprintf("%d-%d", start, end)
		}
		files[rel] = append(files[rel], entry{
			Lines:   lineRange,
			Agent:   agent,
			Session: session.String,
			Model:   model.String,
			TS:      ts,
		})
		count++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if count == 0 {
		if len(sha) >= 7 {
			logx.Debugf("note: no AI ranges for %s, skipping", sha[:7])
		}
		return nil
	}

	note := struct {
		V      int                `json:"v"`
		Commit string             `json:"commit"`
		Files  map[string][]entry `json:"files"`
	}{V: 1, Commit: sha, Files: files}
	encoded, err := json.Marshal(note)
	if err != nil {
		return err
	}

	cmd := exec.Command("git", "notes", "--ref="+paths.NotesRef, "add", "-f", "-F", "-", sha)
	cmd.Dir = repoRoot
	cmd.Env = gitx.BypassEnv()
	cmd.Stdin = strings.NewReader(string(encoded))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git notes: %v: %s", err, string(out))
	}
	if len(sha) >= 7 {
		logx.Debugf("note: wrote %d range(s) for %s", count, sha[:7])
	}
	return nil
}

// ── helpers ───────────────────────────────────────────────────────────────

func captureLineRanges(
	db *store.DB,
	eventID, filePath string,
	relFilePath sql.NullString,
	toolName string,
	payload map[string]any,
) error {
	if _, err := os.Stat(filePath); err != nil {
		logx.Debugf("line-range: file gone, skipping (%s)", filePath)
		return nil
	}
	content, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}
	input := lineRangeToolInput(payload)
	if input == nil {
		return nil
	}
	out := lineranges.Extract(toolName, *input, string(content))
	return insertLineRanges(db, eventID, filePath, relFilePath, content, toolName, out)
}

func captureApplyPatchLineRanges(
	db *store.DB,
	eventID string,
	files []lineranges.ApplyPatchFile,
	cwd string,
	workspaceRoots []string,
) error {
	for _, file := range files {
		if len(file.Edits) == 0 {
			continue
		}
		resolvedPath := resolveEventFilePath(file.FilePath, cwd, workspaceRoots)
		canonicalPath := gitx.Canonicalize(resolvedPath)
		if _, err := os.Stat(canonicalPath); err != nil {
			logx.Debugf("line-range: file gone, skipping (%s)", canonicalPath)
			continue
		}
		root := findEventRepoRoot(cwd, canonicalPath, workspaceRoots)
		var rel sql.NullString
		if root != "" {
			if v := gitx.RelPathInRepo(root, canonicalPath); v != "" {
				rel = sql.NullString{String: v, Valid: true}
			}
		}
		content, err := os.ReadFile(canonicalPath)
		if err != nil {
			continue
		}
		out := lineranges.Extract("MultiEdit", lineranges.ToolInput{Edits: file.Edits}, string(content))
		if err := insertLineRanges(db, eventID, canonicalPath, rel, content, "apply_patch", out); err != nil {
			return err
		}
	}
	return nil
}

func insertLineRanges(
	db *store.DB,
	eventID, filePath string,
	relFilePath sql.NullString,
	content []byte,
	toolName string,
	out lineranges.Extracted,
) error {
	if len(out.Ranges) == 0 {
		if out.Unlocated > 0 {
			logx.Debugf("line-range: %d edits unlocated in %s", out.Unlocated, filePath)
		}
		return nil
	}
	sha := sha256.Sum256(content)
	shaHex := hex.EncodeToString(sha[:])
	for _, r := range out.Ranges {
		if _, err := db.Exec(`
			INSERT INTO event_line_ranges (
				id, event_id, file_path, rel_file_path, blob_sha_after,
				start_line, end_line, new_text_lines
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			uuid.NewString(), eventID, filePath, relFilePath, shaHex,
			r.StartLine, r.EndLine, r.NewTextLines); err != nil {
			return err
		}
	}
	target := filePath
	if relFilePath.Valid {
		target = relFilePath.String
	}
	if out.Unlocated > 0 {
		logx.Debugf("line-range: captured %d range(s) for %s on %s (%d unlocated)",
			len(out.Ranges), toolName, target, out.Unlocated)
	} else {
		logx.Debugf("line-range: captured %d range(s) for %s on %s",
			len(out.Ranges), toolName, target)
	}
	return nil
}

func upsertRepository(db *store.DB, rootPath, remoteURL, name string) (string, error) {
	var existing sql.NullString
	err := db.QueryRow(`SELECT id FROM repositories WHERE root_path = ?`, rootPath).Scan(&existing)
	if err == nil && existing.Valid {
		return existing.String, nil
	}
	if err != nil && err != sql.ErrNoRows {
		return "", err
	}
	id := uuid.NewString()
	_, err = db.Exec(`
		INSERT INTO repositories (id, org_id, remote_url, name, root_path)
		VALUES (?, ?, ?, ?, ?)`,
		id, store.LocalOrgID, nullableString(remoteURL), name, rootPath)
	if err != nil {
		// Race: try one more read.
		var got sql.NullString
		if e2 := db.QueryRow(`SELECT id FROM repositories WHERE root_path = ?`, rootPath).Scan(&got); e2 == nil && got.Valid {
			return got.String, nil
		}
		return "", err
	}
	return id, nil
}

func upsertRepositoryByCwd(db *store.DB, rootPath string) (string, error) {
	remoteURL := gitx.Run(rootPath, "config", "--get", "remote.origin.url")
	name := filepath.Base(rootPath)
	if remoteURL != "" {
		name = guessRepoNameFromRemote(remoteURL)
	}
	return upsertRepository(db, rootPath, remoteURL, name)
}

func resolveSessionID(db *store.DB, agentSlug, rawSessionID string) string {
	var existing sql.NullString
	err := db.QueryRow(`SELECT agent_slug FROM sessions WHERE id = ?`, rawSessionID).Scan(&existing)
	if err == sql.ErrNoRows || !existing.Valid {
		return rawSessionID
	}
	if existing.String == agentSlug {
		return rawSessionID
	}
	return agentSlug + ":" + rawSessionID
}

func extractFilePath(payload map[string]any) string {
	if ti, ok := payload["tool_input"].(map[string]any); ok {
		if v, ok := ti["file_path"].(string); ok && v != "" {
			return v
		}
	}
	if fps, ok := payload["file_paths"].([]any); ok {
		for _, v := range fps {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	if v, ok := payload["file_path"].(string); ok && v != "" {
		return v
	}
	return ""
}

func extractWorkspaceRoots(payload map[string]any) []string {
	roots, _ := payload["workspace_roots"].([]any)
	var out []string
	for _, r := range roots {
		if s, ok := r.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

func applyPatchFilesForPayload(payload map[string]any) []lineranges.ApplyPatchFile {
	if payload["tool_name"] != "apply_patch" {
		return nil
	}
	ti, ok := payload["tool_input"].(map[string]any)
	if !ok {
		return nil
	}
	cmd, _ := ti["command"].(string)
	if cmd == "" {
		return nil
	}
	return lineranges.ParseApplyPatch(cmd)
}

func resolveEventFilePath(filePath, cwd string, workspaceRoots []string) string {
	if filepath.IsAbs(filePath) {
		return filePath
	}
	var bases []string
	if cwd != "" && gitx.FindRepoRoot(cwd) != "" {
		bases = append(bases, cwd)
		bases = append(bases, workspaceRoots...)
	} else {
		bases = append(bases, workspaceRoots...)
		if cwd != "" {
			bases = append(bases, cwd)
		}
	}
	if len(bases) == 0 {
		return filePath
	}
	// Prefer the first existing candidate; fall back to the first candidate.
	for _, base := range bases {
		candidate := filepath.Clean(filepath.Join(base, filePath))
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return filepath.Clean(filepath.Join(bases[0], filePath))
}

func findEventRepoRoot(cwd, canonicalFilePath string, workspaceRoots []string) string {
	if canonicalFilePath != "" {
		if r := gitx.FindRepoRoot(canonicalFilePath); r != "" {
			return r
		}
	}
	for _, root := range workspaceRoots {
		if r := gitx.FindRepoRoot(root); r != "" {
			return r
		}
	}
	if cwd != "" {
		return gitx.FindRepoRoot(cwd)
	}
	return ""
}

func lineRangeToolForEvent(agentSlug, eventType string, payload map[string]any) string {
	if eventType == "PostToolUse" {
		toolName, _ := payload["tool_name"].(string)
		if toolName == "" {
			return ""
		}
		if agentSlug == "cursor" && editTools[toolName] {
			// Cursor's afterFileEdit already captured ranges for these.
			return ""
		}
		if editTools[toolName] {
			return toolName
		}
		return ""
	}
	if eventType != "afterFileEdit" {
		return ""
	}
	if _, ok := payload["edits"].([]any); ok {
		return "MultiEdit"
	}
	if _, ok := payload["new_string"].(string); ok {
		return "Edit"
	}
	if _, ok := payload["content"].(string); ok {
		return "Write"
	}
	return ""
}

func lineRangeToolInput(payload map[string]any) *lineranges.ToolInput {
	// Prefer tool_input (Claude/Codex shape).
	if ti, ok := payload["tool_input"].(map[string]any); ok {
		out := lineranges.ToolInput{}
		if v, ok := ti["file_path"].(string); ok {
			out.FilePath = v
		}
		if v, ok := ti["old_string"].(string); ok {
			out.OldString = v
		}
		if v, ok := ti["new_string"].(string); ok {
			out.NewString = v
		}
		if v, ok := ti["content"].(string); ok {
			out.Content = v
		}
		if edits, ok := ti["edits"].([]any); ok {
			for _, e := range edits {
				em, ok := e.(map[string]any)
				if !ok {
					continue
				}
				oldStr, _ := em["old_string"].(string)
				newStr, _ := em["new_string"].(string)
				out.Edits = append(out.Edits, lineranges.Edit{OldString: oldStr, NewString: newStr})
			}
		}
		return &out
	}
	// Fall back to a flat synthesis (Cursor afterFileEdit shape).
	out := lineranges.ToolInput{}
	found := false
	if v, ok := payload["file_path"].(string); ok {
		out.FilePath = v
		found = true
	}
	if v, ok := payload["old_string"].(string); ok {
		out.OldString = v
		found = true
	}
	if v, ok := payload["new_string"].(string); ok {
		out.NewString = v
		found = true
	}
	if v, ok := payload["content"].(string); ok {
		out.Content = v
		found = true
	}
	if edits, ok := payload["edits"].([]any); ok {
		for _, e := range edits {
			em, ok := e.(map[string]any)
			if !ok {
				continue
			}
			oldStr, _ := em["old_string"].(string)
			newStr, _ := em["new_string"].(string)
			out.Edits = append(out.Edits, lineranges.Edit{OldString: oldStr, NewString: newStr})
		}
		if len(out.Edits) > 0 {
			found = true
		}
	}
	if !found {
		return nil
	}
	return &out
}

var remoteNameRE = regexp.MustCompile(`[\/:]([^/]+?)(\.git)?$`)

func guessRepoNameFromRemote(remote string) string {
	m := remoteNameRE.FindStringSubmatch(remote)
	if len(m) >= 2 {
		return m[1]
	}
	return remote
}

func pickString(payload map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := payload[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func normalizeEventType(et string) string {
	switch et {
	case "preToolUse":
		return "PreToolUse"
	case "postToolUse":
		return "PostToolUse"
	}
	return et
}

func nullableString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func readStdin() ([]byte, error) {
	// If stdin is a TTY, treat as empty (matches the TS behavior — a
	// terminal-attached run is almost always a misfire).
	info, err := os.Stdin.Stat()
	if err == nil && (info.Mode()&os.ModeCharDevice) != 0 {
		return nil, nil
	}
	return io.ReadAll(os.Stdin)
}
