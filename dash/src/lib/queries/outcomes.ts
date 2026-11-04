import { db } from "../db.ts";
import { filterSql, sessionScopeSql, type Filters, type Outcome } from "../filters.ts";
import { aiEditsCte } from "./overview.ts";
import { AI_EDIT_EVENT_CONDITION } from "./edit-condition.ts";

// Per-session outcome flags as a CTE. Correlated EXISTS over the session
// indexes; cheap at any realistic session count.
export const SESSION_OUTCOMES_CTE = `session_outcomes AS (
  SELECT
    s.id AS session_id,
    EXISTS (SELECT 1 FROM events e WHERE e.session_id = s.id AND ${AI_EDIT_EVENT_CONDITION}) AS has_edits,
    EXISTS (SELECT 1 FROM commit_sessions cs WHERE cs.session_id = s.id) AS has_commits
  FROM sessions s
)`;

export const OUTCOME_EXPR = `CASE WHEN so.has_commits THEN 'shipped' WHEN so.has_edits THEN 'edited' ELSE 'explored' END`;

export function outcomeOf(hasEdits: boolean | number, hasCommits: boolean | number): Outcome {
  if (hasCommits) return "shipped";
  if (hasEdits) return "edited";
  return "explored";
}

// SQL fragment restricting to one outcome; "" when no outcome filter is set.
// Requires `session_outcomes so` to be joined.
export function outcomeWhere(o: Outcome | null): string {
  if (!o) return "";
  return ` AND ${OUTCOME_EXPR} = '${o}'`;
}

export interface Funnel {
  sessions: number;
  with_edits: number;
  with_commits: number;
  lines_generated: number;
  lines_committed: number;
  commits: number;
}

// Sessions started in the window, by what they left behind, plus the line
// funnel from the overview CTE.
export function funnel(f: Filters): Funnel {
  const conn = db();
  const scope = sessionScopeSql(f, "s", "s.started_at");
  const sessions = conn
    .prepare(
      `WITH ${SESSION_OUTCOMES_CTE}
       SELECT COUNT(*) AS sessions,
              COALESCE(SUM(so.has_edits), 0) AS with_edits,
              COALESCE(SUM(so.has_commits), 0) AS with_commits
       FROM sessions s JOIN session_outcomes so ON so.session_id = s.id
       WHERE 1=1${scope.sql}`,
    )
    .get(...scope.params) as { sessions: number; with_edits: number; with_commits: number };

  const fchunk = filterSql(f);
  const comScope = sessionScopeSql(f, "s", "c.committed_at");
  const lines = conn
    .prepare(
      `${aiEditsCte(fchunk.sql)}
       SELECT
         (SELECT COALESCE(SUM(lines_generated), 0) FROM ai_edits_scored) AS lines_generated,
         (SELECT COALESCE(SUM(ca.lines_generated), 0)
            FROM committed_ai ca JOIN commits c ON c.id = ca.commit_id
            LEFT JOIN sessions s ON s.id = ca.session_id WHERE 1=1${comScope.sql}) AS lines_committed,
         (SELECT COUNT(DISTINCT ca.commit_id)
            FROM committed_ai ca JOIN commits c ON c.id = ca.commit_id
            LEFT JOIN sessions s ON s.id = ca.session_id WHERE 1=1${comScope.sql}) AS commits`,
    )
    .get(...fchunk.params, ...comScope.params, ...comScope.params) as {
    lines_generated: number;
    lines_committed: number;
    commits: number;
  };
  return { ...sessions, ...lines };
}

export interface LeadTime {
  total: number;
  under_15m: number;
  over_1d: number;
}

// Hours from a session's last attributed edit to the commit that took it.
export function leadTime(f: Filters): LeadTime {
  const conn = db();
  const scope = sessionScopeSql(f, "s", "c.committed_at");
  return conn
    .prepare(
      `SELECT COUNT(*) AS total,
              COALESCE(SUM(h < 0.25), 0) AS under_15m,
              COALESCE(SUM(h >= 24), 0) AS over_1d
       FROM (
         SELECT (julianday(c.committed_at) - julianday(cs.last_event_ts)) * 24 AS h
         FROM commit_sessions cs
         JOIN commits c ON c.id = cs.commit_id
         LEFT JOIN sessions s ON s.id = cs.session_id
         WHERE 1=1${scope.sql}
       )`,
    )
    .get(...scope.params) as LeadTime;
}
