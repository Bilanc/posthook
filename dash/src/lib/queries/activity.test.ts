import { test } from "node:test";
import assert from "node:assert/strict";
import { DatabaseSync } from "node:sqlite";
import { installFixture } from "../test-fixture.ts";

const dbPath = installFixture();

// Two November sessions on top of the fixture, written before the read-only
// connection opens. F edits on 1 Nov and its commit lands on 3 Nov; G edits on
// 5 Nov and carries a 0-line attribution row (posthook's "recount found
// nothing" tombstone) on a 6 Nov commit.
{
  const w = new DatabaseSync(dbPath);
  w.exec(`
    INSERT INTO sessions (id, agent_slug, model_slug, repo_id, branch, started_at, ended_at)
    VALUES ('F', 'claude-code', 'claude-fable-5-1', 'r1', 'main', '2026-11-01T10:00:00Z', '2026-11-01T10:05:00Z'),
           ('G', 'claude-code', 'claude-fable-5-1', 'r1', 'main', '2026-11-05T10:00:00Z', '2026-11-05T10:05:00Z');
    INSERT INTO events (id, session_id, ts, event_type, agent_slug, repo_id, rel_file_path, payload) VALUES
      ('f1', 'F', '2026-11-01T10:00:10Z', 'PostToolUse', 'claude-code', 'r1', 'src/f.ts',
       '{"hook_event_name":"PostToolUse","tool_name":"Write","tool_input":{"content":"a\nb\nc\nd\n"},"duration_ms":10}'),
      ('f2', 'F', '2026-11-01T10:00:20Z', 'Stop', 'claude-code', 'r1', NULL, '{"hook_event_name":"Stop"}'),
      ('g1', 'G', '2026-11-05T10:00:10Z', 'PostToolUse', 'claude-code', 'r1', 'src/g.ts',
       '{"hook_event_name":"PostToolUse","tool_name":"Write","tool_input":{"content":"a\n"},"duration_ms":10}'),
      ('g2', 'G', '2026-11-05T10:00:20Z', 'Stop', 'claude-code', 'r1', NULL, '{"hook_event_name":"Stop"}');
    INSERT INTO commits (id, repo_id, sha, committed_at, message, lines_added, lines_removed, files_changed) VALUES
      ('c2', 'r1', 'def5678', '2026-11-03T09:00:00Z', 'later', 4, 0, 1),
      ('c3', 'r1', '0000000', '2026-11-06T09:00:00Z', 'nothing of G', 7, 0, 1);
    INSERT INTO commit_sessions (commit_id, session_id, agent_slug, model_slug, first_event_ts, last_event_ts, event_count, files_touched, lines_attributed) VALUES
      ('c2', 'F', 'claude-code', 'claude-fable-5-1', '2026-11-01T10:00:10Z', '2026-11-01T10:00:10Z', 1, 1, 3),
      ('c3', 'G', 'claude-code', 'claude-fable-5-1', '2026-11-05T10:00:10Z', '2026-11-05T10:00:10Z', 0, 0, 0);
  `);
  w.close();
}

const { dailyActivity, activityTotals } = await import("./activity.ts");
const { funnel, leadTime } = await import("./outcomes.ts");
const { modelTable } = await import("./models.ts");
const { toolMix } = await import("./toolmix.ts");
const { timeSplit } = await import("./timesplit.ts");
const { editHeatmap } = await import("./heat.ts");
const { resolveFilters, parseFilters, priorWindow, windowDays } = await import("../filters.ts");

const f = resolveFilters(parseFilters({ from: "2026-10-01", to: "2026-10-31" }));
const near = (a: number, b: number, eps = 0.01) => assert.ok(Math.abs(a - b) < eps, `${a} ≈ ${b}`);

test("dailyActivity buckets sessions, prompts, edits and gap-capped active minutes per day and agent", () => {
  const rows = dailyActivity(f);
  const byKey = Object.fromEntries(rows.map((r) => [`${r.day.slice(5)}|${r.agent}`, r]));
  const a = byKey["10-10|claude-code"];
  assert.ok(a, "session A's day exists");
  assert.equal(a.sessions, 1);
  assert.equal(a.prompts, 1);
  assert.equal(a.edits, 1);
  assert.equal(a.lines_generated, 3);
  // e1→e2 10s, e2→e3 10s: a third of a minute.
  near(a.active_minutes, 20 / 60);
  const a2 = byKey["10-20|claude-code"];
  assert.equal(a2.prompts, 1);
  assert.equal(a2.lines_generated, 2);
  // e3 (10 Oct) → e4 (20 Oct) is a ten-day gap, capped at the idle threshold.
  near(a2.active_minutes, 5 + 10 / 60);
  assert.equal(a2.lines_committed, 4);
  assert.equal(a2.commits, 1);
  const b = byKey["10-11|codex"];
  assert.equal(b.sessions, 1);
  assert.equal(b.edits, 0);
  near(b.active_minutes, 10 / 60);
  const t = activityTotals(rows);
  assert.equal(t.sessions, 3);
  assert.equal(t.prompts, 4);
  assert.equal(t.lines_generated, 8);
  assert.equal(t.lines_committed, 4);
});

test("dailyActivity respects the agent filter", () => {
  const rows = dailyActivity({ ...f, agents: ["codex"] });
  assert.deepEqual([...new Set(rows.map((r) => r.agent))], ["codex"]);
  assert.equal(activityTotals(rows).sessions, 1);
});

test("a commit counts on its own date even when its edits predate the window", () => {
  // 2–30 Nov: F's edit (1 Nov) is outside, its commit (3 Nov) inside.
  const nov = resolveFilters(parseFilters({ from: "2026-11-02", to: "2026-11-30" }));
  const rows = dailyActivity(nov);
  const day = rows.find((r) => r.day.endsWith("11-03"));
  assert.ok(day, "the commit's day exists");
  assert.equal(day.lines_committed, 3);
  assert.equal(day.commits, 1);
  assert.equal(day.sessions, 0);
  const t = activityTotals(rows);
  assert.equal(t.lines_committed, 3);
  assert.equal(t.commits, 1);
  const fn = funnel(nov);
  assert.equal(fn.lines_committed, 3);
  assert.equal(fn.commits, 1);
});

test("a 0-line attribution row does not make a session shipped or count as a commit", () => {
  const nov = resolveFilters(parseFilters({ from: "2026-11-01", to: "2026-11-30" }));
  const fn = funnel(nov);
  assert.equal(fn.sessions, 2);
  assert.equal(fn.with_edits, 2);
  assert.equal(fn.with_commits, 1); // F only
  assert.equal(fn.commits, 1);
  assert.equal(fn.lines_committed, 3);
  const rows = dailyActivity(nov);
  assert.equal(rows.find((r) => r.day.endsWith("11-06"))?.commits ?? 0, 0);
  const g = modelTable(nov).find((r) => r.agent === "claude-code");
  assert.ok(g);
  assert.equal(g.sessions, 2);
  assert.equal(g.sessions_with_commits, 1);
  assert.equal(g.commits, 1);
  assert.equal(leadTime(nov).total, 1);
});

test("funnel counts sessions by outcome and lines by stage", () => {
  const fn = funnel(f);
  assert.equal(fn.sessions, 3);
  assert.equal(fn.with_edits, 2);
  assert.equal(fn.with_commits, 1);
  assert.equal(fn.lines_generated, 8);
  assert.equal(fn.lines_committed, 4);
  assert.equal(fn.commits, 1);
});

test("leadTime measures last edit to commit", () => {
  const lt = leadTime(f);
  assert.equal(lt.total, 1);
  assert.equal(lt.under_15m, 1); // 10:20:30 → 10:25:00
  assert.equal(lt.over_1d, 0);
});

test("modelTable aggregates per agent and model with null tokens where unreported", () => {
  const rows = modelTable(f);
  const cc = rows.find((r) => r.agent === "claude-code" && r.model === "claude-fable-5-1");
  assert.ok(cc);
  assert.equal(cc.sessions, 2);
  assert.equal(cc.sessions_with_edits, 2);
  assert.equal(cc.sessions_with_commits, 1);
  assert.equal(cc.prompts, 3);
  assert.equal(cc.turns, 3);
  assert.equal(cc.tools, 5);
  assert.equal(cc.edits, 4);
  assert.equal(cc.lines_generated, 8);
  assert.equal(cc.lines_committed, 4);
  assert.equal(cc.output_tokens, 7000);
  assert.equal(cc.edited_files, 3);
  assert.equal(cc.reedited_files, 1);
  const cx = rows.find((r) => r.agent === "codex");
  assert.ok(cx);
  assert.equal(cx.sessions, 1);
  assert.equal(cx.output_tokens, null);
  assert.equal(cx.edits, 0);
});

test("toolMix folds tool names into families", () => {
  const rows = toolMix(f);
  const key = (r: { agent: string; family: string }) => `${r.agent}/${r.family}`;
  const m = Object.fromEntries(rows.map((r) => [key(r), r.calls]));
  assert.equal(m["claude-code/shell"], 1);
  assert.equal(m["claude-code/edit"], 4);
  assert.equal(m["codex/shell"], 1);
});

test("timeSplit sums tool durations by family and flags agents without durations", () => {
  const ts = timeSplit(f);
  const shell = ts.waits.find((w) => w.family === "shell");
  assert.ok(shell);
  assert.equal(shell.calls, 2);
  assert.equal(shell.calls_with_duration, 1);
  near(shell.minutes, 2000 / 60000, 1e-6);
  assert.deepEqual(ts.agents_without_durations, ["codex"]);
  assert.equal(ts.slowest[0].tool_name, "Bash");
  assert.equal(ts.slowest[0].duration_ms, 2000);
  assert.equal(ts.slowest[0].repo_name, "demo");
  near(ts.active_minutes, 20 / 60 + 5 + 10 / 60 + 10 / 60 + 20 / 60);
});

test("editHeatmap returns one cell per weekday/hour with edits", () => {
  const cells = editHeatmap(f);
  assert.equal(cells.reduce((a, c) => a + c.n, 0), 4);
  for (const c of cells) {
    assert.ok(c.dow >= 0 && c.dow <= 6);
    assert.ok(c.hour >= 0 && c.hour <= 23);
  }
});

test("priorWindow shifts the range back by its own length", () => {
  assert.equal(windowDays(f), 31);
  const p = priorWindow(f);
  assert.ok(p);
  assert.equal(p.from, "2026-08-31");
  assert.equal(p.to, "2026-09-30");
  assert.equal(priorWindow({ ...f, from: null }), null);
});
