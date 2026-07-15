package store

import (
	"database/sql"
	"strings"

	"github.com/bilanc/posthook/internal/transcript"
	"github.com/google/uuid"
)

// UpsertSessionPrompts writes a session's ordered prompt list. seq is the
// 1-based position within the session; ON CONFLICT(session_id, seq) DO NOTHING
// makes repeated calls idempotent — a transcript only ever grows, so earlier
// seqs never change and each Stop re-parse just appends the new tail. Returns
// the number of newly inserted prompts.
func (db *DB) UpsertSessionPrompts(sessionID, agentSlug, source string, prompts []transcript.UserPrompt) (int, error) {
	inserted := 0
	for i, p := range prompts {
		text := strings.TrimSpace(p.Text)
		if text == "" {
			continue
		}
		res, err := db.Exec(`
			INSERT INTO session_prompts (id, org_id, session_id, ts, seq, agent_slug, source, prompt_text)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(session_id, seq) DO NOTHING`,
			uuid.NewString(), LocalOrgID, sessionID, nullableTS(p.TS), i+1,
			agentSlug, source, text)
		if err != nil {
			return inserted, err
		}
		if n, err := res.RowsAffected(); err == nil {
			inserted += int(n)
		}
	}
	return inserted, nil
}

// AppendHookPrompt records one prompt delivered directly by an agent hook
// (Cursor's beforeSubmitPrompt), assigning the next seq for the session.
// Hook fires are serialized by the single-connection pool, so MAX(seq)+1 is
// race-safe in practice.
func (db *DB) AppendHookPrompt(sessionID, agentSlug, ts, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	_, err := db.Exec(`
		INSERT INTO session_prompts (id, org_id, session_id, ts, seq, agent_slug, source, prompt_text)
		SELECT ?, ?, ?, ?, COALESCE(MAX(seq), 0) + 1, ?, 'hook', ?
		FROM session_prompts WHERE session_id = ?`,
		uuid.NewString(), LocalOrgID, sessionID, nullableTS(ts), agentSlug, text, sessionID)
	return err
}

func nullableTS(ts string) sql.NullString {
	if strings.TrimSpace(ts) == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: ts, Valid: true}
}

// backfillSessionPrompts populates session_prompts for sessions recorded
// before the table existed. Two passes, mirroring the live ingest paths:
// transcripts first (Claude Code / Codex sessions whose events carry a
// transcript_path), then Cursor's beforeSubmitPrompt events for sessions
// still without prompts. Both passes only touch prompt-less sessions, so the
// upgrade pass is idempotent.
func (db *DB) backfillSessionPrompts() (int, error) {
	touched := 0

	// Pass 1: transcripts.
	rows, err := db.Query(`
		SELECT s.id, s.agent_slug, (
			SELECT json_extract(e.payload, '$.transcript_path')
			FROM events e
			WHERE e.session_id = s.id
			  AND json_extract(e.payload, '$.transcript_path') IS NOT NULL
			ORDER BY e.ts DESC
			LIMIT 1
		) AS path
		FROM sessions s
		WHERE NOT EXISTS (
			SELECT 1 FROM session_prompts sp WHERE sp.session_id = s.id
		)`)
	if err != nil {
		return 0, err
	}
	type row struct {
		id, agent string
		path      sql.NullString
	}
	var todo []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.agent, &r.path); err != nil {
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
	for _, r := range todo {
		prompts := transcript.ExtractUserPrompts(r.path.String)
		if len(prompts) == 0 {
			continue
		}
		n, err := db.UpsertSessionPrompts(r.id, r.agent, "transcript", prompts)
		if err != nil {
			return touched, err
		}
		touched += n
	}

	// Pass 2: Cursor hook prompts for sessions the transcript pass didn't cover.
	hookRows, err := db.Query(`
		SELECT e.session_id, e.agent_slug, e.ts, json_extract(e.payload, '$.prompt') AS prompt
		FROM events e
		WHERE e.event_type = 'beforeSubmitPrompt'
		  AND e.session_id IS NOT NULL
		  AND json_extract(e.payload, '$.prompt') IS NOT NULL
		  AND NOT EXISTS (
			SELECT 1 FROM session_prompts sp WHERE sp.session_id = e.session_id
		  )
		ORDER BY e.session_id, e.ts`)
	if err != nil {
		return touched, err
	}
	type hookPrompt struct {
		sessionID, agent, ts, text string
	}
	var hookTodo []hookPrompt
	for hookRows.Next() {
		var h hookPrompt
		if err := hookRows.Scan(&h.sessionID, &h.agent, &h.ts, &h.text); err != nil {
			hookRows.Close()
			return touched, err
		}
		hookTodo = append(hookTodo, h)
	}
	hookRows.Close()
	if err := hookRows.Err(); err != nil {
		return touched, err
	}
	for _, h := range hookTodo {
		if err := db.AppendHookPrompt(h.sessionID, h.agent, h.ts, h.text); err != nil {
			return touched, err
		}
		touched++
	}

	return touched, nil
}
