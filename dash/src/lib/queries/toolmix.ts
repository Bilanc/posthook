import { db } from "../db.ts";
import { filterSql, type Filters } from "../filters.ts";
import { toolFamilySql, type ToolFamily } from "../tool-families.ts";

export interface ToolMixRow {
  agent: string;
  family: ToolFamily;
  calls: number;
}

// Tool calls by agent and family over the filter window.
export function toolMix(f: Filters): ToolMixRow[] {
  const conn = db();
  const fchunk = filterSql(f);
  return conn
    .prepare(
      `SELECT e.agent_slug AS agent,
              ${toolFamilySql(`json_extract(e.payload, '$.tool_name')`)} AS family,
              COUNT(*) AS calls
       FROM events e LEFT JOIN sessions s ON s.id = e.session_id
       WHERE e.event_type IN ('PostToolUse', 'postToolUse') AND json_valid(e.payload)${fchunk.sql}
       GROUP BY 1, 2
       ORDER BY 1, calls DESC`,
    )
    .all(...fchunk.params) as ToolMixRow[];
}
