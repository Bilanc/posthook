import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { readUsage } from "./transcript.ts";

function transcript(lines: unknown[]): string {
  const dir = mkdtempSync(join(tmpdir(), "posthook-transcript-"));
  const path = join(dir, "session.jsonl");
  writeFileSync(path, lines.map((l) => JSON.stringify(l)).join("\n") + "\n");
  return path;
}

// A Pi (pi.dev) session file: usage rides on each assistant message entry,
// and usage.input is already the uncached part of the prompt.
test("readUsage reads Pi assistant messages", () => {
  const path = transcript([
    { type: "session", version: 3, id: "s1", timestamp: "2026-11-19T15:42:34.952Z", cwd: "/repo" },
    { type: "message", id: "a1", parentId: null, timestamp: "2026-11-19T15:42:40.000Z", message: { role: "user", content: "hi", timestamp: 1 } },
    {
      type: "message", id: "a2", parentId: "a1", timestamp: "2026-11-19T15:42:50.000Z",
      message: { role: "assistant", model: "claude-opus", usage: { input: 120, output: 10, cacheRead: 9000, cacheWrite: 300, reasoning: 4, totalTokens: 9430 }, timestamp: 2 },
    },
    { type: "message", id: "a3", parentId: "a2", timestamp: "2026-11-19T15:42:51.000Z", message: { role: "toolResult", toolName: "edit", content: [], timestamp: 3 } },
    {
      type: "message", id: "a4", parentId: "a3", timestamp: "2026-11-19T15:43:00.000Z",
      message: { role: "assistant", model: "gpt-6", usage: { input: 15, output: 20, cacheRead: 9400, cacheWrite: 0, totalTokens: 9435 }, timestamp: 4 },
    },
  ]);
  assert.deepEqual(readUsage(path), [
    {
      ts: "2026-11-19T15:42:50.000Z", model: "claude-opus",
      input_tokens: 120, output_tokens: 10, cache_read_tokens: 9000, cache_creation_tokens: 300,
      thinking_tokens: 4, thinking_ms: null,
    },
    {
      ts: "2026-11-19T15:43:00.000Z", model: "gpt-6",
      input_tokens: 15, output_tokens: 20, cache_read_tokens: 9400, cache_creation_tokens: 0,
      thinking_tokens: null, thinking_ms: null,
    },
  ]);
});

test("readUsage returns null for a Pi session with no usage", () => {
  const path = transcript([
    { type: "session", version: 3, id: "s1", timestamp: "2026-11-19T15:42:34.952Z", cwd: "/repo" },
    { type: "message", id: "a1", parentId: null, timestamp: "2026-11-19T15:42:40.000Z", message: { role: "user", content: "hi", timestamp: 1 } },
  ]);
  assert.equal(readUsage(path), null);
});
