package store

import "database/sql"

// RefreshCommitAttributions rebuilds commit_sessions and commit_session_files
// for one commit (when commitID != "") or all commits. The attribution
// boundary is the next commit that touches the same file, so a single
// session's edits across multiple files get attributed to whichever commit
// landed each file.
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

	insertFiles := attributedRangesCTE + `
		INSERT INTO commit_session_files (
			commit_id, session_id, file_path, first_event_ts, last_event_ts,
			event_count, lines_attributed
		)
		SELECT
			commit_id,
			session_id,
			rel_file_path,
			MIN(event_ts),
			MAX(event_ts),
			COUNT(DISTINCT event_id),
			COALESCE(SUM(new_text_lines), 0)
		FROM attributed_ranges
		GROUP BY commit_id, session_id, rel_file_path`
	if _, err := db.Exec(insertFiles, commitArg, commitArg); err != nil {
		return 0, err
	}

	insertSessions := `
		INSERT INTO commit_sessions (
			commit_id, session_id, agent_slug, model_slug, first_event_ts, last_event_ts,
			event_count, files_touched, lines_attributed, attribution_source, confidence
		)
		SELECT
			csf.commit_id,
			csf.session_id,
			COALESCE(s.agent_slug, (
				SELECT e.agent_slug
				FROM events e
				WHERE e.session_id = csf.session_id
				ORDER BY datetime(e.ts) DESC
				LIMIT 1
			), 'unknown') AS agent_slug,
			COALESCE(s.model_slug, (
				SELECT json_extract(e.payload, '$.model')
				FROM events e
				WHERE e.session_id = csf.session_id
				  AND json_extract(e.payload, '$.model') IS NOT NULL
				ORDER BY datetime(e.ts) DESC
				LIMIT 1
			)) AS model_slug,
			MIN(csf.first_event_ts),
			MAX(csf.last_event_ts),
			SUM(csf.event_count),
			COUNT(DISTINCT csf.file_path),
			COALESCE(SUM(csf.lines_attributed), 0),
			'event_line_ranges',
			'line_range_next_file_commit'
		FROM commit_session_files csf
		LEFT JOIN sessions s ON s.id = csf.session_id
		WHERE (? IS NULL OR csf.commit_id = ?)
		GROUP BY csf.commit_id, csf.session_id`
	if _, err := db.Exec(insertSessions, commitArg, commitArg); err != nil {
		return 0, err
	}

	var n sql.NullInt64
	err := db.QueryRow(
		`SELECT COUNT(*) FROM commit_sessions WHERE (? IS NULL OR commit_id = ?)`,
		commitArg, commitArg,
	).Scan(&n)
	if err != nil {
		return 0, err
	}
	return int(n.Int64), nil
}
