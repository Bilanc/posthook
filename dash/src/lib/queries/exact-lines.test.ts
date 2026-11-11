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
// Session L: a shell command whose row names a.go (edited in place, content
// unknown) while the one line posthook measured went to b.go.
//
// Session R: the session writes 30 lines to b.go; one patch then names a.go
// first, adds a line there and replaces the 30 lines in b.go with five.
// Session S: the same, after the session had already written 3 lines to a.go.
// Session M: a measured patch with no split (as a pre-release build recorded
// it) whose ranges are 4 lines in a.go and 2 in b.go.
// Session N: the shell command of session L as a build without the split
// recorded it: the row names a.go, the one range is in b.go.
//
// Session T: writes README.md in two repositories, first with one Write
// each, then with one patch that touches both.
//
// An event whose counts are not all its own row's file carries the split in
// events.file_lines.
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
      ('K', 'codex', 'gpt-6', 'r1', 'main', '2026-10-19T10:00:00Z', '2026-10-19T10:10:00Z'),
      ('L', 'claude-code', 'claude-fable-5-1', 'r1', 'main', '2026-10-20T10:00:00Z', '2026-10-20T10:10:00Z'),
      ('R', 'codex', 'gpt-6', 'r1', 'main', '2026-10-21T10:00:00Z', '2026-10-21T10:10:00Z'),
      ('S', 'codex', 'gpt-6', 'r1', 'main', '2026-10-22T10:00:00Z', '2026-10-22T10:10:00Z'),
      ('M', 'codex', 'gpt-6', 'r1', 'main', '2026-10-23T10:00:00Z', '2026-10-23T10:10:00Z'),
      ('N', 'claude-code', 'claude-fable-5-1', 'r1', 'main', '2026-10-24T10:00:00Z', '2026-10-24T10:10:00Z');
    INSERT INTO events (id, session_id, ts, event_type, agent_slug, repo_id, rel_file_path, file_path, lines_added, lines_generated, lines_reworked, payload) VALUES
      ('j1', 'J', '2026-10-17T10:00:10Z', 'PostToolUse', 'codex', 'r1', 'a.go', '/tmp/demo/a.go', 2, 2, 1,
        '${post("apply_patch", { command: "*** Begin Patch" })}'),
      ('h1', 'H', '2026-10-18T10:00:10Z', 'PostToolUse', 'claude-code', 'r1', 'src/s.sh', '/tmp/demo/src/s.sh', NULL, 5, 0,
        '${post("Bash", { command: "cat > src/s.sh" })}'),
      ('h2', 'H', '2026-10-18T10:00:20Z', 'PostToolUse', 'claude-code', 'r1', 'src/s.sh', '/tmp/demo/src/s.sh', NULL, 0, 2,
        '${post("Bash", { command: "cat > src/s.sh" })}'),
      ('k1', 'K', '2026-10-19T10:00:10Z', 'PostToolUse', 'codex', 'r1', 'src/p.ts', '/tmp/demo/src/p.ts', 7, NULL, NULL,
        '${post("apply_patch", { command: "*** Begin Patch" })}'),
      ('l1', 'L', '2026-10-20T10:00:10Z', 'PostToolUse', 'claude-code', 'r1', 'a.go', '/tmp/demo/a.go', NULL, 1, 0,
        '${post("Bash", { command: "sed -i s/old/new/ a.go; echo hello > b.go" })}'),
      ('r1', 'R', '2026-10-21T10:00:10Z', 'PostToolUse', 'codex', 'r1', 'b.go', '/tmp/demo/b.go', NULL, 30, 0,
        '${post("Write", { content: "x" })}'),
      ('r2', 'R', '2026-10-21T10:00:20Z', 'PostToolUse', 'codex', 'r1', 'a.go', '/tmp/demo/a.go', 6, 6, 30,
        '${post("apply_patch", { command: "*** Begin Patch" })}'),
      ('s1', 'S', '2026-10-22T10:00:05Z', 'PostToolUse', 'codex', 'r1', 'a.go', '/tmp/demo/a.go', NULL, 3, 0,
        '${post("Edit", { old_string: "", new_string: "x" })}'),
      ('s2', 'S', '2026-10-22T10:00:10Z', 'PostToolUse', 'codex', 'r1', 'b.go', '/tmp/demo/b.go', NULL, 30, 0,
        '${post("Write", { content: "x" })}'),
      ('s3', 'S', '2026-10-22T10:00:20Z', 'PostToolUse', 'codex', 'r1', 'a.go', '/tmp/demo/a.go', 6, 6, 30,
        '${post("apply_patch", { command: "*** Begin Patch" })}'),
      ('m1', 'M', '2026-10-23T10:00:10Z', 'PostToolUse', 'codex', 'r1', 'a.go', '/tmp/demo/a.go', 6, 6, 0,
        '${post("apply_patch", { command: "*** Begin Patch" })}'),
      ('n1', 'N', '2026-10-24T10:00:10Z', 'PostToolUse', 'claude-code', 'r1', 'a.go', '/tmp/demo/a.go', NULL, 1, 0,
        '${post("Bash", { command: "sed -i s/old/new/ a.go; echo hello > b.go" })}');
    INSERT INTO event_line_ranges (id, event_id, file_path, rel_file_path, start_line, end_line, new_text_lines) VALUES
      ('r-j1a', 'j1', '/tmp/demo/a.go', 'a.go', 9, 9, 1),
      ('r-j1b', 'j1', '/tmp/demo/b.go', 'b.go', 3, 3, 1),
      ('r-h1', 'h1', '/tmp/demo/src/s.sh', 'src/s.sh', 1, 5, 5),
      ('r-k1a', 'k1', '/tmp/demo/src/p.ts', 'src/p.ts', 1, 4, 4),
      ('r-k1b', 'k1', '/tmp/demo/src/q.ts', 'src/q.ts', 1, 3, 3),
      ('r-l1', 'l1', '/tmp/demo/b.go', 'b.go', 1, 1, 1),
      ('r-r1', 'r1', '/tmp/demo/b.go', 'b.go', 1, 30, 30),
      ('r-r2a', 'r2', '/tmp/demo/a.go', 'a.go', 2, 2, 1),
      ('r-r2b', 'r2', '/tmp/demo/b.go', 'b.go', 1, 5, 5),
      ('r-s1', 's1', '/tmp/demo/a.go', 'a.go', 1, 3, 3),
      ('r-s2', 's2', '/tmp/demo/b.go', 'b.go', 1, 30, 30),
      ('r-s3a', 's3', '/tmp/demo/a.go', 'a.go', 4, 4, 1),
      ('r-s3b', 's3', '/tmp/demo/b.go', 'b.go', 1, 5, 5),
      ('r-m1a', 'm1', '/tmp/demo/a.go', 'a.go', 1, 4, 4),
      ('r-m1b', 'm1', '/tmp/demo/b.go', 'b.go', 1, 2, 2),
      ('r-n1', 'n1', '/tmp/demo/b.go', 'b.go', 1, 1, 1);

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
    UPDATE events SET file_lines = '${J([
      { rel_file_path: "b.go", file_path: "/tmp/demo/b.go", lines_generated: 1, lines_reworked: 0 },
    ])}' WHERE id = 'l1';
    UPDATE events SET file_lines = '${J([
      { rel_file_path: "a.go", file_path: "/tmp/demo/a.go", lines_generated: 1, lines_reworked: 0 },
      { rel_file_path: "b.go", file_path: "/tmp/demo/b.go", lines_generated: 5, lines_reworked: 30 },
    ])}' WHERE id IN ('r2', 's3');

    INSERT INTO repositories (id, name, root_path) VALUES ('r2', 'other', '/tmp/other');
    INSERT INTO sessions (id, agent_slug, model_slug, repo_id, branch, started_at, ended_at)
    VALUES ('T', 'codex', 'gpt-6', 'r1', 'main', '2026-10-25T10:00:00Z', '2026-10-25T10:10:00Z');
    INSERT INTO events (id, session_id, ts, event_type, agent_slug, repo_id, rel_file_path, file_path, lines_added, lines_generated, lines_reworked, payload) VALUES
      ('t1', 'T', '2026-10-25T10:00:10Z', 'PostToolUse', 'codex', 'r1', 'README.md', '/tmp/demo/README.md', NULL, 3, 0,
        '${post("Write", { content: "x" })}'),
      ('t2', 'T', '2026-10-25T10:00:20Z', 'PostToolUse', 'codex', 'r2', 'README.md', '/tmp/other/README.md', NULL, 2, 0,
        '${post("Write", { content: "x" })}'),
      ('t3', 'T', '2026-10-25T10:00:30Z', 'PostToolUse', 'codex', 'r1', 'README.md', '/tmp/demo/README.md', 5, 5, 0,
        '${post("apply_patch", { command: "*** Begin Patch" })}');
    INSERT INTO event_line_ranges (id, event_id, file_path, rel_file_path, start_line, end_line, new_text_lines) VALUES
      ('r-t1', 't1', '/tmp/demo/README.md', 'README.md', 1, 3, 3),
      ('r-t2', 't2', '/tmp/other/README.md', 'README.md', 1, 2, 2),
      ('r-t3a', 't3', '/tmp/demo/README.md', 'README.md', 4, 4, 1),
      ('r-t3b', 't3', '/tmp/other/README.md', 'README.md', 3, 6, 4);
    UPDATE events SET file_lines = '${J([
      { repo_id: "r1", rel_file_path: "README.md", file_path: "/tmp/demo/README.md", lines_generated: 1, lines_reworked: 0 },
      { repo_id: "r2", rel_file_path: "README.md", file_path: "/tmp/other/README.md", lines_generated: 4, lines_reworked: 0 },
    ])}' WHERE id = 't3';
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

test("a split of one file moves the count off the file the event row names", () => {
  const files = sessionTimelineData("L").files.map((r) => [r.file, r.edits, r.lines_generated]);
  // The row names a.go; the measured line is b.go's.
  assert.deepEqual(files, [["b.go", 1, 1]]);
});

test("a patch that replaces 30 of the session's lines in another file nets them there", () => {
  const files = Object.fromEntries(
    sessionTimelineData("R").files.map((r) => [r.file, [r.edits, r.lines_generated]]),
  );
  // 30 written to b.go, then a patch: +1 in a.go, and b.go's 30 become 5.
  // Taking the patch's rework off the file its row names gave a.go 0, b.go 35.
  assert.deepEqual(files, { "a.go": [1, 1], "b.go": [2, 5] });
  assert.equal(getSessionDetail("R")?.lines_generated, 6);
});

test("rework in one file of a patch leaves the other file's earlier edits alone", () => {
  const files = Object.fromEntries(
    sessionTimelineData("S").files.map((r) => [r.file, [r.edits, r.lines_generated]]),
  );
  // a.go already had 3 of the session's lines; the patch adds 1 more there.
  assert.deepEqual(files, { "a.go": [2, 4], "b.go": [2, 5] });
  assert.equal(getSessionDetail("S")?.lines_generated, 9);
});

test("a measured edit without a split follows its ranges", () => {
  // Ranges in two files: split by range path, as an older client's patch is.
  const multi = Object.fromEntries(
    sessionTimelineData("M").files.map((r) => [r.file, [r.edits, r.lines_generated]]),
  );
  assert.deepEqual(multi, { "a.go": [1, 4], "b.go": [1, 2] });
  // One range, in a file the row does not name: the count goes to that file.
  const single = sessionTimelineData("N").files.map((r) => [r.file, r.edits, r.lines_generated]);
  assert.deepEqual(single, [["b.go", 1, 1]]);
});

test("the same path in two repositories is two files", () => {
  const files = sessionTimelineData("T").files.map((r) => [r.repo_name, r.file, r.edits, r.lines_generated]);
  // Each README has a Write of its own and a share of the patch: 3 + 1 and
  // 2 + 4. Keyed by path alone they were one row of 10.
  assert.deepEqual(files, [
    ["other", "README.md", 2, 6],
    ["demo", "README.md", 2, 4],
  ]);
  assert.equal(getSessionDetail("T")?.lines_generated, 10);
});

test("a single-repository session carries its one repository on every row", () => {
  const repos = new Set(sessionTimelineData("F").files.map((r) => r.repo_name));
  assert.deepEqual([...repos], ["demo"]);
});

test("an older client's multi-file patch is still split by its ranges", () => {
  const files = Object.fromEntries(
    sessionTimelineData("K").files.map((r) => [r.file, [r.edits, r.lines_generated]]),
  );
  assert.deepEqual(files, { "src/p.ts": [1, 4], "src/q.ts": [1, 3] });
});

test("the files of a session add up to its total", () => {
  for (const id of ["A", "C", "F", "G", "H", "J", "K", "L", "M", "N", "R", "S", "T"]) {
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
