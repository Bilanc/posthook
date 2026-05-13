import { openDb } from "../store.ts";

export async function runStatus(): Promise<void> {
  const db = openDb();

  const totals = db
    .query(
      `SELECT
        (SELECT COUNT(*) FROM events) AS events,
        (SELECT COUNT(*) FROM sessions) AS sessions,
        (SELECT COUNT(*) FROM commits) AS commits,
        (SELECT COUNT(*) FROM repositories) AS repos`,
    )
    .get() as { events: number; sessions: number; commits: number; repos: number };

  console.log(`posthook — local store at ~/.posthook/posthook.db`);
  console.log(`  events:       ${totals.events}`);
  console.log(`  sessions:     ${totals.sessions}`);
  console.log(`  commits:      ${totals.commits}`);
  console.log(`  repositories: ${totals.repos}`);
  console.log("");

  const byAgent = db
    .query(
      `SELECT agent_slug, COUNT(*) AS n
       FROM events
       GROUP BY agent_slug
       ORDER BY n DESC`,
    )
    .all() as Array<{ agent_slug: string; n: number }>;
  if (byAgent.length > 0) {
    console.log("Events by agent:");
    for (const row of byAgent) console.log(`  ${row.agent_slug.padEnd(16)} ${row.n}`);
    console.log("");
  }

  const recentCommits = db
    .query(
      `SELECT c.sha, c.lines_added, c.lines_removed, c.files_changed, c.committed_at, r.name AS repo
       FROM commits c
       JOIN repositories r ON r.id = c.repo_id
       ORDER BY c.committed_at DESC
       LIMIT 10`,
    )
    .all() as Array<{
    sha: string;
    lines_added: number;
    lines_removed: number;
    files_changed: number;
    committed_at: string;
    repo: string;
  }>;
  if (recentCommits.length > 0) {
    console.log("Recent commits:");
    for (const c of recentCommits) {
      const stamp = c.committed_at.slice(0, 16).replace("T", " ");
      console.log(
        `  ${stamp}  ${c.sha.slice(0, 7)}  ${c.repo.padEnd(20)} +${c.lines_added}/-${c.lines_removed} (${c.files_changed} files)`,
      );
    }
  }
}
