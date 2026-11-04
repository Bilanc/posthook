import { db } from "../db.ts";
import {
  filterSql,
  sessionScopeSql,
  type Filters,
} from "../filters.ts";
import { aiEditsCte } from "./overview.ts";

// Active time is the sum of gaps between consecutive events in a session, each
// gap capped at this many minutes. It replaces wall-clock session span, which
// counts idle hours and double-counts parallel sessions.
export const IDLE_GAP_MINUTES = (() => {
  const v = Number(process.env.POSTHOOK_DASH_IDLE_MINUTES);
  return Number.isFinite(v) && v > 0 ? v : 5;
})();

// Per-session active minutes as a CTE body. `eventsWhere` narrows the events
// scanned (must start with " AND " or be empty); the gap cap is inlined so the
// fragment adds no positional parameter of its own.
export function sessionActiveSql(eventsWhere = ""): string {
  return `
    SELECT session_id, SUM(MIN((julianday(ts) - julianday(prev)) * 1440.0, ${IDLE_GAP_MINUTES})) AS active_minutes
    FROM (
      SELECT e.session_id, e.ts,
             LAG(e.ts) OVER (PARTITION BY e.session_id ORDER BY e.ts) AS prev
      FROM events e
      WHERE e.session_id IS NOT NULL${eventsWhere}
    )
    WHERE prev IS NOT NULL
    GROUP BY session_id`;
}

export interface DailyActivityRow {
  day: string; // YYYY-MM-DD, local
  agent: string;
  active_minutes: number;
  sessions: number;
  prompts: number;
  edits: number;
  lines_generated: number;
  lines_committed: number;
  commits: number;
}

// One row per local day per agent. Generated/committed lines reuse the
// overview CTE so the numbers match the funnel; sessions, prompts and active
// minutes scope on the sessions table.
export function dailyActivity(f: Filters): DailyActivityRow[] {
  const conn = db();
  const fchunk = filterSql(f);
  const cte = aiEditsCte(fchunk.sql);
  const evScope = sessionScopeSql(f, "s", "e.ts");
  const sesScope = sessionScopeSql(f, "s", "s.started_at");
  const prScope = sessionScopeSql(f, "s", "p.ts");
  const comScope = sessionScopeSql(f, "s", "c.committed_at");

  const sql = `${cte},
    ev AS (
      SELECT e.session_id, e.ts, s.agent_slug,
             LAG(e.ts) OVER (PARTITION BY e.session_id ORDER BY e.ts) AS prev
      FROM events e
      JOIN sessions s ON s.id = e.session_id
      WHERE 1=1${evScope.sql}
    ),
    act AS (
      SELECT date(ts, 'localtime') AS day, agent_slug,
             SUM(MIN((julianday(ts) - julianday(prev)) * 1440.0, ?)) AS active_minutes
      FROM ev WHERE prev IS NOT NULL GROUP BY 1, 2
    ),
    ses AS (
      SELECT date(s.started_at, 'localtime') AS day, s.agent_slug, COUNT(*) AS sessions
      FROM sessions s WHERE 1=1${sesScope.sql} GROUP BY 1, 2
    ),
    pr AS (
      SELECT date(p.ts, 'localtime') AS day, s.agent_slug, COUNT(*) AS prompts
      FROM session_prompts p JOIN sessions s ON s.id = p.session_id
      WHERE p.ts IS NOT NULL${prScope.sql} GROUP BY 1, 2
    ),
    gen AS (
      SELECT date(e.ts, 'localtime') AS day, e.agent_slug,
             COALESCE(SUM(e.lines_generated), 0) AS lines_generated, COUNT(*) AS edits
      FROM ai_edits_scored e GROUP BY 1, 2
    ),
    com AS (
      SELECT date(c.committed_at, 'localtime') AS day, ca.agent_slug,
             COALESCE(SUM(ca.lines_generated), 0) AS lines_committed,
             COUNT(DISTINCT ca.commit_id) AS commits
      FROM committed_ai ca
      JOIN commits c ON c.id = ca.commit_id
      LEFT JOIN sessions s ON s.id = ca.session_id
      WHERE 1=1${comScope.sql} GROUP BY 1, 2
    ),
    keys AS (
      SELECT day, agent_slug FROM act UNION SELECT day, agent_slug FROM ses
      UNION SELECT day, agent_slug FROM pr UNION SELECT day, agent_slug FROM gen
      UNION SELECT day, agent_slug FROM com
    )
    SELECT k.day, k.agent_slug AS agent,
           COALESCE(a.active_minutes, 0) AS active_minutes,
           COALESCE(se.sessions, 0) AS sessions,
           COALESCE(p.prompts, 0) AS prompts,
           COALESCE(g.edits, 0) AS edits,
           COALESCE(g.lines_generated, 0) AS lines_generated,
           COALESCE(c.lines_committed, 0) AS lines_committed,
           COALESCE(c.commits, 0) AS commits
    FROM keys k
    LEFT JOIN act a ON a.day = k.day AND a.agent_slug = k.agent_slug
    LEFT JOIN ses se ON se.day = k.day AND se.agent_slug = k.agent_slug
    LEFT JOIN pr p ON p.day = k.day AND p.agent_slug = k.agent_slug
    LEFT JOIN gen g ON g.day = k.day AND g.agent_slug = k.agent_slug
    LEFT JOIN com c ON c.day = k.day AND c.agent_slug = k.agent_slug
    ORDER BY k.day, k.agent_slug`;
  return conn
    .prepare(sql)
    .all(
      ...fchunk.params,
      ...evScope.params,
      IDLE_GAP_MINUTES,
      ...sesScope.params,
      ...prScope.params,
      ...comScope.params,
    ) as DailyActivityRow[];
}

export interface ActivityTotals {
  active_minutes: number;
  sessions: number;
  prompts: number;
  edits: number;
  lines_generated: number;
  lines_committed: number;
  commits: number;
}

export function activityTotals(rows: DailyActivityRow[]): ActivityTotals {
  const t: ActivityTotals = {
    active_minutes: 0,
    sessions: 0,
    prompts: 0,
    edits: 0,
    lines_generated: 0,
    lines_committed: 0,
    commits: 0,
  };
  for (const r of rows) {
    t.active_minutes += r.active_minutes;
    t.sessions += r.sessions;
    t.prompts += r.prompts;
    t.edits += r.edits;
    t.lines_generated += r.lines_generated;
    t.lines_committed += r.lines_committed;
    t.commits += r.commits;
  }
  return t;
}
