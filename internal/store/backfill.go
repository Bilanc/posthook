package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/bilanc/posthook/internal/gitx"
	"github.com/bilanc/posthook/internal/lineranges"
	"github.com/bilanc/posthook/internal/transcript"
	"github.com/google/uuid"
)

// backfillEventSessions creates session rows for orphan events. When two
// agents emit the same session_id (rare but observed), we disambiguate by
// prefixing with the agent slug.
func (db *DB) backfillEventSessions() (int, error) {
	rows, err := db.Query(`
		SELECT
			e.agent_slug,
			e.session_id AS raw_session_id,
			CASE
				WHEN s.id IS NULL THEN e.session_id
				ELSE e.agent_slug || ':' || e.session_id
			END AS session_id,
			MIN(e.ts) AS first_ts,
			MAX(e.ts) AS last_ts,
			(
				SELECT json_extract(e2.payload, '$.model')
				FROM events e2
				WHERE e2.agent_slug = e.agent_slug
				  AND e2.session_id = e.session_id
				  AND json_extract(e2.payload, '$.model') IS NOT NULL
				ORDER BY datetime(e2.ts) DESC
				LIMIT 1
			) AS model_slug,
			(
				SELECT e2.repo_id
				FROM events e2
				WHERE e2.agent_slug = e.agent_slug
				  AND e2.session_id = e.session_id
				  AND e2.repo_id IS NOT NULL
				ORDER BY datetime(e2.ts) DESC
				LIMIT 1
			) AS repo_id,
			(
				SELECT e2.cwd
				FROM events e2
				WHERE e2.agent_slug = e.agent_slug
				  AND e2.session_id = e.session_id
				  AND e2.cwd IS NOT NULL
				ORDER BY datetime(e2.ts) DESC
				LIMIT 1
			) AS cwd
		FROM events e
		LEFT JOIN sessions s ON s.id = e.session_id
		WHERE e.session_id IS NOT NULL
		  AND (s.id IS NULL OR s.agent_slug != e.agent_slug)
		GROUP BY e.agent_slug, e.session_id, s.id`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	type group struct {
		agentSlug    string
		rawSessionID string
		sessionID    string
		firstTS      string
		lastTS       string
		model        sql.NullString
		repoID       sql.NullString
		cwd          sql.NullString
	}
	var groups []group
	for rows.Next() {
		var g group
		if err := rows.Scan(&g.agentSlug, &g.rawSessionID, &g.sessionID,
			&g.firstTS, &g.lastTS, &g.model, &g.repoID, &g.cwd); err != nil {
			return 0, err
		}
		groups = append(groups, g)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	touched := 0
	for _, g := range groups {
		_, err := db.Exec(`
			INSERT INTO sessions (id, org_id, agent_slug, model_slug, repo_id, cwd, started_at, ended_at)
			VALUES (?, 'local', ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				model_slug = COALESCE(sessions.model_slug, excluded.model_slug),
				repo_id = COALESCE(sessions.repo_id, excluded.repo_id),
				cwd = COALESCE(sessions.cwd, excluded.cwd),
				started_at = CASE
					WHEN datetime(excluded.started_at) < datetime(sessions.started_at)
						THEN excluded.started_at
					ELSE sessions.started_at
				END,
				ended_at = CASE
					WHEN sessions.ended_at IS NULL THEN excluded.ended_at
					WHEN excluded.ended_at IS NULL THEN sessions.ended_at
					WHEN datetime(excluded.ended_at) > datetime(sessions.ended_at)
						THEN excluded.ended_at
					ELSE sessions.ended_at
				END`,
			g.sessionID, g.agentSlug, g.model, g.repoID, g.cwd, g.firstTS, g.lastTS)
		if err != nil {
			return 0, err
		}

		if g.sessionID != g.rawSessionID {
			if _, err := db.Exec(`
				UPDATE commit_session_files
				SET session_id = ?, synced_at = NULL
				WHERE session_id = ?
				  AND EXISTS (
					SELECT 1
					FROM commit_sessions cs
					WHERE cs.commit_id = commit_session_files.commit_id
					  AND cs.session_id = commit_session_files.session_id
					  AND cs.agent_slug = ?
				  )`,
				g.sessionID, g.rawSessionID, g.agentSlug); err != nil {
				return 0, err
			}
			if _, err := db.Exec(`
				UPDATE commit_sessions
				SET session_id = ?, synced_at = NULL
				WHERE session_id = ? AND agent_slug = ?`,
				g.sessionID, g.rawSessionID, g.agentSlug); err != nil {
				return 0, err
			}
			if _, err := db.Exec(`
				UPDATE events
				SET session_id = ?, synced_at = NULL
				WHERE agent_slug = ? AND session_id = ?`,
				g.sessionID, g.agentSlug, g.rawSessionID); err != nil {
				return 0, err
			}
		}
		touched++
	}
	return touched, nil
}

// backfillEventRepos walks events with NULL repo_id, resolves their repo from
// file/workspace/cwd, and populates the column.
func (db *DB) backfillEventRepos() (int, error) {
	rows, err := db.Query(`
		SELECT id, cwd, file_path, payload
		FROM events
		WHERE repo_id IS NULL
		  AND (
			(cwd IS NOT NULL AND cwd != '')
			OR (file_path IS NOT NULL AND file_path != '')
		  )`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	type evt struct {
		id       string
		cwd      sql.NullString
		filePath sql.NullString
		payload  sql.NullString
	}
	var evts []evt
	for rows.Next() {
		var e evt
		if err := rows.Scan(&e.id, &e.cwd, &e.filePath, &e.payload); err != nil {
			return 0, err
		}
		evts = append(evts, e)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	type resolved struct {
		id   string
		root string
	}
	cache := map[string]resolved{}
	touched := 0

	for _, e := range evts {
		workspaceRoots := extractWorkspaceRoots(e.payload.String)
		applyPatchFiles := applyPatchFilesForPayload(e.payload.String)
		filePath := e.filePath.String
		if filePath == "" && len(applyPatchFiles) > 0 {
			filePath = applyPatchFiles[0].FilePath
		}
		var eventFilePath string
		if filePath != "" {
			eventFilePath = gitx.Canonicalize(resolveEventFilePath(filePath, e.cwd.String, workspaceRoots))
		}
		root := findEventRepoRoot(e.cwd.String, eventFilePath, workspaceRoots)
		if root == "" {
			continue
		}
		r, ok := cache[root]
		if !ok {
			id, err := db.lookupOrCreateRepoByRoot(root)
			if err != nil {
				return 0, err
			}
			r = resolved{id: id, root: root}
			cache[root] = r
		}
		var rel sql.NullString
		if eventFilePath != "" {
			if v := gitx.RelPathInRepo(r.root, eventFilePath); v != "" {
				rel = sql.NullString{String: v, Valid: true}
			}
		}
		if _, err := db.Exec(`UPDATE events SET repo_id = ?, rel_file_path = ?, synced_at = NULL WHERE id = ?`,
			r.id, rel, e.id); err != nil {
			return 0, err
		}
		touched++
	}
	return touched, nil
}

// backfillLineRangeRelPaths copies rel_file_path from events into orphan
// event_line_ranges rows that lack it (early versions didn't populate it).
func (db *DB) backfillLineRangeRelPaths() (int, error) {
	res, err := db.Exec(`
		UPDATE event_line_ranges
		SET rel_file_path = (
			SELECT e.rel_file_path FROM events e WHERE e.id = event_line_ranges.event_id
		),
		synced_at = NULL
		WHERE rel_file_path IS NULL
		  AND EXISTS (
			SELECT 1 FROM events e
			WHERE e.id = event_line_ranges.event_id
			  AND e.rel_file_path IS NOT NULL
		  )`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// backfillAfterFileEditLineRanges retroactively captures line ranges for
// Cursor afterFileEdit events that pre-date our range-capture logic.
func (db *DB) backfillAfterFileEditLineRanges() (int, error) {
	rows, err := db.Query(`
		SELECT e.id, e.file_path, e.rel_file_path, e.payload
		FROM events e
		WHERE e.event_type = 'afterFileEdit'
		  AND e.file_path IS NOT NULL
		  AND e.payload IS NOT NULL
		  AND NOT EXISTS (
			SELECT 1 FROM event_line_ranges elr WHERE elr.event_id = e.id
		  )`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	type row struct {
		id       string
		filePath string
		rel      sql.NullString
		payload  string
	}
	var todo []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.filePath, &r.rel, &r.payload); err != nil {
			return 0, err
		}
		todo = append(todo, r)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	inserted := 0
	for _, r := range todo {
		if _, err := os.Stat(r.filePath); err != nil {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(r.payload), &payload); err != nil {
			continue
		}
		input := lineRangeToolInput(payload)
		if input == nil {
			continue
		}
		content, err := os.ReadFile(r.filePath)
		if err != nil {
			continue
		}
		out := lineranges.Extract("MultiEdit", *input, string(content))
		if len(out.Ranges) == 0 {
			continue
		}
		sha := sha256.Sum256(content)
		shaHex := hex.EncodeToString(sha[:])
		for _, rng := range out.Ranges {
			if _, err := db.Exec(`
				INSERT INTO event_line_ranges (
					id, event_id, file_path, rel_file_path, blob_sha_after,
					start_line, end_line, new_text_lines
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				uuid.NewString(), r.id, r.filePath, r.rel, shaHex,
				rng.StartLine, rng.EndLine, rng.NewTextLines); err != nil {
				return 0, err
			}
			inserted++
		}
	}
	return inserted, nil
}

// backfillApplyPatchLineRanges captures ranges for codex apply_patch events
// that pre-date our range-capture logic.
func (db *DB) backfillApplyPatchLineRanges() (int, error) {
	rows, err := db.Query(`
		SELECT e.id, e.cwd, e.payload
		FROM events e
		WHERE e.event_type = 'PostToolUse'
		  AND e.payload IS NOT NULL
		  AND json_extract(e.payload, '$.tool_name') = 'apply_patch'
		  AND NOT EXISTS (
			SELECT 1 FROM event_line_ranges elr WHERE elr.event_id = e.id
		  )`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	type row struct {
		id      string
		cwd     sql.NullString
		payload string
	}
	var todo []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.cwd, &r.payload); err != nil {
			return 0, err
		}
		todo = append(todo, r)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	inserted := 0
	for _, r := range todo {
		files := applyPatchFilesForPayload(r.payload)
		if len(files) == 0 {
			continue
		}
		workspaceRoots := extractWorkspaceRoots(r.payload)
		linesAdded := 0
		linesRemoved := 0
		for _, f := range files {
			linesAdded += f.LinesAdded
			linesRemoved += f.LinesRemoved
		}
		var firstFilePath string
		var firstRepoID sql.NullString
		var firstRel sql.NullString

		for _, file := range files {
			if len(file.Edits) == 0 {
				continue
			}
			resolvedPath := gitx.Canonicalize(resolveEventFilePath(file.FilePath, r.cwd.String, workspaceRoots))
			if _, err := os.Stat(resolvedPath); err != nil {
				continue
			}
			repoRoot := findEventRepoRoot(r.cwd.String, resolvedPath, workspaceRoots)
			var rel sql.NullString
			if repoRoot != "" {
				if v := gitx.RelPathInRepo(repoRoot, resolvedPath); v != "" {
					rel = sql.NullString{String: v, Valid: true}
				}
			}
			if firstFilePath == "" {
				firstFilePath = resolvedPath
				firstRel = rel
				if repoRoot != "" {
					id, err := db.lookupOrCreateRepoByRoot(repoRoot)
					if err != nil {
						return 0, err
					}
					firstRepoID = sql.NullString{String: id, Valid: true}
				}
			}
			content, err := os.ReadFile(resolvedPath)
			if err != nil {
				continue
			}
			out := lineranges.Extract("MultiEdit", lineranges.ToolInput{Edits: file.Edits}, string(content))
			if len(out.Ranges) == 0 {
				continue
			}
			sha := sha256.Sum256(content)
			shaHex := hex.EncodeToString(sha[:])
			for _, rng := range out.Ranges {
				if _, err := db.Exec(`
					INSERT INTO event_line_ranges (
						id, event_id, file_path, rel_file_path, blob_sha_after,
						start_line, end_line, new_text_lines
					) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
					uuid.NewString(), r.id, resolvedPath, rel, shaHex,
					rng.StartLine, rng.EndLine, rng.NewTextLines); err != nil {
					return 0, err
				}
				inserted++
			}
		}

		if _, err := db.Exec(`
			UPDATE events
			SET file_path = COALESCE(file_path, ?),
				repo_id = COALESCE(repo_id, ?),
				rel_file_path = COALESCE(rel_file_path, ?),
				lines_added = COALESCE(lines_added, ?),
				lines_removed = COALESCE(lines_removed, ?),
				synced_at = NULL
			WHERE id = ?`,
			nullableOf(firstFilePath), firstRepoID, firstRel,
			linesAdded, linesRemoved, r.id); err != nil {
			return 0, err
		}
	}
	return inserted, nil
}

// deleteDuplicateCursorPostToolUseEditRanges removes range rows that duplicate
// what afterFileEdit already captured. Cursor fires both PostToolUse AND
// afterFileEdit for the same edit; the afterFileEdit one is canonical because
// it has the post-write file content.
func (db *DB) deleteDuplicateCursorPostToolUseEditRanges() (int, error) {
	res, err := db.Exec(`
		DELETE FROM event_line_ranges
		WHERE event_id IN (
			SELECT p.id
			FROM events p
			WHERE p.agent_slug = 'cursor'
			  AND p.event_type = 'PostToolUse'
			  AND json_extract(p.payload, '$.tool_name') IN ('Edit', 'Write', 'MultiEdit')
			  AND p.file_path IS NOT NULL
			  AND EXISTS (
				SELECT 1
				FROM events afe
				WHERE afe.agent_slug = 'cursor'
				  AND afe.event_type = 'afterFileEdit'
				  AND afe.file_path = p.file_path
				  AND (
					afe.session_id = p.session_id
					OR (afe.session_id IS NULL AND p.session_id IS NULL)
				  )
				  AND ABS((julianday(afe.ts) - julianday(p.ts)) * 86400.0) <= 5
			  )
		)`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// backfillSessionRepos picks the most common events.repo_id for sessions
// missing one.
func (db *DB) backfillSessionRepos() (int, error) {
	rows, err := db.Query(`SELECT id FROM sessions WHERE repo_id IS NULL`)
	if err != nil {
		return 0, err
	}
	var sessionIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		sessionIDs = append(sessionIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	touched := 0
	for _, id := range sessionIDs {
		var repoID sql.NullString
		err := db.QueryRow(`
			SELECT repo_id
			FROM events
			WHERE session_id = ? AND repo_id IS NOT NULL
			GROUP BY repo_id
			ORDER BY COUNT(*) DESC
			LIMIT 1`, id).Scan(&repoID)
		if err == sql.ErrNoRows || !repoID.Valid {
			continue
		}
		if err != nil && err != sql.ErrNoRows {
			return 0, err
		}
		if _, err := db.Exec(`UPDATE sessions SET repo_id = ?, synced_at = NULL WHERE id = ?`, repoID, id); err != nil {
			return 0, err
		}
		touched++
	}
	return touched, nil
}

// backfillSessionModels parses the on-disk Claude transcript for sessions
// with NULL model_slug to recover the model name (Stop hook payloads don't
// carry it).
func (db *DB) backfillSessionModels() (int, error) {
	rows, err := db.Query(`SELECT id FROM sessions WHERE model_slug IS NULL OR model_slug = ''`)
	if err != nil {
		return 0, err
	}
	var sessionIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		sessionIDs = append(sessionIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	touched := 0
	for _, id := range sessionIDs {
		var path sql.NullString
		err := db.QueryRow(`
			SELECT json_extract(payload, '$.transcript_path') AS path
			FROM events
			WHERE session_id = ?
			  AND json_extract(payload, '$.transcript_path') IS NOT NULL
			ORDER BY ts DESC
			LIMIT 1`, id).Scan(&path)
		if err == sql.ErrNoRows || !path.Valid {
			continue
		}
		if err != nil {
			return 0, err
		}
		summary := transcript.ParseFile(path.String)
		if summary == nil || summary.Model == "" {
			continue
		}
		if _, err := db.Exec(`UPDATE sessions SET model_slug = ?, synced_at = NULL WHERE id = ?`, summary.Model, id); err != nil {
			return 0, err
		}
		touched++
	}
	return touched, nil
}

// backfillSessionTokens parses the on-disk transcript (Claude JSONL or Codex
// rollout) for sessions missing token counts. Cursor sessions never carry a
// transcript_path so they stay NULL — "agent doesn't report usage", not zero.
func (db *DB) backfillSessionTokens() (int, error) {
	rows, err := db.Query(`
		SELECT s.id, (
			SELECT json_extract(e.payload, '$.transcript_path')
			FROM events e
			WHERE e.session_id = s.id
			  AND json_extract(e.payload, '$.transcript_path') IS NOT NULL
			ORDER BY e.ts DESC
			LIMIT 1
		) AS path
		FROM sessions s
		WHERE s.input_tokens IS NULL`)
	if err != nil {
		return 0, err
	}
	type row struct {
		id   string
		path sql.NullString
	}
	var todo []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.path); err != nil {
			rows.Close()
			return 0, err
		}
		if r.path.Valid && r.path.String != "" {
			todo = append(todo, r)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	touched := 0
	for _, r := range todo {
		summary := transcript.ParseFile(r.path.String)
		if summary == nil || summary.Tokens == nil {
			continue
		}
		t := summary.Tokens
		if _, err := db.Exec(`
			UPDATE sessions SET
				input_tokens = ?, output_tokens = ?,
				cache_read_tokens = ?, cache_creation_tokens = ?,
				synced_at = NULL
			WHERE id = ?`,
			t.Input, t.Output, t.CacheRead, t.CacheCreation, r.id); err != nil {
			return 0, err
		}
		touched++
	}
	return touched, nil
}

// backfillSessionEngineers attributes a session to an engineer by matching
// commits whose committed_at falls inside the session's time window. If
// exactly one distinct author shows up we use it; zero or many → leave NULL
// rather than misattribute.
func (db *DB) backfillSessionEngineers() (int, error) {
	rows, err := db.Query(`
		SELECT s.id, s.repo_id, s.started_at,
		       COALESCE(s.ended_at, (SELECT MAX(ts) FROM events e WHERE e.session_id = s.id)) AS end_ts
		FROM sessions s
		WHERE s.engineer_email IS NULL
		  AND s.repo_id IS NOT NULL
		  AND s.started_at IS NOT NULL`)
	if err != nil {
		return 0, err
	}
	type row struct {
		id        string
		repoID    string
		startedAt string
		endTS     sql.NullString
	}
	var sessions []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.repoID, &r.startedAt, &r.endTS); err != nil {
			rows.Close()
			return 0, err
		}
		sessions = append(sessions, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	touched := 0
	for _, s := range sessions {
		if !s.endTS.Valid || s.endTS.String == s.startedAt {
			continue
		}
		authorRows, err := db.Query(`
			SELECT author_email, author_name, COUNT(*) AS n
			FROM commits
			WHERE repo_id = ?
			  AND author_email IS NOT NULL
			  AND datetime(committed_at) >= datetime(?)
			  AND datetime(committed_at) <= datetime(?)
			GROUP BY author_email, author_name`,
			s.repoID, s.startedAt, s.endTS.String)
		if err != nil {
			return 0, err
		}
		type author struct {
			email sql.NullString
			name  sql.NullString
		}
		var authors []author
		for authorRows.Next() {
			var a author
			var n int
			if err := authorRows.Scan(&a.email, &a.name, &n); err != nil {
				authorRows.Close()
				return 0, err
			}
			authors = append(authors, a)
		}
		authorRows.Close()
		if err := authorRows.Err(); err != nil {
			return 0, err
		}
		if len(authors) != 1 {
			continue
		}
		a := authors[0]
		if _, err := db.Exec(`UPDATE sessions SET engineer_email = ?, engineer_name = ?, synced_at = NULL WHERE id = ?`,
			a.email, a.name, s.id); err != nil {
			return 0, err
		}
		touched++
	}
	return touched, nil
}

// ── helpers shared between backfill passes ────────────────────────────────

func (db *DB) lookupOrCreateRepoByRoot(root string) (string, error) {
	var existing sql.NullString
	err := db.QueryRow(`SELECT id FROM repositories WHERE root_path = ?`, root).Scan(&existing)
	if err == nil && existing.Valid {
		return existing.String, nil
	}
	if err != nil && err != sql.ErrNoRows {
		return "", err
	}
	id := uuid.NewString()
	remoteURL := gitx.Run(root, "config", "--get", "remote.origin.url")
	if _, err := db.Exec(`
		INSERT INTO repositories (id, org_id, remote_url, name, root_path)
		VALUES (?, 'local', ?, ?, ?)
		ON CONFLICT(root_path) DO NOTHING`,
		id, nullableOf(remoteURL), filepath.Base(root), root); err != nil {
		return "", err
	}
	// Re-read in case of race or ON CONFLICT.
	var got string
	if err := db.QueryRow(`SELECT id FROM repositories WHERE root_path = ?`, root).Scan(&got); err != nil {
		return id, nil
	}
	return got, nil
}

func extractWorkspaceRoots(rawPayload string) []string {
	if rawPayload == "" {
		return nil
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(rawPayload), &payload); err != nil {
		return nil
	}
	roots, _ := payload["workspace_roots"].([]any)
	var out []string
	for _, r := range roots {
		if s, ok := r.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

func applyPatchFilesForPayload(rawPayload string) []lineranges.ApplyPatchFile {
	if rawPayload == "" {
		return nil
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(rawPayload), &payload); err != nil {
		return nil
	}
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

// resolveEventFilePath resolves a relative or absolute file path against the
// best base. cwd is preferred when it's inside a git repo; workspace_roots
// are used as fallbacks (Cursor runs hooks from ~/.cursor so cwd alone can't
// identify the project). Returns the first candidate without an existence
// check — backfill paths call this on rows where the file may already be
// gone, and Canonicalize handles non-existent paths gracefully downstream.
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

// lineRangeToolInput synthesizes a ToolInput from arbitrary payload shapes
// (Cursor's afterFileEdit is flatter than Claude's PostToolUse).
func lineRangeToolInput(payload map[string]any) *lineranges.ToolInput {
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
		for _, ev := range edits {
			em, ok := ev.(map[string]any)
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

