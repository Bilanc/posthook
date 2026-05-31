import { db } from "../db";
import { filterSql, type Filters } from "../filters";
import { aiEditsCte } from "./overview";
import type { BreakdownRow } from "@/types/posthook";

function runBreakdown(
  groupExpr: string,
  displayExpr: string | null,
  matchExpr: string,
  f: Filters,
): BreakdownRow[] {
  const conn = db();
  const fchunk = filterSql(f);
  const cte = aiEditsCte(fchunk.sql);

  // committed_ai correlated subquery uses `matchExpr` to align with the outer group.
  const sql = `${cte}
    SELECT
      ${groupExpr} AS key,
      ${displayExpr ?? "NULL"} AS display_name,
      COUNT(*) AS edits,
      COALESCE(SUM(e.lines_generated), 0) AS lines_generated,
      COALESCE((
        SELECT SUM(ca.lines_generated) FROM committed_ai ca WHERE ${matchExpr}
      ), 0) AS lines_committed,
      COUNT(DISTINCT e.session_id) AS sessions,
      (
        SELECT b.model_slug FROM ai_edits_scored b
        WHERE ${matchExpr.replaceAll("ca.", "b.")}
        GROUP BY b.model_slug ORDER BY COUNT(*) DESC LIMIT 1
      ) AS top_model
    FROM ai_edits_scored e
    GROUP BY ${groupExpr}
    ORDER BY edits DESC`;
  return conn.prepare(sql).all(...fchunk.params) as BreakdownRow[];
}

export function breakdownByAgent(f: Filters): BreakdownRow[] {
  return runBreakdown("e.agent_slug", "e.agent_slug", "ca.agent_slug = e.agent_slug", f);
}

export function breakdownByModel(f: Filters): BreakdownRow[] {
  return runBreakdown("e.model_slug", "e.model_slug", "ca.model_slug = e.model_slug", f);
}

export function breakdownByEngineer(f: Filters): BreakdownRow[] {
  return runBreakdown(
    "e.engineer_email",
    "e.engineer_name",
    "ca.engineer_email = e.engineer_email",
    f,
  );
}

// Repo needs a join to repositories for the display name.
export function breakdownByRepo(f: Filters): BreakdownRow[] {
  const conn = db();
  const fchunk = filterSql(f);
  const cte = aiEditsCte(fchunk.sql);
  const sql = `${cte}
    SELECT
      COALESCE(e.repo_id, '(no repo)') AS key,
      COALESCE(r.name, '(no repo)') AS display_name,
      COUNT(*) AS edits,
      COALESCE(SUM(e.lines_generated), 0) AS lines_generated,
      COALESCE((
        SELECT SUM(ca.lines_generated) FROM committed_ai ca WHERE ca.repo_id = e.repo_id
      ), 0) AS lines_committed,
      COUNT(DISTINCT e.session_id) AS sessions,
      (
        SELECT b.model_slug FROM ai_edits_scored b
        WHERE b.repo_id = e.repo_id
        GROUP BY b.model_slug ORDER BY COUNT(*) DESC LIMIT 1
      ) AS top_model
    FROM ai_edits_scored e
    LEFT JOIN repositories r ON r.id = e.repo_id
    GROUP BY e.repo_id
    ORDER BY edits DESC`;
  return conn.prepare(sql).all(...fchunk.params) as BreakdownRow[];
}

// Distinct values for filter UI dropdowns.
export interface FilterOptions {
  agents: string[];
  models: string[];
  repos: Array<{ id: string; name: string }>;
  engineers: Array<{ email: string; name: string | null }>;
}

export function filterOptions(): FilterOptions {
  const conn = db();
  const agents = (conn
    .prepare(
      `SELECT agent_slug FROM (
         SELECT DISTINCT agent_slug FROM sessions
         UNION
         SELECT DISTINCT agent_slug FROM events
       )
       ORDER BY agent_slug`,
    )
    .all() as Array<{ agent_slug: string }>)
    .map((r) => r.agent_slug)
    .filter(Boolean);
  const models = (conn
    .prepare(
      `SELECT model_slug FROM (
         SELECT DISTINCT model_slug FROM sessions WHERE model_slug IS NOT NULL
         UNION
         SELECT DISTINCT json_extract(payload, '$.model') AS model_slug
         FROM events
         WHERE json_extract(payload, '$.model') IS NOT NULL
       )
       ORDER BY model_slug`,
    )
    .all() as Array<{ model_slug: string }>)
    .map((r) => r.model_slug);
  const repos = conn
    .prepare(
      `SELECT r.id, r.name
       FROM repositories r
       WHERE EXISTS (
         SELECT 1 FROM events e
         WHERE e.repo_id = r.id
           AND e.event_type != 'hook_misfire'
       )
       OR EXISTS (
         SELECT 1 FROM sessions s
         WHERE s.repo_id = r.id
       )
       OR EXISTS (
         SELECT 1
         FROM commit_sessions cs
         JOIN commits c ON c.id = cs.commit_id
         WHERE c.repo_id = r.id
       )
       ORDER BY r.name`,
    )
    .all() as Array<{ id: string; name: string }>;
  const engineers = conn
    .prepare(
      `SELECT DISTINCT engineer_email AS email, engineer_name AS name
       FROM sessions WHERE engineer_email IS NOT NULL ORDER BY engineer_email`,
    )
    .all() as Array<{ email: string; name: string | null }>;
  return { agents, models, repos, engineers };
}
