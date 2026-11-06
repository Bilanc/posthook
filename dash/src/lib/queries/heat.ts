import { db } from "../db.ts";
import { filterSql, type Filters } from "../filters.ts";
import { aiEditsCte } from "./overview.ts";

export interface HeatCell {
  dow: number; // 0 = Sunday .. 6 = Saturday, local time
  hour: number; // 0..23, local time
  n: number;
}

// AI edits by local weekday and hour: when the agent is in the loop.
export function editHeatmap(f: Filters): HeatCell[] {
  const conn = db();
  const fchunk = filterSql(f);
  return conn
    .prepare(
      `${aiEditsCte(fchunk.sql)}
       SELECT CAST(strftime('%w', e.ts, 'localtime') AS INTEGER) AS dow,
              CAST(strftime('%H', e.ts, 'localtime') AS INTEGER) AS hour,
              COUNT(*) AS n
       FROM ai_edits_scored e
       GROUP BY 1, 2`,
    )
    .all(...fchunk.params) as HeatCell[];
}
