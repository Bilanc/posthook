import { db } from "../db";
import { filterSql, type Filters } from "../filters";
import { aiEditsCte } from "./overview";

// One point per local calendar day. `generated` buckets by edit time,
// `committed` buckets by the commit's committed_at — the same line can land
// on a later day than it was generated.
export interface DailyOverallRow {
  day: string; // YYYY-MM-DD, local time
  generated: number;
  committed: number;
}

export interface DailyKeyRow {
  day: string; // YYYY-MM-DD, local time
  key: string;
  lines: number;
}

export interface DailyUsage {
  overall: DailyOverallRow[];
  byAgent: DailyKeyRow[];
  byModel: DailyKeyRow[];
  byRepo: DailyKeyRow[];
  byEngineer: DailyKeyRow[];
}

// Events store UTC RFC3339 timestamps; 'localtime' converts to the viewer's
// local day so chart buckets line up with the local-day from/to filters.
const DAY = (col: string) => `date(${col}, 'localtime')`;

function dailyOverall(f: Filters): DailyOverallRow[] {
  const conn = db();
  const fchunk = filterSql(f);
  const cte = aiEditsCte(fchunk.sql);

  const generated = conn
    .prepare(
      `${cte}
       SELECT ${DAY("e.ts")} AS day, COALESCE(SUM(e.lines_generated), 0) AS lines
       FROM ai_edits_scored e
       GROUP BY day`,
    )
    .all(...fchunk.params) as Array<{ day: string; lines: number }>;

  const committed = conn
    .prepare(
      `${cte}
       SELECT ${DAY("c2.committed_at")} AS day, COALESCE(SUM(ca.lines_generated), 0) AS lines
       FROM committed_ai ca
       JOIN commits c2 ON c2.id = ca.commit_id
       GROUP BY day`,
    )
    .all(...fchunk.params) as Array<{ day: string; lines: number }>;

  const byDay = new Map<string, DailyOverallRow>();
  for (const r of generated) {
    byDay.set(r.day, { day: r.day, generated: r.lines, committed: 0 });
  }
  for (const r of committed) {
    const row = byDay.get(r.day) ?? { day: r.day, generated: 0, committed: 0 };
    row.committed = r.lines;
    byDay.set(r.day, row);
  }
  return [...byDay.values()].sort((a, b) => a.day.localeCompare(b.day));
}

function dailyByDimension(
  groupExpr: string,
  joinSql: string,
  f: Filters,
): DailyKeyRow[] {
  const conn = db();
  const fchunk = filterSql(f);
  const cte = aiEditsCte(fchunk.sql);
  const sql = `${cte}
    SELECT
      ${DAY("e.ts")} AS day,
      ${groupExpr} AS key,
      COALESCE(SUM(e.lines_generated), 0) AS lines
    FROM ai_edits_scored e
    ${joinSql}
    GROUP BY day, key
    ORDER BY day`;
  return conn.prepare(sql).all(...fchunk.params) as DailyKeyRow[];
}

export function dailyUsage(f: Filters): DailyUsage {
  return {
    overall: dailyOverall(f),
    byAgent: dailyByDimension("e.agent_slug", "", f),
    byModel: dailyByDimension("e.model_slug", "", f),
    byRepo: dailyByDimension(
      "COALESCE(r.name, '(no repo)')",
      "LEFT JOIN repositories r ON r.id = e.repo_id",
      f,
    ),
    byEngineer: dailyByDimension("e.engineer_name", "", f),
  };
}
