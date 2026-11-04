import type { ModelRow } from "@/lib/queries/models";

// Every ratio on the Agents page is derived here from the raw counts, so the
// table, the dumbbell and the sort all read the same numbers. A ratio is null
// when its denominator is missing or zero; callers print "—" for null.
export interface ModelMetrics {
  row: ModelRow;
  keep: number | null;          // lines_committed / lines_generated (0..1)
  linesPerKTok: number | null;  // lines_generated per 1K output tokens
  toolsPerEdit: number | null;  // tool calls per edit event
  reedit: number | null;        // reedited_files / edited_files (0..1)
  shipped: number;              // sessions_with_commits / sessions (0..1)
}

function ratio(n: number, d: number | null | undefined): number | null {
  if (d == null || !(d > 0)) return null;
  const r = n / d;
  return Number.isFinite(r) ? r : null;
}

export function deriveMetrics(row: ModelRow): ModelMetrics {
  return {
    row,
    keep: ratio(row.lines_committed, row.lines_generated),
    linesPerKTok: ratio(row.lines_generated, row.output_tokens == null ? null : row.output_tokens / 1000),
    toolsPerEdit: ratio(row.tools, row.edits),
    reedit: ratio(row.reedited_files, row.edited_files),
    shipped: ratio(row.sessions_with_commits, row.sessions) ?? 0,
  };
}

export const SORT_KEYS = [
  "model",
  "sessions",
  "active",
  "prompts",
  "generated",
  "committed",
  "keep",
  "lpk",
  "tpe",
  "reedit",
  "shipped",
] as const;
export type SortKey = (typeof SORT_KEYS)[number];
export type SortDir = "asc" | "desc";

export const DEFAULT_SORT: SortKey = "sessions";

// Text sorts ascending by default; every numeric column descending.
export function defaultDir(key: SortKey): SortDir {
  return key === "model" ? "asc" : "desc";
}

export function parseSort(
  sort: string | string[] | undefined,
  dir: string | string[] | undefined,
): { key: SortKey; dir: SortDir } {
  const s = Array.isArray(sort) ? sort[0] : sort;
  const d = Array.isArray(dir) ? dir[0] : dir;
  const key = (SORT_KEYS as readonly string[]).includes(s ?? "") ? (s as SortKey) : DEFAULT_SORT;
  return { key, dir: d === "asc" || d === "desc" ? d : defaultDir(key) };
}

function sortValue(m: ModelMetrics, key: SortKey): number | null {
  switch (key) {
    case "sessions": return m.row.sessions;
    case "active": return m.row.active_minutes;
    case "prompts": return m.row.prompts;
    case "generated": return m.row.lines_generated;
    case "committed": return m.row.lines_committed;
    case "keep": return m.keep;
    case "lpk": return m.linesPerKTok;
    case "tpe": return m.toolsPerEdit;
    case "reedit": return m.reedit;
    case "shipped": return m.row.sessions_with_commits;
    case "model": return null;
  }
}

// Stable sort; rows with no value for the column ("—") always sink to the
// bottom whichever direction is chosen, so the sample never hides behind
// nulls. Ties fall back to the default order (sessions desc, then name).
export function sortMetrics(list: ModelMetrics[], key: SortKey, dir: SortDir): ModelMetrics[] {
  const sign = dir === "asc" ? 1 : -1;
  const byName = (a: ModelMetrics, b: ModelMetrics) =>
    a.row.agent.localeCompare(b.row.agent) || a.row.model.localeCompare(b.row.model);
  return [...list].sort((a, b) => {
    if (key === "model") return sign * byName(a, b);
    const va = sortValue(a, key);
    const vb = sortValue(b, key);
    if (va == null && vb == null) return b.row.sessions - a.row.sessions || byName(a, b);
    if (va == null) return 1;
    if (vb == null) return -1;
    if (va !== vb) return sign * (va - vb);
    return b.row.sessions - a.row.sessions || byName(a, b);
  });
}
