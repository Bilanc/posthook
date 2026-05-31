import { db } from "../db";
import { filterSqlForSessions, type Filters } from "../filters";
import type { SessionRow } from "@/types/posthook";

export interface SessionListRow {
  id: string;
  agent_slug: string;
  model_slug: string | null;
  engineer_email: string | null;
  engineer_name: string | null;
  repo_id: string | null;
  repo_name: string | null;
  started_at: string;
  ended_at: string | null;
  duration_hours: number | null;
  lines_generated: number;
  edits: number;
  files_touched: number;
  commits_attributed: number;
}

const NL = (col: string) => `(length(${col}) - length(replace(${col}, char(10), '')))`;
const LINE_RANGE_LINES = `COALESCE((
  SELECT SUM(elr.new_text_lines)
  FROM event_line_ranges elr
  WHERE elr.event_id = e.id
), 0)`;
const LINES_GENERATED = `CASE
  WHEN ${LINE_RANGE_LINES} > 0 THEN ${LINE_RANGE_LINES}
  WHEN json_extract(e.payload, '$.tool_name') = 'apply_patch' THEN COALESCE(e.lines_added, 0)
  ELSE ${NL("COALESCE(json_extract(e.payload, '$.tool_input.new_string'), '')")}
    + ${NL("COALESCE(json_extract(e.payload, '$.tool_input.content'), '')")}
END`;
const AI_EDIT_EVENT_CONDITION = `(
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
)`;

export interface ListSessionsResult {
  rows: SessionListRow[];
  total: number;
}

export function listSessions(
  f: Filters,
  page: number,
  perPage: number,
): ListSessionsResult {
  const conn = db();
  const fchunk = filterSqlForSessions(f);

  const totalRow = conn
    .prepare(`SELECT COUNT(*) AS n FROM sessions s WHERE 1=1${fchunk.sql}`)
    .get(...fchunk.params) as { n: number };

  const offset = (page - 1) * perPage;

  // Aggregate per-session edit stats and attributed commit counts in a single query so
  // pagination still works.
  const sql = `
    WITH per_session_edits AS (
      SELECT
        e.session_id,
        COUNT(*) AS edits,
        COUNT(DISTINCT e.rel_file_path) AS files_touched,
        SUM(${LINES_GENERATED}) AS lines_generated
      FROM events e
      WHERE ${AI_EDIT_EVENT_CONDITION}
      GROUP BY e.session_id
    ),
    per_session_commits AS (
      SELECT
        session_id,
        COUNT(DISTINCT commit_id) AS commits_attributed
      FROM commit_sessions
      GROUP BY session_id
    )
    SELECT
      s.id,
      s.agent_slug,
      s.model_slug,
      s.engineer_email,
      s.engineer_name,
      s.repo_id,
      r.name AS repo_name,
      s.started_at,
      s.ended_at,
      (julianday(COALESCE(s.ended_at, s.started_at)) - julianday(s.started_at)) * 24 AS duration_hours,
      COALESCE(pse.lines_generated, 0) AS lines_generated,
      COALESCE(pse.edits, 0) AS edits,
      COALESCE(pse.files_touched, 0) AS files_touched,
      COALESCE(psc.commits_attributed, 0) AS commits_attributed
    FROM sessions s
    LEFT JOIN repositories r ON r.id = s.repo_id
    LEFT JOIN per_session_edits pse ON pse.session_id = s.id
    LEFT JOIN per_session_commits psc ON psc.session_id = s.id
    WHERE 1=1${fchunk.sql}
    ORDER BY datetime(s.started_at) DESC
    LIMIT ? OFFSET ?`;

  const rows = conn
    .prepare(sql)
    .all(...fchunk.params, perPage, offset) as SessionListRow[];
  return { rows, total: totalRow.n };
}

export interface SessionDetail extends SessionRow {
  repo_name: string | null;
  repo_remote_url: string | null;
  lines_generated: number;
  edits: number;
  files_touched: number;
}

export function getSessionDetail(id: string): SessionDetail | null {
  const conn = db();
  const sql = `
    SELECT
      s.*,
      r.name AS repo_name,
      r.remote_url AS repo_remote_url,
      (
        SELECT COALESCE(SUM(${LINES_GENERATED}), 0)
        FROM events e
        WHERE e.session_id = s.id
          AND ${AI_EDIT_EVENT_CONDITION}
      ) AS lines_generated,
      (
        SELECT COUNT(*) FROM events e
        WHERE e.session_id = s.id AND ${AI_EDIT_EVENT_CONDITION}
      ) AS edits,
      (
        SELECT COUNT(DISTINCT e.rel_file_path) FROM events e
        WHERE e.session_id = s.id AND ${AI_EDIT_EVENT_CONDITION}
          AND e.rel_file_path IS NOT NULL
      ) AS files_touched
    FROM sessions s
    LEFT JOIN repositories r ON r.id = s.repo_id
    WHERE s.id = ?`;
  return (conn.prepare(sql).get(id) as SessionDetail | undefined) ?? null;
}

export interface FileTouched {
  rel_file_path: string;
  edits: number;
  lines_generated: number;
}

export function filesTouchedInSession(sessionId: string): FileTouched[] {
  const conn = db();
  return conn
    .prepare(
      `SELECT
         e.rel_file_path,
         COUNT(*) AS edits,
         SUM(${LINES_GENERATED}) AS lines_generated
       FROM events e
       WHERE e.session_id = ?
         AND ${AI_EDIT_EVENT_CONDITION}
         AND e.rel_file_path IS NOT NULL
       GROUP BY e.rel_file_path
       ORDER BY lines_generated DESC, edits DESC`,
    )
    .all(sessionId) as FileTouched[];
}

// Latest transcript_path from any event in the session (used for prompts panel).
export function transcriptPathForSession(sessionId: string): string | null {
  const conn = db();
  const row = conn
    .prepare(
      `SELECT json_extract(payload, '$.transcript_path') AS path
       FROM events
       WHERE session_id = ?
         AND json_extract(payload, '$.transcript_path') IS NOT NULL
       ORDER BY ts DESC LIMIT 1`,
    )
    .get(sessionId) as { path: string | null } | undefined;
  return row?.path ?? null;
}
