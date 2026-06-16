import { db } from "../db";
import { filterSql, localDayStartIso, localDayEndIso, type Filters } from "../filters";
import type { OverviewSummary } from "@/types/posthook";

// SQLite expression: count newlines in a TEXT column.
const NL = (col: string) => `(length(${col}) - length(replace(${col}, char(10), '')))`;
const ACTIVE_REPO_EXISTS = `(
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
)`;

// Shared metric CTE. Filters are injected into ai_edits, then committed_ai uses
// the materialized commit/session attribution table for sessions that survive
// those filters.
export function aiEditsCte(filtersSql: string): string {
  return `
WITH ai_edits AS (
  SELECT
    e.id,
    e.ts,
    e.agent_slug,
    e.session_id,
    e.cwd,
    e.repo_id,
    e.rel_file_path,
    json_extract(e.payload, '$.tool_name') AS tool_name,
    COALESCE(s.model_slug, json_extract(e.payload, '$.model'), 'unknown') AS model_slug,
    COALESCE(s.engineer_email, '(unknown)') AS engineer_email,
    COALESCE(s.engineer_name, s.engineer_email, '(unknown)') AS engineer_name,
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
    ${filtersSql}
),
ai_edits_scored AS (
  SELECT
    *,
    CASE
      WHEN line_range_lines > 0 THEN line_range_lines
      WHEN tool_name = 'apply_patch' THEN event_lines_added
      ELSE ${NL("new_string")} + ${NL("content")}
    END AS lines_generated,
    CASE
      WHEN tool_name = 'apply_patch' THEN event_lines_removed
      ELSE ${NL("old_string")}
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
    COALESCE(s.engineer_email, '(unknown)') AS engineer_email,
    cs.session_id,
    cs.lines_attributed AS lines_generated
  FROM commit_sessions cs
  JOIN commits c ON c.id = cs.commit_id
  LEFT JOIN sessions s ON s.id = cs.session_id
  WHERE EXISTS (
    SELECT 1
    FROM ai_edits_scored a
    WHERE a.session_id = cs.session_id
      AND a.repo_id = c.repo_id
  )
)`;
}

// Mirrors the CLI's commits-totals query, but respects date range so the
// denominator for "AI %" matches the filtered numerator.
function commitTotalsSql(f: Filters): { sql: string; params: unknown[] } {
  let sql = `SELECT
    COUNT(*) AS n,
    COALESCE(SUM(lines_added), 0) AS added,
    COALESCE(SUM(lines_removed), 0) AS removed
  FROM commits c
  WHERE 1=1`;
  const params: unknown[] = [];
  if (f.from) {
    sql += " AND datetime(committed_at) >= datetime(?)";
    params.push(localDayStartIso(f.from));
  }
  if (f.to) {
    sql += " AND datetime(committed_at) <= datetime(?)";
    params.push(localDayEndIso(f.to));
  }
  if (f.repos.length > 0) {
    sql += ` AND repo_id IN (${f.repos.map(() => "?").join(",")})`;
    params.push(...f.repos);
  } else {
    sql += ` AND ${ACTIVE_REPO_EXISTS}`;
  }
  return { sql, params };
}

export function overviewSummary(f: Filters): OverviewSummary {
  const conn = db();
  const fchunk = filterSql(f);
  const cte = aiEditsCte(fchunk.sql);

  const summary = conn
    .prepare(
      `${cte}
       SELECT
         (SELECT COUNT(*) FROM ai_edits_scored) AS edits,
         (SELECT COALESCE(SUM(lines_generated), 0) FROM ai_edits_scored) AS lines_generated,
         (SELECT COALESCE(SUM(lines_replaced), 0) FROM ai_edits_scored) AS lines_replaced,
         (SELECT COALESCE(SUM(lines_generated), 0) FROM committed_ai) AS lines_committed,
         (SELECT COUNT(DISTINCT session_id) FROM ai_edits_scored) AS sessions`,
    )
    .get(...fchunk.params) as {
    edits: number;
    lines_generated: number;
    lines_replaced: number;
    lines_committed: number;
    sessions: number;
  };

  // Working hours: span sum of sessions (re-uses ingest schema). Filters apply
  // by intersecting with the sessions table directly, not via the CTE.
  const hours = workingHours(f);

  const tokens = tokenTotals(f);

  // Max concurrent sessions in window.
  const parallel = maxConcurrent(f);

  // Top model in the filtered AI edits.
  const topModel = conn
    .prepare(
      `${cte}
       SELECT model_slug, COUNT(*) AS n
       FROM ai_edits_scored
       GROUP BY model_slug
       ORDER BY n DESC
       LIMIT 1`,
    )
    .get(...fchunk.params) as { model_slug: string; n: number } | undefined;

  const commits = commitTotalsSql(f);
  const commitTotals = conn.prepare(commits.sql).get(...commits.params) as {
    n: number;
    added: number;
    removed: number;
  };

  const aiCodePct =
    commitTotals.added > 0
      ? Math.min(100, (summary.lines_committed / commitTotals.added) * 100)
      : 0;

  return {
    edits: summary.edits,
    lines_generated: summary.lines_generated,
    lines_replaced: summary.lines_replaced,
    lines_committed: summary.lines_committed,
    sessions: summary.sessions,
    working_hours: hours,
    max_concurrent: parallel,
    top_model: topModel?.model_slug ?? null,
    top_model_edits: topModel?.n ?? 0,
    commit_count: commitTotals.n,
    commit_lines_added: commitTotals.added,
    commit_lines_removed: commitTotals.removed,
    ai_code_pct: aiCodePct,
    input_tokens: tokens.input_tokens,
    output_tokens: tokens.output_tokens,
    cache_read_tokens: tokens.cache_read_tokens,
    cache_creation_tokens: tokens.cache_creation_tokens,
  };
}

// Token sums over sessions in the filter window. Same filter semantics as
// workingHours (dates/agents/engineers intersect the sessions table directly).
// SUM over all-NULL groups stays null — "no agent in range reports usage".
function tokenTotals(f: Filters): {
  input_tokens: number | null;
  output_tokens: number | null;
  cache_read_tokens: number | null;
  cache_creation_tokens: number | null;
} {
  const conn = db();
  let sql = `SELECT
       SUM(s.input_tokens) AS input_tokens,
       SUM(s.output_tokens) AS output_tokens,
       SUM(s.cache_read_tokens) AS cache_read_tokens,
       SUM(s.cache_creation_tokens) AS cache_creation_tokens
     FROM sessions s
     WHERE 1=1`;
  const params: unknown[] = [];
  if (f.from) {
    sql += " AND datetime(s.started_at) >= datetime(?)";
    params.push(localDayStartIso(f.from));
  }
  if (f.to) {
    sql += " AND datetime(s.started_at) <= datetime(?)";
    params.push(localDayEndIso(f.to));
  }
  if (f.agents.length > 0) {
    sql += ` AND s.agent_slug IN (${f.agents.map(() => "?").join(",")})`;
    params.push(...f.agents);
  }
  if (f.engineers.length > 0) {
    sql += ` AND s.engineer_email IN (${f.engineers.map(() => "?").join(",")})`;
    params.push(...f.engineers);
  }
  return conn.prepare(sql).get(...params) as ReturnType<typeof tokenTotals>;
}

function workingHours(f: Filters): number {
  const conn = db();
  let sql = `SELECT COALESCE(SUM(duration_hours), 0) AS total_hours
       FROM (
         SELECT
           s.id,
           (julianday(
              COALESCE(s.ended_at, (SELECT MAX(ts) FROM events e WHERE e.session_id = s.id))
            ) - julianday(
              COALESCE(s.started_at, (SELECT MIN(ts) FROM events e WHERE e.session_id = s.id))
            )) * 24 AS duration_hours
         FROM sessions s
         WHERE 1=1`;
  const params: unknown[] = [];
  if (f.from) {
    sql += " AND datetime(s.started_at) >= datetime(?)";
    params.push(localDayStartIso(f.from));
  }
  if (f.to) {
    sql += " AND datetime(s.started_at) <= datetime(?)";
    params.push(localDayEndIso(f.to));
  }
  if (f.agents.length > 0) {
    sql += ` AND s.agent_slug IN (${f.agents.map(() => "?").join(",")})`;
    params.push(...f.agents);
  }
  if (f.engineers.length > 0) {
    sql += ` AND s.engineer_email IN (${f.engineers.map(() => "?").join(",")})`;
    params.push(...f.engineers);
  }
  sql += `       ) WHERE duration_hours IS NOT NULL`;
  const row = conn.prepare(sql).get(...params) as { total_hours: number };
  return row.total_hours;
}

function maxConcurrent(f: Filters): number {
  const conn = db();
  let preFilter = `1=1`;
  const params: unknown[] = [];
  if (f.from) {
    preFilter += " AND datetime(s.started_at) >= datetime(?)";
    params.push(localDayStartIso(f.from));
  }
  if (f.to) {
    preFilter += " AND datetime(s.started_at) <= datetime(?)";
    params.push(localDayEndIso(f.to));
  }
  const sql = `WITH session_spans AS (
         SELECT
           s.id,
           COALESCE(s.started_at, (SELECT MIN(ts) FROM events e WHERE e.session_id = s.id)) AS start_ts,
           COALESCE(s.ended_at, (SELECT MAX(ts) FROM events e WHERE e.session_id = s.id)) AS end_ts
         FROM sessions s
         WHERE ${preFilter}
       )
       SELECT COALESCE(MAX(concurrent), 0) AS max_concurrent
       FROM (
         SELECT a.id,
                (SELECT COUNT(*) FROM session_spans b
                  WHERE b.start_ts <= a.end_ts AND b.end_ts >= a.start_ts) AS concurrent
         FROM session_spans a
       )`;
  const row = conn.prepare(sql).get(...params) as { max_concurrent: number };
  return row.max_concurrent;
}

// Funnel data: AI edit lines generated vs lines attributed on commits.
export interface FunnelData {
  generated: number;
  committed: number;
}

export function funnel(f: Filters): FunnelData {
  const s = overviewSummary(f);
  return {
    generated: s.lines_generated,
    committed: s.lines_committed,
  };
}
