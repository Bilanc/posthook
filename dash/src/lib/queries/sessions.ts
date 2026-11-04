import { db } from "../db.ts";
import { filterSqlForSessions, type Filters, type Outcome } from "../filters.ts";
import { sessionActiveSql } from "./activity.ts";
import { AI_EDIT_EVENT_CONDITION, LINES_GENERATED } from "./edit-condition.ts";
import { ATTRIBUTED, OUTCOME_EXPR, SESSION_OUTCOMES_CTE } from "./outcomes.ts";

// Re-exported for callers that still import these from here.
export { NL, LINE_RANGE_LINES, LINES_GENERATED, AI_EDIT_EVENT_CONDITION } from "./edit-condition.ts";
import type { SessionRow } from "@/types/posthook";

export interface SessionListRow {
  id: string;
  agent_slug: string;
  model_slug: string | null;
  engineer_email: string | null;
  engineer_name: string | null;
  repo_id: string | null;
  repo_name: string | null;
  branch: string | null;
  started_at: string;
  ended_at: string | null;
  // First real prompt (compaction summaries skipped), capped at 500 chars; null
  // when the session captured none.
  first_prompt: string | null;
  prompts: number;
  // Gap-capped active minutes (see activity.ts), 0 for single-event sessions.
  active_minutes: number;
  lines_generated: number;
  lines_committed: number;
  commits_attributed: number;
  input_tokens: number | null;
  output_tokens: number | null;
  outcome: Outcome;
  // Activity-strip data, each sorted ascending: Stop-event timestamps (one per
  // turn), AI edit timestamps and committed_at of the attributed commits.
  stop_ts: string[];
  edit_ts: string[];
  commit_ts: string[];
}

// Prompts that open with a compaction summary are not the session's title.
export const CONTINUED_PROMPT_PREFIX = "This session is being continued";

// group_concat output -> sorted timestamps. Null/empty in, [] out; entries that
// do not parse as dates are dropped so the strip never sees NaN.
export function splitTimestamps(joined: string | null | undefined): string[] {
  if (!joined) return [];
  return joined
    .split(",")
    .map((s) => s.trim())
    .filter((s) => s && Number.isFinite(Date.parse(s)))
    .sort((a, b) => Date.parse(a) - Date.parse(b));
}

export interface ListSessionsResult {
  rows: SessionListRow[];
  total: number;
}

type RawListRow = Omit<SessionListRow, "outcome" | "stop_ts" | "edit_ts" | "commit_ts"> & {
  outcome: string;
  stop_ts: string | null;
  edit_ts: string | null;
  commit_ts: string | null;
};

export function listSessions(
  f: Filters,
  page: number,
  perPage: number,
): ListSessionsResult {
  const conn = db();
  const fchunk = filterSqlForSessions(f);
  const outcomeSql = f.outcome ? ` AND ${OUTCOME_EXPR} = '${f.outcome}'` : "";

  // The outcome join only costs something when an outcome filter is set.
  const countSql = f.outcome
    ? `WITH ${SESSION_OUTCOMES_CTE}
       SELECT COUNT(*) AS n
       FROM sessions s JOIN session_outcomes so ON so.session_id = s.id
       WHERE 1=1${fchunk.sql}${outcomeSql}`
    : `SELECT COUNT(*) AS n FROM sessions s WHERE 1=1${fchunk.sql}`;
  const totalRow = conn.prepare(countSql).get(...fchunk.params) as { n: number };

  const offset = (page - 1) * perPage;

  // One statement: pick the page of session ids first, then compute the
  // per-session aggregates and strip timestamps only for those rows.
  const sql = `
    WITH ${SESSION_OUTCOMES_CTE},
    page AS (
      SELECT s.id, so.has_edits, so.has_commits
      FROM sessions s JOIN session_outcomes so ON so.session_id = s.id
      WHERE 1=1${fchunk.sql}${outcomeSql}
      ORDER BY datetime(s.started_at) DESC, s.id DESC
      LIMIT ? OFFSET ?
    ),
    active AS (${sessionActiveSql(" AND e.session_id IN (SELECT id FROM page)")})
    SELECT
      s.id,
      s.agent_slug,
      s.model_slug,
      s.engineer_email,
      s.engineer_name,
      s.repo_id,
      r.name AS repo_name,
      s.branch,
      s.started_at,
      s.ended_at,
      s.input_tokens,
      s.output_tokens,
      (
        SELECT substr(p.prompt_text, 1, 500)
        FROM session_prompts p
        WHERE p.session_id = s.id AND p.prompt_text NOT LIKE ? ESCAPE '\\'
        ORDER BY p.seq LIMIT 1
      ) AS first_prompt,
      (SELECT COUNT(*) FROM session_prompts p WHERE p.session_id = s.id) AS prompts,
      COALESCE(a.active_minutes, 0) AS active_minutes,
      COALESCE((
        SELECT SUM(${LINES_GENERATED}) FROM events e
        WHERE e.session_id = s.id AND ${AI_EDIT_EVENT_CONDITION}
      ), 0) AS lines_generated,
      COALESCE((
        SELECT SUM(cs.lines_attributed) FROM commit_sessions cs WHERE cs.session_id = s.id
      ), 0) AS lines_committed,
      (
        SELECT COUNT(DISTINCT cs.commit_id) FROM commit_sessions cs
        WHERE cs.session_id = s.id AND ${ATTRIBUTED}
      ) AS commits_attributed,
      ${OUTCOME_EXPR} AS outcome,
      (
        SELECT group_concat(e.ts) FROM events e
        WHERE e.session_id = s.id AND e.event_type = 'Stop'
      ) AS stop_ts,
      (
        SELECT group_concat(e.ts) FROM events e
        WHERE e.session_id = s.id AND ${AI_EDIT_EVENT_CONDITION}
      ) AS edit_ts,
      (
        SELECT group_concat(c.committed_at)
        FROM commit_sessions cs JOIN commits c ON c.id = cs.commit_id
        WHERE cs.session_id = s.id AND ${ATTRIBUTED}
      ) AS commit_ts
    FROM page so
    JOIN sessions s ON s.id = so.id
    LEFT JOIN repositories r ON r.id = s.repo_id
    LEFT JOIN active a ON a.session_id = s.id
    ORDER BY datetime(s.started_at) DESC, s.id DESC`;

  // Placeholders bind in textual order: page filter, LIMIT/OFFSET, then the
  // first-prompt prefix pattern in the outer SELECT.
  const continuedLike = `${CONTINUED_PROMPT_PREFIX.replace(/[\\%_]/g, "\\$&")}%`;
  const raw = conn
    .prepare(sql)
    .all(...fchunk.params, perPage, offset, continuedLike) as RawListRow[];

  const rows: SessionListRow[] = raw.map((r) => ({
    ...r,
    outcome: r.outcome === "shipped" || r.outcome === "edited" ? r.outcome : "explored",
    stop_ts: splitTimestamps(r.stop_ts),
    edit_ts: splitTimestamps(r.edit_ts),
    commit_ts: splitTimestamps(r.commit_ts),
  }));
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

export interface StoredPrompt {
  ts: string;
  text: string;
}

export function storedPromptsForSession(sessionId: string): StoredPrompt[] {
  return db()
    .prepare(
      "SELECT ts, prompt_text AS text " +
        "FROM session_prompts " +
        "WHERE session_id = ? " +
        "ORDER BY seq",
    )
    .all(sessionId) as StoredPrompt[];
}
