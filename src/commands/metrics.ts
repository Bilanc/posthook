import { openDb } from "../store.ts";

// SQLite expression: count newlines in a TEXT column.
// length(s) - length(replace(s, char(10), '')) counts '\n' chars.
const NL = (col: string) => `(length(${col}) - length(replace(${col}, char(10), '')))`;

// Reusable CTE: every AI edit event with parsed payload fields + linked session.
const AI_EDITS_CTE = `
WITH ai_edits AS (
  SELECT
    e.id,
    e.ts,
    e.agent_slug,
    e.session_id,
    e.cwd,
    e.repo_id,
    e.rel_file_path,
    COALESCE(s.model_slug, json_extract(e.payload, '$.model'), 'unknown') AS model_slug,
    COALESCE(json_extract(e.payload, '$.tool_input.new_string'), '') AS new_string,
    COALESCE(json_extract(e.payload, '$.tool_input.old_string'), '') AS old_string,
    COALESCE(json_extract(e.payload, '$.tool_input.content'), '') AS content
  FROM events e
  LEFT JOIN sessions s ON s.id = e.session_id
  WHERE e.event_type = 'PostToolUse'
    AND json_extract(e.payload, '$.tool_name') IN ('Edit', 'Write', 'MultiEdit')
),
ai_edits_scored AS (
  SELECT
    *,
    ${NL("new_string")} + ${NL("content")} AS lines_generated,
    ${NL("old_string")} AS lines_replaced
  FROM ai_edits
),
commit_windows AS (
  SELECT
    c.id AS commit_id,
    c.repo_id,
    c.committed_at,
    c.lines_added AS commit_lines_added,
    (SELECT MAX(datetime(c2.committed_at)) FROM commits c2
       WHERE c2.repo_id = c.repo_id AND datetime(c2.committed_at) < datetime(c.committed_at)) AS prev_committed_at
  FROM commits c
),
committed_ai AS (
  SELECT
    cw.commit_id,
    cw.repo_id,
    cw.commit_lines_added,
    a.agent_slug,
    a.model_slug,
    a.session_id,
    a.lines_generated
  FROM commit_windows cw
  JOIN commit_files cf ON cf.commit_id = cw.commit_id
  JOIN ai_edits_scored a
    ON a.repo_id = cw.repo_id
    AND a.rel_file_path = cf.file_path
    AND datetime(a.ts) < datetime(cw.committed_at)
    AND (cw.prev_committed_at IS NULL OR datetime(a.ts) >= datetime(cw.prev_committed_at))
)
`;

interface BreakdownRow {
  key: string;
  edits: number;
  lines_generated: number;
  lines_committed: number;
  sessions: number;
  top_model: string | null;
}

export async function runMetrics(): Promise<void> {
  const db = openDb();

  const summary = db
    .query(
      `${AI_EDITS_CTE}
       SELECT
         (SELECT COUNT(*) FROM ai_edits_scored) AS edits,
         (SELECT COALESCE(SUM(lines_generated), 0) FROM ai_edits_scored) AS lines_generated,
         (SELECT COALESCE(SUM(lines_replaced), 0) FROM ai_edits_scored) AS lines_replaced,
         (SELECT COALESCE(SUM(lines_generated), 0) FROM committed_ai) AS lines_committed,
         (SELECT COUNT(DISTINCT session_id) FROM ai_edits_scored) AS sessions
       `,
    )
    .get() as {
    edits: number;
    lines_generated: number;
    lines_replaced: number;
    lines_committed: number;
    sessions: number;
  };

  // Working hours: span of events for each session.
  const hours = db
    .query(
      `SELECT COALESCE(SUM(duration_hours), 0) AS total_hours
       FROM (
         SELECT
           s.id,
           (julianday(
              COALESCE(s.ended_at, (SELECT MAX(ts) FROM events e WHERE e.session_id = s.id))
            ) - julianday(
              COALESCE(s.started_at, (SELECT MIN(ts) FROM events e WHERE e.session_id = s.id))
            )) * 24 AS duration_hours
         FROM sessions s
       )
       WHERE duration_hours IS NOT NULL`,
    )
    .get() as { total_hours: number };

  const parallel = db
    .query(
      `WITH session_spans AS (
         SELECT
           s.id,
           COALESCE(s.started_at, (SELECT MIN(ts) FROM events e WHERE e.session_id = s.id)) AS start_ts,
           COALESCE(s.ended_at, (SELECT MAX(ts) FROM events e WHERE e.session_id = s.id)) AS end_ts
         FROM sessions s
       )
       SELECT COALESCE(MAX(concurrent), 0) AS max_concurrent
       FROM (
         SELECT a.id,
                (SELECT COUNT(*) FROM session_spans b
                  WHERE b.start_ts <= a.end_ts AND b.end_ts >= a.start_ts) AS concurrent
         FROM session_spans a
       )`,
    )
    .get() as { max_concurrent: number };

  // Top model by edit count.
  const topModel = db
    .query(
      `${AI_EDITS_CTE}
       SELECT model_slug, COUNT(*) AS n
       FROM ai_edits_scored
       GROUP BY model_slug
       ORDER BY n DESC
       LIMIT 1`,
    )
    .get() as { model_slug: string; n: number } | undefined;

  const commitTotals = db
    .query(
      `SELECT
         COUNT(*) AS n,
         COALESCE(SUM(lines_added), 0) AS added,
         COALESCE(SUM(lines_removed), 0) AS removed
       FROM commits`,
    )
    .get() as { n: number; added: number; removed: number };

  const aiCodePct =
    commitTotals.added > 0
      ? Math.min(100, (summary.lines_committed / commitTotals.added) * 100)
      : 0;

  // ──────────────────────────────────────────────────────────────────────
  console.log("posthook metrics");
  console.log("");
  console.log("Overall");
  console.log(`  AI edit events            ${summary.edits}`);
  console.log(`  AI lines generated        ${summary.lines_generated}  (raw new_string + content)`);
  console.log(`  AI lines replaced         ${summary.lines_replaced}`);
  console.log(`  AI lines committed*       ${summary.lines_committed}`);
  console.log(`  AI code %*                ${aiCodePct.toFixed(1)}%`);
  console.log(`  Working hours             ${hours.total_hours.toFixed(2)}`);
  console.log(`  Distinct sessions         ${summary.sessions}`);
  console.log(`  Max concurrent sessions   ${parallel.max_concurrent}`);
  console.log(
    `  Top model                 ${topModel ? `${topModel.model_slug} (${topModel.n} edits)` : "(no data)"}`,
  );
  console.log(`  Commits captured          ${commitTotals.n}  (+${commitTotals.added} / -${commitTotals.removed} lines)`);
  console.log("");

  printBreakdown("By agent", breakdownByAgent(db));
  printBreakdown("By model", breakdownByModel(db));
  printBreakdown("By repo", breakdownByRepo(db));

  console.log("Notes");
  console.log("  * AI lines committed = sum of generated lines from AI edits whose file landed in a commit");
  console.log("    in the time window after the previous commit. Approximate — overcounts when AI lines are");
  console.log("    later rewritten before commit. Line-level attribution (Phase 1) will replace this.");
  console.log("  • Older events (before the v2 migration) lack repo_id and won't link to commits.");
}

function breakdownByAgent(db: ReturnType<typeof openDb>): BreakdownRow[] {
  return db
    .query(
      `${AI_EDITS_CTE}
       SELECT
         e.agent_slug AS key,
         COUNT(*) AS edits,
         COALESCE(SUM(e.lines_generated), 0) AS lines_generated,
         COALESCE((SELECT SUM(ca.lines_generated) FROM committed_ai ca WHERE ca.agent_slug = e.agent_slug), 0) AS lines_committed,
         COUNT(DISTINCT e.session_id) AS sessions,
         (
           SELECT model_slug FROM ai_edits_scored b
           WHERE b.agent_slug = e.agent_slug
           GROUP BY model_slug ORDER BY COUNT(*) DESC LIMIT 1
         ) AS top_model
       FROM ai_edits_scored e
       GROUP BY e.agent_slug
       ORDER BY edits DESC`,
    )
    .all() as BreakdownRow[];
}

function breakdownByModel(db: ReturnType<typeof openDb>): BreakdownRow[] {
  return db
    .query(
      `${AI_EDITS_CTE}
       SELECT
         e.model_slug AS key,
         COUNT(*) AS edits,
         COALESCE(SUM(e.lines_generated), 0) AS lines_generated,
         COALESCE((SELECT SUM(ca.lines_generated) FROM committed_ai ca WHERE ca.model_slug = e.model_slug), 0) AS lines_committed,
         COUNT(DISTINCT e.session_id) AS sessions,
         NULL AS top_model
       FROM ai_edits_scored e
       GROUP BY e.model_slug
       ORDER BY edits DESC`,
    )
    .all() as BreakdownRow[];
}

function breakdownByRepo(db: ReturnType<typeof openDb>): BreakdownRow[] {
  return db
    .query(
      `${AI_EDITS_CTE}
       SELECT
         COALESCE(r.name, '(no repo)') AS key,
         COUNT(*) AS edits,
         COALESCE(SUM(e.lines_generated), 0) AS lines_generated,
         COALESCE((SELECT SUM(ca.lines_generated) FROM committed_ai ca WHERE ca.repo_id = e.repo_id), 0) AS lines_committed,
         COUNT(DISTINCT e.session_id) AS sessions,
         (
           SELECT model_slug FROM ai_edits_scored b
           WHERE b.repo_id = e.repo_id
           GROUP BY model_slug ORDER BY COUNT(*) DESC LIMIT 1
         ) AS top_model
       FROM ai_edits_scored e
       LEFT JOIN repositories r ON r.id = e.repo_id
       GROUP BY e.repo_id
       ORDER BY edits DESC`,
    )
    .all() as BreakdownRow[];
}

function printBreakdown(title: string, rows: BreakdownRow[]): void {
  console.log(title);
  if (rows.length === 0) {
    console.log("  (no data)");
    console.log("");
    return;
  }
  const keyCol = Math.max(16, ...rows.map((r) => (r.key ?? "").length));
  const modelCol = Math.max(12, ...rows.map((r) => (r.top_model ?? "").length));
  console.log(
    `  ${"key".padEnd(keyCol)}  ${"edits".padStart(6)}  ${"gen".padStart(7)}  ${"commit".padStart(6)}  ${"sess".padStart(4)}  ${"top model".padEnd(modelCol)}`,
  );
  for (const r of rows) {
    console.log(
      `  ${(r.key ?? "").padEnd(keyCol)}  ${String(r.edits).padStart(6)}  ${String(r.lines_generated).padStart(7)}  ${String(r.lines_committed).padStart(6)}  ${String(r.sessions).padStart(4)}  ${(r.top_model ?? "—").padEnd(modelCol)}`,
    );
  }
  console.log("");
}
