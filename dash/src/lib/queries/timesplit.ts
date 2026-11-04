import { db } from "../db.ts";
import { filterSql, sessionScopeSql, type Filters } from "../filters.ts";
import { toolFamilySql, type ToolFamily } from "../tool-families.ts";
import { IDLE_GAP_MINUTES } from "./activity.ts";

const TOOL_NAME = `json_extract(e.payload, '$.tool_name')`;
// Claude Code reports duration_ms; Cursor reports duration (ms). Codex reports
// neither, so its waits are unknown rather than zero.
const DURATION_MS = `COALESCE(json_extract(e.payload, '$.duration_ms'), json_extract(e.payload, '$.duration'))`;
const POST_TOOL = `e.event_type IN ('PostToolUse', 'postToolUse') AND json_valid(e.payload)`;

export interface FamilyWait {
  family: ToolFamily;
  calls: number;
  calls_with_duration: number;
  minutes: number;
}

export interface SlowCall {
  session_id: string;
  ts: string;
  tool_name: string;
  summary: string | null;
  duration_ms: number;
  repo_name: string | null;
  agent_slug: string;
}

export interface TimeSplit {
  active_minutes: number;
  waits: FamilyWait[];
  // Agents in scope whose tool calls carry no duration (their time in tools
  // is unknown, not zero).
  agents_without_durations: string[];
  slowest: SlowCall[];
}

export function timeSplit(f: Filters, slowestN = 5): TimeSplit {
  const conn = db();
  const fchunk = filterSql(f);

  const waits = conn
    .prepare(
      `SELECT ${toolFamilySql(TOOL_NAME)} AS family,
              COUNT(*) AS calls,
              SUM(${DURATION_MS} IS NOT NULL) AS calls_with_duration,
              COALESCE(SUM(${DURATION_MS}), 0) / 60000.0 AS minutes
       FROM events e LEFT JOIN sessions s ON s.id = e.session_id
       WHERE ${POST_TOOL}${fchunk.sql}
       GROUP BY 1
       ORDER BY minutes DESC`,
    )
    .all(...fchunk.params) as FamilyWait[];

  const coverage = conn
    .prepare(
      `SELECT e.agent_slug, SUM(${DURATION_MS} IS NOT NULL) AS with_duration, COUNT(*) AS n
       FROM events e LEFT JOIN sessions s ON s.id = e.session_id
       WHERE ${POST_TOOL}${fchunk.sql}
       GROUP BY 1`,
    )
    .all(...fchunk.params) as Array<{ agent_slug: string; with_duration: number; n: number }>;

  const evScope = sessionScopeSql(f, "s", "e.ts");
  const active = conn
    .prepare(
      `SELECT COALESCE(SUM(MIN((julianday(ts) - julianday(prev)) * 1440.0, ?)), 0) AS active_minutes
       FROM (
         SELECT e.session_id, e.ts,
                LAG(e.ts) OVER (PARTITION BY e.session_id ORDER BY e.ts) AS prev
         FROM events e JOIN sessions s ON s.id = e.session_id
         WHERE 1=1${evScope.sql}
       ) WHERE prev IS NOT NULL`,
    )
    .get(IDLE_GAP_MINUTES, ...evScope.params) as { active_minutes: number };

  const slowest = conn
    .prepare(
      `SELECT e.session_id, e.ts, ${TOOL_NAME} AS tool_name,
              COALESCE(json_extract(e.payload, '$.tool_input.command'),
                       json_extract(e.payload, '$.tool_input.description'),
                       e.rel_file_path) AS summary,
              ${DURATION_MS} AS duration_ms,
              r.name AS repo_name,
              e.agent_slug
       FROM events e
       LEFT JOIN sessions s ON s.id = e.session_id
       LEFT JOIN repositories r ON r.id = e.repo_id
       WHERE ${POST_TOOL} AND ${DURATION_MS} IS NOT NULL${fchunk.sql}
       ORDER BY duration_ms DESC
       LIMIT ?`,
    )
    .all(...fchunk.params, slowestN) as SlowCall[];

  return {
    active_minutes: active.active_minutes,
    waits,
    agents_without_durations: coverage.filter((c) => c.n > 0 && c.with_duration === 0).map((c) => c.agent_slug),
    slowest,
  };
}
