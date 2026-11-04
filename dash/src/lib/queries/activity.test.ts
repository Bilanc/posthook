import { test } from "node:test";
import assert from "node:assert/strict";
import { installFixture } from "../test-fixture.ts";

installFixture();
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
