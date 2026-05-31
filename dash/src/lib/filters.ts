import { subMonths, formatISO, parseISO, startOfDay, endOfDay } from "date-fns";

export interface Filters {
  from: string | null;   // YYYY-MM-DD interpreted as the local start of day, or null
  to: string | null;     // YYYY-MM-DD interpreted as the local end of day, or null
  agents: string[];
  models: string[];
  repos: string[];
  engineers: string[];
}

export type SearchParams = Record<string, string | string[] | undefined>;

function pickMulti(v: string | string[] | undefined): string[] {
  if (!v) return [];
  if (Array.isArray(v)) return v.filter(Boolean);
  return v.split(",").map((s) => s.trim()).filter(Boolean);
}

function pickOne(v: string | string[] | undefined): string | null {
  if (!v) return null;
  if (Array.isArray(v)) return v[0] ?? null;
  return v.trim() || null;
}

export function parseFilters(searchParams: SearchParams): Filters {
  return {
    from: pickOne(searchParams.from),
    to: pickOne(searchParams.to),
    agents: pickMulti(searchParams.agents),
    models: pickMulti(searchParams.models),
    repos: pickMulti(searchParams.repos),
    engineers: pickMulti(searchParams.engineers),
  };
}

export function defaultFilters(): Filters {
  // Default to "last month" — most users will care about recent activity, and the
  // empty default would dump the full DB into every chart on first load.
  const now = new Date();
  const to = formatISO(now, { representation: "date" });
  const from = formatISO(subMonths(now, 1), { representation: "date" });
  return { from, to, agents: [], models: [], repos: [], engineers: [] };
}

// Resolve user-set filters against defaults: only "from"/"to" gain a default,
// the array filters stay empty if unset (empty = "all").
export function resolveFilters(parsed: Filters): Filters {
  const d = defaultFilters();
  return {
    from: parsed.from ?? d.from,
    to: parsed.to ?? d.to,
    agents: parsed.agents,
    models: parsed.models,
    repos: parsed.repos,
    engineers: parsed.engineers,
  };
}

// Build the SQL WHERE fragment + params for an `events e` / `sessions s` join.
// The fragment always starts with " AND " — caller is responsible for the leading
// WHERE clause. Returns "" if no filters apply.
export interface SqlChunk {
  sql: string;
  params: unknown[];
}

function inClause(col: string, values: string[], params: unknown[]): string {
  if (values.length === 0) return "";
  const placeholders = values.map(() => "?").join(",");
  params.push(...values);
  return ` AND ${col} IN (${placeholders})`;
}

export function localDayStartIso(date: string): string {
  return formatISO(startOfDay(parseISO(date)));
}

export function localDayEndIso(date: string): string {
  return formatISO(endOfDay(parseISO(date)));
}

// SQL fragment that filters the foundational `events`/`sessions` rows.
// The CTE in overview.ts joins events e LEFT JOIN sessions s, so both aliases exist.
export function filterSql(f: Filters): SqlChunk {
  const params: unknown[] = [];
  let sql = "";
  if (f.from) {
    sql += " AND datetime(e.ts) >= datetime(?)";
    params.push(localDayStartIso(f.from));
  }
  if (f.to) {
    sql += " AND datetime(e.ts) <= datetime(?)";
    params.push(localDayEndIso(f.to));
  }
  sql += inClause("e.agent_slug", f.agents, params);
  sql += inClause("COALESCE(s.model_slug, json_extract(e.payload, '$.model'), 'unknown')", f.models, params);
  sql += inClause("e.repo_id", f.repos, params);
  sql += inClause("s.engineer_email", f.engineers, params);
  return { sql, params };
}

// Variant for queries that have `sessions s` but no events alias yet — used for the
// sessions list page where date/agent/etc filter the sessions table directly.
export function filterSqlForSessions(f: Filters): SqlChunk {
  const params: unknown[] = [];
  let sql = "";
  if (f.from) {
    sql += " AND datetime(s.started_at) >= datetime(?)";
    params.push(localDayStartIso(f.from));
  }
  if (f.to) {
    sql += " AND datetime(s.started_at) <= datetime(?)";
    params.push(localDayEndIso(f.to));
  }
  sql += inClause("s.agent_slug", f.agents, params);
  sql += inClause("s.model_slug", f.models, params);
  sql += inClause("s.repo_id", f.repos, params);
  sql += inClause("s.engineer_email", f.engineers, params);
  return { sql, params };
}

// Serialize filters back to a query string for use in <Link> hrefs etc.
export function filtersToQueryString(f: Partial<Filters>): string {
  const parts: string[] = [];
  if (f.from) parts.push(`from=${encodeURIComponent(f.from)}`);
  if (f.to) parts.push(`to=${encodeURIComponent(f.to)}`);
  for (const [key, values] of [
    ["agents", f.agents],
    ["models", f.models],
    ["repos", f.repos],
    ["engineers", f.engineers],
  ] as const) {
    if (values && values.length > 0) parts.push(`${key}=${encodeURIComponent(values.join(","))}`);
  }
  return parts.length ? `?${parts.join("&")}` : "";
}
