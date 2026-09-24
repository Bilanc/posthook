import { db } from "../db";
import { filterSqlForSessions, type Filters } from "../filters";
import { linesGeneratedForUsageSessions } from "./sessions";

// Token usage lives on the sessions table (filled from the Stop-hook
// transcript for Claude Code and Codex; NULL for Cursor). Every query here
// filters sessions directly, so dates bucket by started_at and the model
// filter matches the session's model rather than per-edit model.

export interface TokenBucket {
  input_tokens: number | null;
  output_tokens: number | null;
  cache_read_tokens: number | null;
  cache_creation_tokens: number | null;
}

export interface TokenSummary extends TokenBucket {
  // Sessions in range that report usage vs. all sessions in range.
  sessions_with_usage: number;
  sessions_total: number;
  // AI lines from the same usage-reporting sessions as the token sums.
  lines_generated: number;
}

const BUCKET_COLS = `
  SUM(s.input_tokens) AS input_tokens,
  SUM(s.output_tokens) AS output_tokens,
  SUM(s.cache_read_tokens) AS cache_read_tokens,
  SUM(s.cache_creation_tokens) AS cache_creation_tokens`;

export function tokenSummary(f: Filters): TokenSummary {
  const fchunk = filterSqlForSessions(f);
  const row = db()
    .prepare(
      `SELECT
         ${BUCKET_COLS},
         COUNT(s.input_tokens) AS sessions_with_usage,
         COUNT(*) AS sessions_total
       FROM sessions s
       WHERE 1=1${fchunk.sql}`,
    )
    .get(...fchunk.params) as Omit<TokenSummary, "lines_generated">;
  return { ...row, lines_generated: linesGeneratedForUsageSessions(f) };
}

export function totalTokens(b: TokenBucket): number | null {
  if (
    b.input_tokens == null &&
    b.output_tokens == null &&
    b.cache_read_tokens == null &&
    b.cache_creation_tokens == null
  ) {
    return null;
  }
  return (
    (b.input_tokens ?? 0) +
    (b.output_tokens ?? 0) +
    (b.cache_read_tokens ?? 0) +
    (b.cache_creation_tokens ?? 0)
  );
}

// Share of prompt-side tokens served from cache. Null when nothing was sent.
export function cacheHitRate(b: TokenBucket): number | null {
  const prompt =
    (b.input_tokens ?? 0) + (b.cache_read_tokens ?? 0) + (b.cache_creation_tokens ?? 0);
  if (prompt === 0) return null;
  return ((b.cache_read_tokens ?? 0) / prompt) * 100;
}

export interface DailyTokenRow extends TokenBucket {
  day: string; // YYYY-MM-DD, local time
}

export function dailyTokens(f: Filters): DailyTokenRow[] {
  const fchunk = filterSqlForSessions(f);
  return db()
    .prepare(
      `SELECT
         date(s.started_at, 'localtime') AS day,
         ${BUCKET_COLS}
       FROM sessions s
       WHERE s.input_tokens IS NOT NULL${fchunk.sql}
       GROUP BY day
       ORDER BY day`,
    )
    .all(...fchunk.params) as DailyTokenRow[];
}

export interface TokenBreakdownRow extends TokenBucket {
  key: string;
  display_name: string | null;
  sessions: number;
}

function tokenBreakdown(
  groupExpr: string,
  displayExpr: string,
  joinSql: string,
  f: Filters,
): TokenBreakdownRow[] {
  const fchunk = filterSqlForSessions(f);
  return db()
    .prepare(
      `SELECT
         ${groupExpr} AS key,
         ${displayExpr} AS display_name,
         COUNT(*) AS sessions,
         ${BUCKET_COLS}
       FROM sessions s
       ${joinSql}
       WHERE s.input_tokens IS NOT NULL${fchunk.sql}
       GROUP BY key
       ORDER BY (COALESCE(input_tokens, 0) + COALESCE(output_tokens, 0)
                 + COALESCE(cache_read_tokens, 0) + COALESCE(cache_creation_tokens, 0)) DESC`,
    )
    .all(...fchunk.params) as TokenBreakdownRow[];
}

export function tokensByAgent(f: Filters): TokenBreakdownRow[] {
  return tokenBreakdown("s.agent_slug", "s.agent_slug", "", f);
}

export function tokensByModel(f: Filters): TokenBreakdownRow[] {
  return tokenBreakdown(
    "COALESCE(s.model_slug, 'unknown')",
    "COALESCE(s.model_slug, 'unknown')",
    "",
    f,
  );
}

export function tokensByEngineer(f: Filters): TokenBreakdownRow[] {
  return tokenBreakdown(
    "COALESCE(s.engineer_email, '(unknown)')",
    "COALESCE(s.engineer_name, s.engineer_email, '(unknown)')",
    "",
    f,
  );
}

export function tokensByRepo(f: Filters): TokenBreakdownRow[] {
  return tokenBreakdown(
    "COALESCE(s.repo_id, '(no repo)')",
    "COALESCE(r.name, '(no repo)')",
    "LEFT JOIN repositories r ON r.id = s.repo_id",
    f,
  );
}
