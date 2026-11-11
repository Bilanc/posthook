import { test } from "node:test";
import assert from "node:assert/strict";
import { DatabaseSync } from "node:sqlite";
import { installFixture } from "../test-fixture.ts";

const dbPath = installFixture();

// Session F: edits recorded by a client that measures them itself
// (events.lines_generated / lines_reworked). Every payload is written so the
// older estimate would give a different, larger number.
//
//   f1  Edit   src/f.ts   wrote 5 lines; the new_string repeats 4 lines of context (old estimate 9)
//   f2  Edit   src/f.ts   rewrote 2 of its own lines: 2 written, 2 reworked  -> net 0
//   f3  Write  ~/plan.md  40 lines outside the repo                           -> 0
//   f4  apply_patch across src/g.ts (4 lines) and src/h.ts (2 lines)          -> 6
//   f5  Bash   src/k.sh   3 lines measured, but no position (text had moved)  -> 3
//
// Session G: one apply_patch that names a.go first. Its 3 lines there could
// not be placed; its 2 lines in b.go could.
// Session J: one apply_patch that adds a line in a.go and rewrites one of the
// session's own lines in b.go (2 generated, 1 reworked, and the rework is b.go's).
// Session H: a shell write of 5 lines, then a shell rewrite that only removes
// 2 of them: 0 generated, 2 reworked, no ranges, not an edit tool by name.
// Session K: an older client's patch across two files (no measured counts).
//
// An event that wrote several files carries the split in events.file_lines.
{
  const J = (o: unknown) => JSON.stringify(o).replaceAll("'", "''");
  const post = (tool: string, input: Record<string, unknown>) =>
    J({ hook_event_name: "PostToolUse", tool_name: tool, tool_input: input });
  const w = new DatabaseSync(dbPath);
  w.exec(`
    INSERT INTO sessions (id, agent_slug, model_slug, repo_id, branch, started_at, ended_at)
    VALUES ('F', 'codex', 'gpt-6', 'r1', 'main', '2026-10-15T10:00:00Z', '2026-10-15T10:10:00Z');
    INSERT INTO events (id, session_id, ts, event_type, agent_slug, repo_id, rel_file_path, file_path, lines_added, lines_generated, lines_reworked, payload) VALUES
      ('f1', 'F', '2026-10-15T10:00:10Z', 'PostToolUse', 'codex', 'r1', 'src/f.ts', '/tmp/demo/src/f.ts', NULL, 5, 0,
        '${post("Edit", { old_string: "1\n2\n3\n4\n", new_string: "1\n2\na\nb\nc\nd\ne\n3\n4\n" })}'),
      ('f2', 'F', '2026-10-15T10:00:20Z', 'PostToolUse', 'codex', 'r1', 'src/f.ts', '/tmp/demo/src/f.ts', NULL, 2, 2,
        '${post("Edit", { old_string: "a\nb\n", new_string: "A\nB\n" })}'),
      ('f3', 'F', '2026-10-15T10:00:30Z', 'PostToolUse', 'codex', 'r1', NULL, '/home/dev/plan.md', NULL, 0, 0,
        '${post("Write", { content: "step\n".repeat(40) })}'),
      ('f4', 'F', '2026-10-15T10:00:40Z', 'PostToolUse', 'codex', 'r1', 'src/g.ts', '/tmp/demo/src/g.ts', 6, 6, 0,
        '${post("apply_patch", { command: "*** Begin Patch" })}'),
      ('f5', 'F', '2026-10-15T10:00:50Z', 'PostToolUse', 'codex', 'r1', 'src/k.sh', '/tmp/demo/src/k.sh', NULL, 3, 0,
        '${post("Bash", { command: "cat > src/k.sh" })}');
    INSERT INTO event_line_ranges (id, event_id, file_path, rel_file_path, start_line, end_line, new_text_lines) VALUES
      ('r-f1', 'f1', '/tmp/demo/src/f.ts', 'src/f.ts', 3, 7, 5),
      ('r-f2', 'f2', '/tmp/demo/src/f.ts', 'src/f.ts', 3, 4, 2),
      ('r-f4a', 'f4', '/tmp/demo/src/g.ts', 'src/g.ts', 1, 4, 4),
      ('r-f4b', 'f4', '/tmp/demo/src/h.ts', 'src/h.ts', 10, 11, 2);
    INSERT INTO sessions (id, agent_slug, model_slug, repo_id, branch, started_at, ended_at)
    VALUES ('G', 'codex', 'gpt-6', 'r1', 'main', '2026-10-16T10:00:00Z', '2026-10-16T10:10:00Z');
    INSERT INTO events (id, session_id, ts, event_type, agent_slug, repo_id, rel_file_path, file_path, lines_added, lines_generated, lines_reworked, payload) VALUES
      ('g1', 'G', '2026-10-16T10:00:10Z', 'PostToolUse', 'codex', 'r1', 'a.go', '/tmp/demo/a.go', 5, 5, 0,
        '${post("apply_patch", { command: "*** Begin Patch" })}');
    INSERT INTO event_line_ranges (id, event_id, file_path, rel_file_path, start_line, end_line, new_text_lines) VALUES
      ('r-g1', 'g1', '/tmp/demo/b.go', 'b.go', 4, 5, 2);

    INSERT INTO sessions (id, agent_slug, model_slug, repo_id, branch, started_at, ended_at) VALUES
      ('J', 'codex', 'gpt-6', 'r1', 'main', '2026-10-17T10:00:00Z', '2026-10-17T10:10:00Z'),
      ('H', 'claude-code', 'claude-fable-5-1', 'r1', 'main', '2026-10-18T10:00:00Z', '2026-10-18T10:10:00Z'),
      ('K', 'codex', 'gpt-6', 'r1', 'main', '2026-10-19T10:00:00Z', '2026-10-19T10:10:00Z');
    INSERT INTO events (id, session_id, ts, event_type, agent_slug, repo_id, rel_file_path, file_path, lines_added, lines_generated, lines_reworked, payload) VALUES
      ('j1', 'J', '2026-10-17T10:00:10Z', 'PostToolUse', 'codex', 'r1', 'a.go', '/tmp/demo/a.go', 2, 2, 1,
        '${post("apply_patch", { command: "*** Begin Patch" })}'),
      ('h1', 'H', '2026-10-18T10:00:10Z', 'PostToolUse', 'claude-code', 'r1', 'src/s.sh', '/tmp/demo/src/s.sh', NULL, 5, 0,
        '${post("Bash", { command: "cat > src/s.sh" })}'),
      ('h2', 'H', '2026-10-18T10:00:20Z', 'PostToolUse', 'claude-code', 'r1', 'src/s.sh', '/tmp/demo/src/s.sh', NULL, 0, 2,
        '${post("Bash", { command: "cat > src/s.sh" })}'),
      ('k1', 'K', '2026-10-19T10:00:10Z', 'PostToolUse', 'codex', 'r1', 'src/p.ts', '/tmp/demo/src/p.ts', 7, NULL, NULL,
        '${post("apply_patch", { command: "*** Begin Patch" })}');
    INSERT INTO event_line_ranges (id, event_id, file_path, rel_file_path, start_line, end_line, new_text_lines) VALUES
      ('r-j1a', 'j1', '/tmp/demo/a.go', 'a.go', 9, 9, 1),
      ('r-j1b', 'j1', '/tmp/demo/b.go', 'b.go', 3, 3, 1),
      ('r-h1', 'h1', '/tmp/demo/src/s.sh', 'src/s.sh', 1, 5, 5),
      ('r-k1a', 'k1', '/tmp/demo/src/p.ts', 'src/p.ts', 1, 4, 4),
      ('r-k1b', 'k1', '/tmp/demo/src/q.ts', 'src/q.ts', 1, 3, 3);

    UPDATE events SET file_lines = '${J([
      { rel_file_path: "src/g.ts", file_path: "/tmp/demo/src/g.ts", lines_generated: 4, lines_reworked: 0 },
      { rel_file_path: "src/h.ts", file_path: "/tmp/demo/src/h.ts", lines_generated: 2, lines_reworked: 0 },
    ])}' WHERE id = 'f4';
    UPDATE events SET file_lines = '${J([
      { rel_file_path: "a.go", file_path: "/tmp/demo/a.go", lines_generated: 3, lines_reworked: 0 },
      { rel_file_path: "b.go", file_path: "/tmp/demo/b.go", lines_generated: 2, lines_reworked: 0 },
    ])}' WHERE id = 'g1';
    UPDATE events SET file_lines = '${J([
      { rel_file_path: "a.go", file_path: "/tmp/demo/a.go", lines_generated: 1, lines_reworked: 0 },
      { rel_file_path: "b.go", file_path: "/tmp/demo/b.go", lines_generated: 1, lines_reworked: 1 },
      { rel_file_path: null, file_path: "/home/dev/scratch.md", lines_generated: 0, lines_reworked: 0 },
    ])}' WHERE id = 'j1';
  `);
  w.close();
}

const { listSessions, getSessionDetail } = await import("./sessions.ts");
const { sessionTimelineData } = await import("./turns.ts");
const { resolveFilters, parseFilters } = await import("../filters.ts");

const f = resolveFilters(parseFilters({ from: "2026-10-01", to: "2026-10-31" }));

test("a session's generated lines are the client's counts, net of its own rework", () => {
  // 5 + (2 - 2) + 0 + 6 + 3. The older estimate would say 9 + 2 + 40 + 6 + 0.
  assert.equal(getSessionDetail("F")?.lines_generated, 14);
  const row = listSessions(f, 1, 50).rows.find((r) => r.id === "F");
  assert.equal(row?.lines_generated, 14);
});

test("events an older client recorded are still scored the old way", () => {
  // Fixture session A has no lines_generated: 3 newlines in the Edit, 2 in the Write.
  assert.equal(getSessionDetail("A")?.lines_generated, 5);
});

test("an edit measured without a position still counts as an edit", () => {
  // f5 is a Bash call with no line ranges; lines_generated > 0 makes it an edit.
  assert.equal(getSessionDetail("F")?.edits, 5);
});

test("the files panel splits a multi-file edit by each range's own file", () => {
  const files = Object.fromEntries(
    sessionTimelineData("F").files.map((r) => [r.file, [r.edits, r.lines_generated]]),
  );
  assert.deepEqual(files, {
    "src/f.ts": [2, 5], // f1 wrote 5; f2 reworked as many as it wrote
    "src/g.ts": [1, 4], // f4's first file no longer takes all 6
    "src/h.ts": [1, 2],
    "src/k.sh": [1, 3],
    "/home/dev/plan.md": [1, 0], // listed as touched, counts for nothing
  });
});

test("a patch placed in only one of its files is split, not put on the first file", () => {
  const files = Object.fromEntries(
    sessionTimelineData("G").files.map((r) => [r.file, [r.edits, r.lines_generated]]),
  );
  // b.go gets its two located lines; the three that could not be placed stay
  // on a.go, the file the event names. Before, a.go showed all five and b.go
  // was missing.
  assert.deepEqual(files, { "a.go": [1, 3], "b.go": [1, 2] });
  assert.equal(getSessionDetail("G")?.lines_generated, 5);
});

test("rework in a multi-file patch is taken off the file it happened in", () => {
  const files = Object.fromEntries(
    sessionTimelineData("J").files.map((r) => [r.file, [r.edits, r.lines_generated]]),
  );
  // a.go keeps its new line; b.go's new line replaced one of the session's
  // own. Netting the event's rework on the first file gave a.go 0, b.go 1.
  // The scratch file outside the repo is listed with nothing.
  assert.deepEqual(files, { "a.go": [1, 1], "b.go": [1, 0], "/home/dev/scratch.md": [1, 0] });
  assert.equal(getSessionDetail("J")?.lines_generated, 1);
});

test("a shell rewrite that only removes the session's lines is an edit and is subtracted", () => {
  // h2 wrote nothing and is not an edit tool by name; without it the session
  // would still show the 5 lines h1 wrote.
  const detail = getSessionDetail("H");
  assert.equal(detail?.lines_generated, 3);
  assert.equal(detail?.edits, 2);
  const files = sessionTimelineData("H").files.map((r) => [r.file, r.edits, r.lines_generated]);
  assert.deepEqual(files, [["src/s.sh", 2, 3]]);
});

test("an older client's multi-file patch is still split by its ranges", () => {
  const files = Object.fromEntries(
    sessionTimelineData("K").files.map((r) => [r.file, [r.edits, r.lines_generated]]),
  );
  assert.deepEqual(files, { "src/p.ts": [1, 4], "src/q.ts": [1, 3] });
});

test("the files of a session add up to its total", () => {
  for (const id of ["A", "C", "F", "G", "H", "J", "K"]) {
    const sum = sessionTimelineData(id).files.reduce((n, r) => n + r.lines_generated, 0);
    assert.equal(sum, getSessionDetail(id)?.lines_generated, `session ${id}`);
  }
});

test("a tool call in the timeline shows what that edit wrote, before rework", () => {
  const byId = Object.fromEntries(sessionTimelineData("F").tools.map((t) => [t.id, t.lines_generated]));
  assert.equal(byId.f1, 5);
  assert.equal(byId.f2, 2);
  assert.equal(byId.f3, 0);
});
