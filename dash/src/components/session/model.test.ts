import { test } from "node:test";
import assert from "node:assert/strict";
import {
  clampText,
  commitGithubUrl,
  fileRows,
  layoutTimeline,
  niceCeil,
  resendDelays,
  shortToolName,
  spanLabel,
} from "./model.ts";

test("commitGithubUrl parses ssh and https remotes, rejects others", () => {
  assert.equal(
    commitGithubUrl("git@github.com:Bilanc/posthook.git", "abc"),
    "https://github.com/Bilanc/posthook/commit/abc",
  );
  assert.equal(
    commitGithubUrl("https://github.com/Bilanc/posthook", "abc"),
    "https://github.com/Bilanc/posthook/commit/abc",
  );
  assert.equal(commitGithubUrl("https://gitlab.com/x/y.git", "abc"), null);
  assert.equal(commitGithubUrl(null, "abc"), null);
});

test("shortToolName rewrites the mcp prefix and clips", () => {
  assert.equal(shortToolName("Bash"), "Bash");
  assert.equal(shortToolName("mcp__linear__create_issue"), "mcp:linear__c…");
  assert.equal(shortToolName(null), "tool");
});

test("fileRows unions edited and committed files and floors generated at committed", () => {
  const out = fileRows(
    [
      { file: "a.go", edits: 2, lines_generated: 100 },
      { file: "b.go", edits: 1, lines_generated: 10 },
    ],
    [
      { commit_id: "c1", file_path: "a.go", lines_attributed: 60 },
      { commit_id: "c2", file_path: "a.go", lines_attributed: 20 },
      { commit_id: "c1", file_path: "shell.sh", lines_attributed: 15 },
    ],
    [
      { id: "c1", sha: "x", committed_at: "", message: null, lines_added: 1, lines_removed: 0, files_changed: 1, lines_attributed: 75, files_touched: 2 },
      { id: "c2", sha: "y", committed_at: "", message: null, lines_added: 1, lines_removed: 0, files_changed: 1, lines_attributed: 20, files_touched: 1 },
    ],
  );
  assert.deepEqual(
    out.rows.map((r) => [r.path, r.generated, r.committed]),
    [
      ["a.go", 100, 80],
      ["shell.sh", 15, 15],
      ["b.go", 10, 0],
    ],
  );
  assert.equal(out.generated, 125);
  assert.equal(out.committed, 95);
});

test("fileRows keeps the same path in two repositories apart and names the repository", () => {
  const out = fileRows(
    [
      { file: "README.md", edits: 2, lines_generated: 4, repo_id: "r1", repo_name: "demo" },
      { file: "README.md", edits: 2, lines_generated: 6, repo_id: "r2", repo_name: "other" },
      { file: "/home/dev/plan.md", edits: 1, lines_generated: 0, repo_id: null, repo_name: null },
    ],
    [
      // Committed in the second repository: it must land on that README.
      { commit_id: "c1", file_path: "README.md", lines_attributed: 5, repo_id: "r2", repo_name: "other" },
    ],
    [
      { id: "c1", sha: "x", committed_at: "", message: null, lines_added: 5, lines_removed: 0, files_changed: 1, lines_attributed: 5, files_touched: 1 },
    ],
  );
  assert.deepEqual(
    out.rows.map((r) => [r.path, r.generated, r.committed, r.edits]),
    [
      ["other/README.md", 6, 5, 2],
      ["demo/README.md", 4, 0, 2],
      // A file in no repository keeps its own path.
      ["/home/dev/plan.md", 0, 0, 1],
    ],
  );
});

test("fileRows tells repositories with the same name apart and keys every row uniquely", () => {
  // A worktree beside the main clone shares the remote's name; two clones in
  // folders of the same name share even that.
  const out = fileRows(
    [
      { file: "README.md", edits: 1, lines_generated: 4, repo_id: "r1", repo_name: "posthook", repo_root: "/work/posthook" },
      { file: "README.md", edits: 1, lines_generated: 20, repo_id: "r2", repo_name: "posthook", repo_root: "/work/posthook-wt-fix" },
      { file: "README.md", edits: 1, lines_generated: 7, repo_id: "r3", repo_name: "demo", repo_root: "/work/one/demo" },
      { file: "README.md", edits: 1, lines_generated: 9, repo_id: "r4", repo_name: "demo", repo_root: "/work/two/demo" },
      { file: "README.md", edits: 1, lines_generated: 1, repo_id: "r5", repo_name: "docs", repo_root: "/work/docs" },
    ],
    [],
    [],
  );
  assert.deepEqual(
    out.rows.map((r) => [r.path, r.generated]),
    [
      ["posthook-wt-fix/README.md", 20],
      ["/work/two/demo/README.md", 9],
      ["/work/one/demo/README.md", 7],
      ["posthook/README.md", 4],
      ["docs/README.md", 1],
    ],
  );
  assert.equal(new Set(out.rows.map((r) => r.key)).size, 5);
  assert.equal(new Set(out.rows.map((r) => r.path)).size, 5);
});

test("fileRows shows bare paths when every file is in one repository", () => {
  const out = fileRows(
    [
      { file: "a.go", edits: 1, lines_generated: 3, repo_id: "r1", repo_name: "demo" },
      { file: "b.go", edits: 1, lines_generated: 2, repo_id: "r1", repo_name: "demo" },
    ],
    [{ commit_id: "c1", file_path: "a.go", lines_attributed: 3, repo_id: "r1", repo_name: "demo" }],
    [],
  );
  assert.deepEqual(
    out.rows.map((r) => r.path),
    ["a.go", "b.go"],
  );
});

test("resendDelays measures each prompt against the previous one by seq", () => {
  const d = resendDelays([
    { ts: "2026-10-10T10:00:00Z", seq: 1, text: "a" },
    { ts: "2026-10-10T10:01:20Z", seq: 2, text: "a" },
    { ts: null, seq: 3, text: "b" },
  ]);
  assert.deepEqual(d, { 2: 80 });
});

test("clampText cuts on chars or lines and keeps words whole", () => {
  assert.deepEqual(clampText("short", 400, 6), { head: "short", tail: "" });
  const byLines = clampText("1\n2\n3\n4", 400, 2);
  assert.equal(byLines.head, "1\n2");
  assert.equal(byLines.tail, "3\n4");
  const words = "alpha beta gamma delta epsilon";
  const byChars = clampText(words, 18, 6);
  assert.equal(byChars.head, "alpha beta gamma");
  assert.equal(byChars.tail, "delta epsilon");
});

test("layoutTimeline collapses idle gaps to a fixed break and spreads active time", () => {
  const m = 60_000;
  const ts = [0, 2 * m, 4 * m, 64 * m, 66 * m]; // one 60-minute gap in the middle
  const L = layoutTimeline(ts, { width: 900, padL: 6, padR: 6, gapMs: 8 * m, gapWidth: 34 });
  assert.equal(L.width, 900);
  assert.equal(L.gaps.length, 1);
  assert.equal(L.gaps[0].ms, 60 * m);
  assert.equal(L.gaps[0].w, 34);
  assert.equal(L.activeMs, 6 * m);
  // Active minutes share the remaining width equally: 4 active minutes → 4/6 of it.
  const usable = 900 - 12 - 34;
  assert.ok(Math.abs(L.x(4 * m) - (6 + usable * (4 / 6))) < 1e-6);
  assert.ok(Math.abs(L.x(64 * m) - (6 + usable * (4 / 6) + 34)) < 1e-6);
  assert.ok(Math.abs(L.x(66 * m) - 894) < 1e-6);
  // Interpolates inside a stretch and clamps outside the data.
  assert.ok(Math.abs(L.x(1 * m) - (6 + usable * (1 / 6))) < 1e-6);
  assert.equal(L.x(-5), 6);
  assert.equal(L.x(999 * m), 894);
  assert.equal(L.x(NaN), 6);
});

test("layoutTimeline widens when breaks alone would not fit and never divides by zero", () => {
  const m = 60_000;
  const ts: number[] = [];
  for (let i = 0; i < 40; i++) ts.push(i * 20 * m); // 39 gaps, all idle
  const L = layoutTimeline(ts, { width: 900, padL: 6, padR: 6, gapMs: 8 * m, gapWidth: 34, minActivePx: 100 });
  assert.equal(L.gaps.length, 39);
  assert.equal(L.width, 12 + 39 * 34 + 100);
  assert.ok(Number.isFinite(L.x(10 * m)));
  const single = layoutTimeline([5], { width: 900, padL: 6, padR: 6, gapMs: 8 * m, gapWidth: 34 });
  assert.equal(single.x(5), 6);
  assert.equal(layoutTimeline([], { width: 900, padL: 6, padR: 6, gapMs: 8 * m, gapWidth: 34 }).x(1), 6);
});

test("niceCeil rounds up to 1/2/5 steps", () => {
  assert.equal(niceCeil(0), 1);
  assert.equal(niceCeil(83_653), 100_000);
  assert.equal(niceCeil(17_291), 20_000);
  assert.equal(niceCeil(4_476), 5_000);
  assert.equal(niceCeil(1000), 1000);
});

test("spanLabel picks seconds, minutes or hours", () => {
  assert.equal(spanLabel(42_000), "42s");
  assert.equal(spanLabel(3 * 60_000), "3m");
  assert.equal(spanLabel(65 * 60_000), "1h 05m");
  assert.equal(spanLabel(NaN), "—");
});
