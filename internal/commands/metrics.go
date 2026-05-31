package commands

import (
	"database/sql"
	"fmt"

	"github.com/bilanc/posthook/internal/store"

	"github.com/spf13/cobra"
)

// SQLite expression: count newlines in a TEXT column.
const nl = `(length(%s) - length(replace(%s, char(10), '')))`

// activeRepoExists filters out clone/test-only repos with zero AI activity.
const activeRepoExists = `(
	EXISTS (
		SELECT 1 FROM events active_events
		WHERE active_events.repo_id = c.repo_id
		  AND active_events.event_type != 'hook_misfire'
	)
	OR EXISTS (
		SELECT 1 FROM sessions active_sessions
		WHERE active_sessions.repo_id = c.repo_id
	)
	OR EXISTS (
		SELECT 1
		FROM commit_sessions active_cs
		JOIN commits active_c ON active_c.id = active_cs.commit_id
		WHERE active_c.repo_id = c.repo_id
	)
)`

func aiEditsCTE() string {
	return `
WITH ai_edits AS (
	SELECT
		e.id, e.ts, e.agent_slug, e.session_id, e.cwd, e.repo_id, e.rel_file_path,
		json_extract(e.payload, '$.tool_name') AS tool_name,
		COALESCE(s.model_slug, json_extract(e.payload, '$.model'), 'unknown') AS model_slug,
		COALESCE(json_extract(e.payload, '$.tool_input.new_string'), '') AS new_string,
		COALESCE(json_extract(e.payload, '$.tool_input.old_string'), '') AS old_string,
		COALESCE(json_extract(e.payload, '$.tool_input.content'), '') AS content,
		COALESCE(e.lines_added, 0) AS event_lines_added,
		COALESCE(e.lines_removed, 0) AS event_lines_removed,
		COALESCE((
			SELECT SUM(elr.new_text_lines)
			FROM event_line_ranges elr
			WHERE elr.event_id = e.id
		), 0) AS line_range_lines
	FROM events e
	LEFT JOIN sessions s ON s.id = e.session_id
	WHERE (
		(
			e.event_type IN ('PostToolUse', 'postToolUse')
			AND json_extract(e.payload, '$.tool_name') IN ('Edit', 'Write', 'MultiEdit', 'apply_patch')
			AND NOT (
				e.agent_slug = 'cursor'
				AND EXISTS (
					SELECT 1
					FROM events afe
					WHERE afe.agent_slug = 'cursor'
					  AND afe.event_type = 'afterFileEdit'
					  AND afe.file_path = e.file_path
					  AND (
						afe.session_id = e.session_id
						OR (afe.session_id IS NULL AND e.session_id IS NULL)
					  )
					  AND ABS((julianday(afe.ts) - julianday(e.ts)) * 86400.0) <= 5
				)
			)
		)
		OR e.event_type = 'afterFileEdit'
	)
),
ai_edits_scored AS (
	SELECT
		*,
		CASE
			WHEN line_range_lines > 0 THEN line_range_lines
			WHEN tool_name = 'apply_patch' THEN event_lines_added
			ELSE ` + fmt.Sprintf(nl, "new_string", "new_string") + ` + ` + fmt.Sprintf(nl, "content", "content") + `
		END AS lines_generated,
		CASE
			WHEN tool_name = 'apply_patch' THEN event_lines_removed
			ELSE ` + fmt.Sprintf(nl, "old_string", "old_string") + `
		END AS lines_replaced
	FROM ai_edits
),
committed_ai AS (
	SELECT
		cs.commit_id,
		c.repo_id,
		c.lines_added AS commit_lines_added,
		cs.agent_slug,
		COALESCE(cs.model_slug, 'unknown') AS model_slug,
		cs.session_id,
		cs.lines_attributed AS lines_generated
	FROM commit_sessions cs
	JOIN commits c ON c.id = cs.commit_id
)`
}

type breakdownRow struct {
	key            string
	edits          int
	linesGenerated int
	linesCommitted int
	sessions       int
	topModel       sql.NullString
}

func newMetricsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "metrics",
		Short: "Show AI metrics with breakdowns by agent/model/repo",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMetrics()
		},
	}
}

func runMetrics() error {
	db, err := store.Open()
	if err != nil {
		return err
	}

	var summary struct {
		edits, linesGenerated, linesReplaced, linesCommitted, sessions int
	}
	err = db.QueryRow(aiEditsCTE() + `
		SELECT
			(SELECT COUNT(*) FROM ai_edits_scored),
			(SELECT COALESCE(SUM(lines_generated), 0) FROM ai_edits_scored),
			(SELECT COALESCE(SUM(lines_replaced), 0) FROM ai_edits_scored),
			(SELECT COALESCE(SUM(lines_generated), 0) FROM committed_ai),
			(SELECT COUNT(DISTINCT session_id) FROM ai_edits_scored)`).
		Scan(&summary.edits, &summary.linesGenerated, &summary.linesReplaced,
			&summary.linesCommitted, &summary.sessions)
	if err != nil {
		return err
	}

	var totalHours float64
	err = db.QueryRow(`
		SELECT COALESCE(SUM(duration_hours), 0) AS total_hours
		FROM (
			SELECT
				s.id,
				(julianday(
					COALESCE(s.ended_at, (SELECT MAX(ts) FROM events e WHERE e.session_id = s.id))
				) - julianday(
					COALESCE(s.started_at, (SELECT MIN(ts) FROM events e WHERE e.session_id = s.id))
				)) * 24 AS duration_hours
			FROM sessions s
		)
		WHERE duration_hours IS NOT NULL`).Scan(&totalHours)
	if err != nil {
		return err
	}

	var maxConcurrent int
	err = db.QueryRow(`
		WITH session_spans AS (
			SELECT
				s.id,
				COALESCE(s.started_at, (SELECT MIN(ts) FROM events e WHERE e.session_id = s.id)) AS start_ts,
				COALESCE(s.ended_at, (SELECT MAX(ts) FROM events e WHERE e.session_id = s.id)) AS end_ts
			FROM sessions s
		)
		SELECT COALESCE(MAX(concurrent), 0)
		FROM (
			SELECT a.id,
			       (SELECT COUNT(*) FROM session_spans b
			        WHERE b.start_ts <= a.end_ts AND b.end_ts >= a.start_ts) AS concurrent
			FROM session_spans a
		)`).Scan(&maxConcurrent)
	if err != nil {
		return err
	}

	var topModelSlug sql.NullString
	var topModelN int
	err = db.QueryRow(aiEditsCTE() + `
		SELECT model_slug, COUNT(*) AS n
		FROM ai_edits_scored
		GROUP BY model_slug
		ORDER BY n DESC
		LIMIT 1`).Scan(&topModelSlug, &topModelN)
	if err != nil && err != sql.ErrNoRows {
		return err
	}

	var commitN, commitAdded, commitRemoved int
	err = db.QueryRow(`
		SELECT
			COUNT(*),
			COALESCE(SUM(lines_added), 0),
			COALESCE(SUM(lines_removed), 0)
		FROM commits c
		WHERE ` + activeRepoExists).Scan(&commitN, &commitAdded, &commitRemoved)
	if err != nil {
		return err
	}

	aiCodePct := 0.0
	if commitAdded > 0 {
		aiCodePct = float64(summary.linesCommitted) / float64(commitAdded) * 100
		if aiCodePct > 100 {
			aiCodePct = 100
		}
	}

	fmt.Println("posthook metrics")
	fmt.Println()
	fmt.Println("Overall")
	fmt.Printf("  AI edit events            %d\n", summary.edits)
	fmt.Printf("  AI lines generated        %d  (ranges + tool payload)\n", summary.linesGenerated)
	fmt.Printf("  AI lines replaced         %d\n", summary.linesReplaced)
	fmt.Printf("  AI lines committed*       %d\n", summary.linesCommitted)
	fmt.Printf("  AI code %%*                %.1f%%\n", aiCodePct)
	fmt.Printf("  Working hours             %.2f\n", totalHours)
	fmt.Printf("  Distinct sessions         %d\n", summary.sessions)
	fmt.Printf("  Max concurrent sessions   %d\n", maxConcurrent)
	if topModelSlug.Valid && topModelN > 0 {
		fmt.Printf("  Top model                 %s (%d edits)\n", topModelSlug.String, topModelN)
	} else {
		fmt.Println("  Top model                 (no data)")
	}
	fmt.Printf("  Commits analyzed          %d  (+%d / -%d lines)\n", commitN, commitAdded, commitRemoved)
	fmt.Println()

	for _, b := range []struct {
		title string
		fn    func(*store.DB) ([]breakdownRow, error)
	}{
		{"By agent", breakdownByAgent},
		{"By model", breakdownByModel},
		{"By repo", breakdownByRepo},
	} {
		rows, err := b.fn(db)
		if err != nil {
			return err
		}
		printBreakdown(b.title, rows)
	}

	fmt.Println("Notes")
	fmt.Println("  * AI lines committed = sum of AI line ranges attributed to captured commits.")
	fmt.Println("    Attribution links commits to sessions by file-level next-commit ownership.")
	fmt.Println("  • Commit totals exclude clone/test-only repos with no sessions, events, or AI attribution.")
	fmt.Println("  • Older events (before the v2 migration) lack repo_id and won't link to commits.")
	return nil
}

func breakdownByAgent(db *store.DB) ([]breakdownRow, error) {
	return readBreakdown(db, aiEditsCTE()+`
		SELECT
			e.agent_slug AS key,
			COUNT(*) AS edits,
			COALESCE(SUM(e.lines_generated), 0) AS lines_generated,
			COALESCE((SELECT SUM(ca.lines_generated) FROM committed_ai ca WHERE ca.agent_slug = e.agent_slug), 0) AS lines_committed,
			COUNT(DISTINCT e.session_id) AS sessions,
			(
				SELECT model_slug FROM ai_edits_scored b
				WHERE b.agent_slug = e.agent_slug
				GROUP BY model_slug ORDER BY COUNT(*) DESC LIMIT 1
			) AS top_model
		FROM ai_edits_scored e
		GROUP BY e.agent_slug
		ORDER BY edits DESC`)
}

func breakdownByModel(db *store.DB) ([]breakdownRow, error) {
	return readBreakdown(db, aiEditsCTE()+`
		SELECT
			e.model_slug AS key,
			COUNT(*) AS edits,
			COALESCE(SUM(e.lines_generated), 0) AS lines_generated,
			COALESCE((SELECT SUM(ca.lines_generated) FROM committed_ai ca WHERE ca.model_slug = e.model_slug), 0) AS lines_committed,
			COUNT(DISTINCT e.session_id) AS sessions,
			NULL AS top_model
		FROM ai_edits_scored e
		GROUP BY e.model_slug
		ORDER BY edits DESC`)
}

func breakdownByRepo(db *store.DB) ([]breakdownRow, error) {
	return readBreakdown(db, aiEditsCTE()+`
		SELECT
			COALESCE(r.name, '(no repo)') AS key,
			COUNT(*) AS edits,
			COALESCE(SUM(e.lines_generated), 0) AS lines_generated,
			COALESCE((SELECT SUM(ca.lines_generated) FROM committed_ai ca WHERE ca.repo_id = e.repo_id), 0) AS lines_committed,
			COUNT(DISTINCT e.session_id) AS sessions,
			(
				SELECT model_slug FROM ai_edits_scored b
				WHERE b.repo_id = e.repo_id
				GROUP BY model_slug ORDER BY COUNT(*) DESC LIMIT 1
			) AS top_model
		FROM ai_edits_scored e
		LEFT JOIN repositories r ON r.id = e.repo_id
		GROUP BY e.repo_id
		ORDER BY edits DESC`)
}

func readBreakdown(db *store.DB, query string) ([]breakdownRow, error) {
	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []breakdownRow
	for rows.Next() {
		var r breakdownRow
		if err := rows.Scan(&r.key, &r.edits, &r.linesGenerated, &r.linesCommitted, &r.sessions, &r.topModel); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func printBreakdown(title string, rows []breakdownRow) {
	fmt.Println(title)
	if len(rows) == 0 {
		fmt.Println("  (no data)")
		fmt.Println()
		return
	}
	keyCol := 16
	modelCol := 12
	for _, r := range rows {
		if l := len(r.key); l > keyCol {
			keyCol = l
		}
		if r.topModel.Valid {
			if l := len(r.topModel.String); l > modelCol {
				modelCol = l
			}
		}
	}
	fmt.Printf("  %-*s  %6s  %7s  %6s  %4s  %-*s\n",
		keyCol, "key", "edits", "gen", "commit", "sess", modelCol, "top model")
	for _, r := range rows {
		model := "—"
		if r.topModel.Valid && r.topModel.String != "" {
			model = r.topModel.String
		}
		fmt.Printf("  %-*s  %6d  %7d  %6d  %4d  %-*s\n",
			keyCol, r.key, r.edits, r.linesGenerated, r.linesCommitted, r.sessions, modelCol, model)
	}
	fmt.Println()
}
