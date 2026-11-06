import { test } from "node:test";
import assert from "node:assert/strict";
import { buildTurns, classifyPrompts } from "./turns.ts";
import type { ToolCall } from "./queries/turns.ts";

const tool = (id: string, ts: string, tool_name: string, extra: Partial<ToolCall> = {}): ToolCall => ({
  id,
  ts,
  tool_name,
  duration_ms: null,
  file: null,
  lines_generated: 0,
  summary: null,
  has_stderr: 0,
  ranged: 0,
  ...extra,
});

const stop = (ts: string, msg: string) => ({
  ts,
  last_assistant_message: msg,
  effort: "high",
  permission_mode: "auto",
});

test("classifyPrompts marks compaction summaries and quick resends", () => {
  const ps = classifyPrompts([
    { ts: "2026-10-10T10:00:00Z", seq: 1, text: "do x" },
    { ts: "2026-10-10T10:01:00Z", seq: 2, text: "do x" },
    { ts: "2026-10-10T10:30:00Z", seq: 3, text: "do x" },
    { ts: "2026-10-10T11:00:00Z", seq: 4, text: "This session is being continued from a previous conversation that ran out of context." },
  ]);
  assert.deepEqual(ps.map((p) => p.kind), ["user", "resend", "user", "compaction"]);
});

test("buildTurns cuts at Stops and buckets prompts, tools, commits and usage by time", () => {
  const turns = buildTurns({
    started_at: "2026-10-10T10:00:00Z",
    ended_at: "2026-10-10T10:30:00Z",
    stops: [stop("2026-10-10T10:00:30Z", "done"), stop("2026-10-10T10:20:40Z", "wrote b")],
    prompts: [
      { ts: "2026-10-10T10:00:05Z", seq: 1, text: "Fix the thing" },
      { ts: "2026-10-10T10:20:25Z", seq: 2, text: "Now the other" },
    ],
    tools: [
      tool("e1", "2026-10-10T10:00:10Z", "Bash", { duration_ms: 2000 }),
      tool("e2", "2026-10-10T10:00:20Z", "Edit", { file: "src/a.ts", lines_generated: 3 }),
      tool("e4", "2026-10-10T10:20:30Z", "Bash", { ranged: 1, file: "src/b.ts", lines_generated: 2 }),
    ],
    commits: [
      {
        id: "c1",
        sha: "abc1234",
        committed_at: "2026-10-10T10:25:00Z",
        message: "ship it",
        lines_added: 10,
        lines_removed: 1,
        files_changed: 2,
        lines_attributed: 4,
        files_touched: 2,
      },
    ],
    usage: [
      { ts: "2026-10-10T10:00:12Z", model: "m", input_tokens: 1, output_tokens: 100, cache_read_tokens: 0, cache_creation_tokens: 0, thinking_tokens: 40, thinking_ms: null },
      { ts: "2026-10-10T10:20:35Z", model: "m", input_tokens: 1, output_tokens: 300, cache_read_tokens: 0, cache_creation_tokens: 0, thinking_tokens: null, thinking_ms: null },
    ],
  });
  assert.equal(turns.length, 2);
  const [t1, t2] = turns;
  assert.equal(t1.n, 1);
  assert.equal(t1.start, "2026-10-10T10:00:05Z");
  assert.equal(t1.end, "2026-10-10T10:00:30Z");
  assert.deepEqual(t1.prompts.map((p) => p.text), ["Fix the thing"]);
  assert.deepEqual(t1.tools.map((x) => x.id), ["e1", "e2"]);
  assert.deepEqual(t1.families, { shell: 1, edit: 1 });
  assert.equal(t1.edits.length, 1);
  assert.equal(t1.tool_ms, 2000);
  assert.equal(t1.reply, "done");
  assert.equal(t1.output_tokens, 100);
  assert.equal(t1.thinking_tokens, 40);
  assert.equal(t1.model_messages, 1);
  assert.equal(t1.commits.length, 0);
  assert.equal(t1.open, false);
  // A shell call that posthook located in the file counts as an edit.
  assert.deepEqual(t2.edits.map((x) => x.id), ["e4"]);
  assert.deepEqual(t2.families, { edit: 1 });
  // The commit landed after the last Stop; the last turn keeps it.
  assert.deepEqual(t2.commits.map((c) => c.sha), ["abc1234"]);
  assert.equal(t2.output_tokens, 300);
});

test("buildTurns places a commit made just after a Stop in that turn only", () => {
  const commit = (id: string, committed_at: string) => ({
    id,
    sha: id,
    committed_at,
    message: null,
    lines_added: 1,
    lines_removed: 0,
    files_changed: 1,
    lines_attributed: 1,
    files_touched: 1,
  });
  const turns = buildTurns({
    started_at: "2026-10-10T10:00:00Z",
    ended_at: "2026-10-10T10:30:00Z",
    stops: [stop("2026-10-10T10:00:30Z", "a"), stop("2026-10-10T10:10:00Z", "b"), stop("2026-10-10T10:20:00Z", "c")],
    prompts: [
      { ts: "2026-10-10T10:00:05Z", seq: 1, text: "one" },
      { ts: "2026-10-10T10:01:30Z", seq: 2, text: "two" },
      { ts: "2026-10-10T10:15:00Z", seq: 3, text: "three" },
    ],
    tools: [],
    commits: [
      commit("in-slack", "2026-10-10T10:01:00Z"), // 30s after Stop 1, inside turn 2's window too
      commit("mid-turn", "2026-10-10T10:05:00Z"), // well inside turn 2
      commit("trailing", "2026-10-10T10:40:00Z"), // after the session ended
    ],
    usage: null,
  });
  assert.equal(turns.length, 3);
  assert.deepEqual(turns.map((x) => x.commits.map((c) => c.id)), [["in-slack"], ["mid-turn"], ["trailing"]]);
});

test("buildTurns adds an open turn for activity after the last Stop and leaves tokens null without usage", () => {
  const turns = buildTurns({
    started_at: "2026-10-10T10:00:00Z",
    ended_at: null,
    stops: [stop("2026-10-10T10:00:30Z", "done")],
    prompts: [
      { ts: "2026-10-10T10:00:05Z", seq: 1, text: "first" },
      { ts: "2026-10-10T10:05:00Z", seq: 2, text: "second, still running" },
    ],
    tools: [tool("e9", "2026-10-10T10:05:10Z", "Read")],
    commits: [],
    usage: null,
  });
  assert.equal(turns.length, 2);
  assert.equal(turns[1].open, true);
  assert.equal(turns[1].reply, null);
  assert.deepEqual(turns[1].tools.map((x) => x.family), ["read"]);
  assert.equal(turns[1].output_tokens, null);
  assert.equal(turns[0].output_tokens, null);
});

test("buildTurns with no Stops yields a single open turn holding everything", () => {
  const turns = buildTurns({
    started_at: "2026-10-10T10:00:00Z",
    ended_at: "2026-10-10T10:10:00Z",
    stops: [],
    prompts: [{ ts: "2026-10-10T10:00:05Z", seq: 1, text: "hi" }],
    tools: [tool("e1", "2026-10-10T10:00:10Z", "Bash")],
    commits: [],
    usage: null,
  });
  assert.equal(turns.length, 1);
  assert.equal(turns[0].open, true);
  assert.equal(turns[0].prompts.length, 1);
  assert.equal(turns[0].tools.length, 1);
});
