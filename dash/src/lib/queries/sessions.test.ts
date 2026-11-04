import { test } from "node:test";
import assert from "node:assert/strict";
import { DatabaseSync } from "node:sqlite";
import { installFixture } from "../test-fixture.ts";

const dbPath = installFixture();

// Two November sessions on top of the fixture: D opens with a compaction
// summary and then a real prompt; E only ever saw the summary. Written before
// the read-only connection in db.ts opens.
{
  const w = new DatabaseSync(dbPath);
  w.exec(`
    INSERT INTO sessions (id, agent_slug, model_slug, repo_id, branch, started_at, ended_at)
    VALUES ('D', 'claude-code', 'claude-fable-5-1', 'r1', 'main', '2026-11-02T10:00:00Z', NULL),
           ('E', 'claude-code', 'claude-fable-5-1', 'r1', 'main', '2026-11-03T10:00:00Z', '2026-11-03T10:10:00Z');
    INSERT INTO session_prompts (id, session_id, ts, seq, agent_slug, prompt_text) VALUES
      ('p5', 'D', '2026-11-02T10:00:05Z', 1, 'claude-code', 'This session is being continued from a previous conversation that ran out of context.'),
      ('p6', 'D', '2026-11-02T10:01:00Z', 2, 'claude-code', 'Carry on with the migration'),
      ('p7', 'E', '2026-11-03T10:00:05Z', 1, 'claude-code', 'This session is being continued from a previous conversation.');
  `);
  w.close();
}

const { listSessions, splitTimestamps } = await import("./sessions.ts");
const { resolveFilters, parseFilters, filterSqlForSessions } = await import("../filters.ts");

const f = resolveFilters(parseFilters({ from: "2026-10-01", to: "2026-10-31" }));
const ids = (rows: Array<{ id: string }>) => rows.map((r) => r.id);
const near = (a: number, b: number, eps = 0.01) => assert.ok(Math.abs(a - b) < eps, `${a} ≈ ${b}`);

test("listSessions returns the window newest first with outcomes, aggregates and strip data", () => {
  const { rows, total } = listSessions(f, 1, 50);
  assert.equal(total, 3);
  assert.deepEqual(ids(rows), ["C", "B", "A"]);
  assert.deepEqual(
    rows.map((r) => r.outcome),
    ["edited", "explored", "shipped"],
  );

  const a = rows[2];
  assert.equal(a.first_prompt, "Fix the thing");
  assert.equal(a.prompts, 2);
  assert.equal(a.commits_attributed, 1);
  assert.equal(a.lines_committed, 4);
  assert.equal(a.lines_generated, 5);
  assert.equal(a.repo_name, "demo");
  assert.equal(a.branch, "main");
  assert.equal(a.output_tokens, 5000);
  assert.equal(a.stop_ts.length, 2);
  assert.ok(a.stop_ts[0] < a.stop_ts[1], "stop timestamps are sorted");
  assert.equal(a.edit_ts.length, 2);
  assert.equal(a.commit_ts.length, 1);
  assert.equal(a.commit_ts[0], "2026-10-20T10:25:00Z");
  // e1→e2 10s, e2→e3 10s, e3→e4 ten days capped at the idle gap, e4→e5 10s.
  near(a.active_minutes, 5 + 30 / 60);

  const b = rows[1];
  assert.equal(b.prompts, 1);
  assert.equal(b.first_prompt, "look around");
  assert.equal(b.lines_generated, 0);
  assert.equal(b.lines_committed, 0);
  assert.equal(b.commits_attributed, 0);
  assert.deepEqual(b.edit_ts, []);
  assert.deepEqual(b.commit_ts, []);
  assert.equal(b.stop_ts.length, 1);
  assert.equal(b.output_tokens, null);

  const c = rows[0];
  assert.equal(c.first_prompt, "Touch c");
  assert.equal(c.lines_generated, 3);
  assert.equal(c.edit_ts.length, 2);
  assert.equal(c.stop_ts.length, 1);
  assert.equal(c.branch, "feat");

  for (const r of rows) {
    assert.ok(Number.isFinite(r.active_minutes));
    assert.ok(Number.isFinite(r.lines_generated));
    assert.ok(Number.isFinite(r.lines_committed));
  }
});

test("outcome filter narrows rows and total", () => {
  const shipped = listSessions({ ...f, outcome: "shipped" }, 1, 50);
  assert.deepEqual(ids(shipped.rows), ["A"]);
  assert.equal(shipped.total, 1);
  const edited = listSessions({ ...f, outcome: "edited" }, 1, 50);
  assert.deepEqual(ids(edited.rows), ["C"]);
  assert.equal(edited.total, 1);
  const explored = listSessions({ ...f, outcome: "explored" }, 1, 50);
  assert.deepEqual(ids(explored.rows), ["B"]);
  assert.equal(explored.total, 1);
});

test("free-text search matches prompt text as well as the session columns", () => {
  assert.deepEqual(ids(listSessions({ ...f, q: "Touch" }, 1, 50).rows), ["C"]);
  assert.deepEqual(ids(listSessions({ ...f, q: "look" }, 1, 50).rows), ["B"]);
  assert.deepEqual(ids(listSessions({ ...f, q: "feat" }, 1, 50).rows), ["C"]);
  // LIKE wildcards in the query are escaped, not interpreted.
  assert.equal(listSessions({ ...f, q: "%" }, 1, 50).total, 0);
  const chunk = filterSqlForSessions({ ...f, q: "x" });
  assert.match(chunk.sql, /session_prompts p WHERE p\.session_id = s\.id AND p\.prompt_text LIKE \?/);
});

test("pagination keeps the newest-first order across pages", () => {
  const p1 = listSessions(f, 1, 2);
  assert.deepEqual(ids(p1.rows), ["C", "B"]);
  assert.equal(p1.total, 3);
  const p2 = listSessions(f, 2, 2);
  assert.deepEqual(ids(p2.rows), ["A"]);
  assert.equal(p2.total, 3);
});

test("first_prompt skips compaction summaries and is null when nothing else was captured", () => {
  const nov = resolveFilters(parseFilters({ from: "2026-11-01", to: "2026-11-30" }));
  const { rows } = listSessions(nov, 1, 50);
  assert.deepEqual(ids(rows), ["E", "D"]);
  const d = rows[1];
  assert.equal(d.first_prompt, "Carry on with the migration");
  assert.equal(d.prompts, 2);
  assert.equal(d.outcome, "explored");
  assert.equal(d.ended_at, null);
  assert.equal(d.active_minutes, 0);
  assert.deepEqual(d.stop_ts, []);
  const e = rows[0];
  assert.equal(e.first_prompt, null);
  assert.equal(e.prompts, 1);
});

test("splitTimestamps sorts group_concat output and drops junk", () => {
  assert.deepEqual(splitTimestamps(null), []);
  assert.deepEqual(splitTimestamps(""), []);
  assert.deepEqual(
    splitTimestamps("2026-10-20T10:25:00Z,2026-10-10T10:00:30Z,not-a-date,"),
    ["2026-10-10T10:00:30Z", "2026-10-20T10:25:00Z"],
  );
  // Offset-form commit timestamps sort against Z-form event timestamps.
  assert.deepEqual(
    splitTimestamps("2026-10-10T03:00:00-08:00,2026-10-10T10:00:30Z"),
    ["2026-10-10T10:00:30Z", "2026-10-10T03:00:00-08:00"],
  );
});

