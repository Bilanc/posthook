import { checkShadowHealth, type ShadowHealth } from "./installShadow.ts";
import { openDb } from "../store.ts";

export async function runStatus(): Promise<void> {
  const db = openDb();

  const totals = db
    .query(
      `SELECT
        (SELECT COUNT(*) FROM events WHERE event_type != 'hook_misfire') AS events,
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

  // Surface shadow health prominently. A misconfigured PATH means we silently
  // capture nothing for git commits across the user's machine — they need to
  // see this every time they check status until it's fixed.
  printShadowHealth(checkShadowHealth());

  const byAgent = db
    .query(
      `SELECT agent_slug, COUNT(*) AS n
       FROM events
       WHERE event_type != 'hook_misfire'
       GROUP BY agent_slug
       ORDER BY n DESC`,
    )
    .all() as Array<{ agent_slug: string; n: number }>;
  if (byAgent.length > 0) {
    console.log("Events by agent:");
    for (const row of byAgent) console.log(`  ${row.agent_slug.padEnd(16)} ${row.n}`);
    console.log("");
  }

  const misfires = db
    .query(
      `SELECT
         COUNT(*) AS total,
         SUM(CASE WHEN ts >= datetime('now', '-1 day') THEN 1 ELSE 0 END) AS last_24h
       FROM events
       WHERE event_type = 'hook_misfire'`,
    )
    .get() as { total: number; last_24h: number };
  if (misfires.total > 0) {
    console.log("Hook health:");
    console.log(`  misfires (total)    ${misfires.total}`);
    console.log(`  misfires (last 24h) ${misfires.last_24h ?? 0}`);
    const misfireByAgent = db
      .query(
        `SELECT agent_slug, COUNT(*) AS n
         FROM events
         WHERE event_type = 'hook_misfire'
         GROUP BY agent_slug
         ORDER BY n DESC`,
      )
      .all() as Array<{ agent_slug: string; n: number }>;
    for (const row of misfireByAgent) {
      console.log(`    ${row.agent_slug.padEnd(14)} ${row.n}`);
    }
    console.log("  A misfire means a hook fired but stdin was empty. Run `posthook inspect");
    console.log("  --type hook_misfire` for context. Check the agent's hook config if frequent.");
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

function printShadowHealth(h: ShadowHealth): void {
  // If the user never ran install-shadow, stay silent. They're either using the
  // per-repo template fallback intentionally or haven't run init yet — `init`
  // itself will print appropriate messaging.
  if (!h.attempted) return;

  if (h.winning && h.symlinkValid) {
    console.log("Git shadow:");
    console.log(`  active   ${h.shadowPath}`);
    console.log(`  real git ${h.savedRealGit ?? "(unsaved)"}`);
    console.log("");
    return;
  }

  // Something is wrong. Print a loud, actionable warning.
  console.log("Git shadow: ⚠ NOT INTERCEPTING");
  if (!h.symlinkExists) {
    console.log(`  Symlink missing at ${h.shadowPath ?? "(unknown)"}.`);
    console.log(`  Fix: run \`posthook install-shadow\`.`);
  } else if (!h.symlinkValid) {
    console.log(`  Symlink at ${h.shadowPath} does not point at the posthook binary.`);
    console.log(`  Fix: run \`posthook uninstall-shadow\` then \`posthook install-shadow\`.`);
  } else if (!h.winning) {
    console.log(`  Symlink is in place at ${h.shadowPath}`);
    console.log(`  but \`which git\` returns ${h.whichGit ?? "(nothing)"} — PATH order means our shadow is bypassed.`);
    console.log(`  Fix: add to your shell rc and open a new shell:`);
    if (h.shadowPath) {
      console.log(`    export PATH="${h.shadowPath.replace(/\/git$/, "")}:$PATH"`);
    }
    console.log(`  Then verify with: which git`);
  }
  console.log(
    `  While unfixed, git commits captured only via per-repo hooks (templateDir + posthook track).`,
  );
  console.log("");
}
