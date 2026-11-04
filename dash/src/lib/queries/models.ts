import { db } from "../db.ts";
import { sessionScopeSql, type Filters } from "../filters.ts";
import { IDLE_GAP_MINUTES } from "./activity.ts";
import { AI_EDIT_EVENT_CONDITION, LINES_GENERATED } from "./edit-condition.ts";

export interface ModelRow {
  agent: string;
  model: string;
  sessions: number;
  sessions_with_edits: number;
  sessions_with_commits: number;
  active_minutes: number;
  prompts: number;
  turns: number;
  tools: number;
  edits: number;
  lines_generated: number;
  lines_committed: number;
  commits: number;
  // Null when no session in the group reports usage (Cursor).
  output_tokens: number | null;
  input_tokens: number | null;
  edited_files: number;
  reedited_files: number;
}

// One row per agent + model over sessions started in the window. Every ratio
// on the Agents page is derived from these counts in TypeScript, so the table
// can show the counts next to the ratios.
export function modelTable(f: Filters): ModelRow[] {
  const conn = db();
  const scope = sessionScopeSql(f, "s", "s.started_at");
  const sql = `
    WITH scoped AS (
      SELECT s.id, s.agent_slug, COALESCE(s.model_slug, 'unknown') AS model,
             s.output_tokens, s.input_tokens
      FROM sessions s WHERE 1=1${scope.sql}
    ),
    act AS (
      SELECT session_id, SUM(MIN((julianday(ts) - julianday(prev)) * 1440.0, ?)) AS active_minutes
      FROM (
        SELECT e.session_id, e.ts, LAG(e.ts) OVER (PARTITION BY e.session_id ORDER BY e.ts) AS prev
        FROM events e JOIN scoped sc ON sc.id = e.session_id
      ) WHERE prev IS NOT NULL GROUP BY 1
    ),
    ed AS (
      SELECT e.session_id, COUNT(*) AS edits, COALESCE(SUM(${LINES_GENERATED}), 0) AS lines_generated
      FROM events e JOIN scoped sc ON sc.id = e.session_id
      WHERE ${AI_EDIT_EVENT_CONDITION}
      GROUP BY 1
    ),
    ef AS (
      SELECT session_id, COUNT(*) AS files, SUM(n > 1) AS reedited
      FROM (
        SELECT e.session_id,
               COALESCE(e.rel_file_path, (SELECT x.rel_file_path FROM event_line_ranges x WHERE x.event_id = e.id LIMIT 1)) AS f,
               COUNT(*) AS n
        FROM events e JOIN scoped sc ON sc.id = e.session_id
        WHERE ${AI_EDIT_EVENT_CONDITION}
        GROUP BY 1, 2
      ) WHERE f IS NOT NULL GROUP BY 1
    ),
    tl AS (
      SELECT e.session_id, COUNT(*) AS tools
      FROM events e JOIN scoped sc ON sc.id = e.session_id
      WHERE e.event_type IN ('PostToolUse', 'postToolUse') GROUP BY 1
    ),
    pr AS (
      SELECT p.session_id, COUNT(*) AS prompts
      FROM session_prompts p JOIN scoped sc ON sc.id = p.session_id GROUP BY 1
    ),
    tu AS (
      SELECT e.session_id, COUNT(*) AS turns
      FROM events e JOIN scoped sc ON sc.id = e.session_id
      WHERE e.event_type = 'Stop' GROUP BY 1
    ),
    cm AS (
      SELECT cs.session_id, SUM(cs.lines_attributed) AS lines_committed, COUNT(DISTINCT cs.commit_id) AS commits
      FROM commit_sessions cs JOIN scoped sc ON sc.id = cs.session_id GROUP BY 1
    )
    SELECT sc.agent_slug AS agent, sc.model,
           COUNT(*) AS sessions,
           COALESCE(SUM(ed.session_id IS NOT NULL), 0) AS sessions_with_edits,
           COALESCE(SUM(cm.session_id IS NOT NULL), 0) AS sessions_with_commits,
           COALESCE(SUM(act.active_minutes), 0) AS active_minutes,
           COALESCE(SUM(pr.prompts), 0) AS prompts,
           COALESCE(SUM(tu.turns), 0) AS turns,
           COALESCE(SUM(tl.tools), 0) AS tools,
           COALESCE(SUM(ed.edits), 0) AS edits,
           COALESCE(SUM(ed.lines_generated), 0) AS lines_generated,
           COALESCE(SUM(cm.lines_committed), 0) AS lines_committed,
           COALESCE(SUM(cm.commits), 0) AS commits,
           SUM(sc.output_tokens) AS output_tokens,
           SUM(sc.input_tokens) AS input_tokens,
           COALESCE(SUM(ef.files), 0) AS edited_files,
           COALESCE(SUM(ef.reedited), 0) AS reedited_files
    FROM scoped sc
    LEFT JOIN act ON act.session_id = sc.id
    LEFT JOIN ed ON ed.session_id = sc.id
    LEFT JOIN ef ON ef.session_id = sc.id
    LEFT JOIN tl ON tl.session_id = sc.id
    LEFT JOIN pr ON pr.session_id = sc.id
    LEFT JOIN tu ON tu.session_id = sc.id
    LEFT JOIN cm ON cm.session_id = sc.id
    GROUP BY 1, 2
    ORDER BY sessions DESC, agent, model`;
  return conn.prepare(sql).all(...scope.params, IDLE_GAP_MINUTES) as ModelRow[];
}
