import { test } from "node:test";
import assert from "node:assert/strict";
import { DatabaseSync } from "node:sqlite";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

// Point db.ts at a throwaway database before importing it (db() opens lazily,
// reading POSTHOOK_DB on first call).
const dir = mkdtempSync(join(tmpdir(), "posthook-dash-db-test-"));
const path = join(dir, "posthook.db");
const setup = new DatabaseSync(path);
setup.exec(
  "CREATE TABLE t (id INTEGER, name TEXT); INSERT INTO t VALUES (1, 'a'), (2, 'b');",
);
setup.close();
process.env.POSTHOOK_DB = path;

const { db } = await import("./db.ts");

// Regression: node:sqlite returns rows with a null prototype, which React
// refuses to serialize from Server to Client Components ("Only plain objects
// ... can be passed to Client Components"). The db facade must hand back
// plain objects.
test("get() returns a plain object, not a null-prototype row", () => {
  const row = db().prepare("SELECT * FROM t WHERE id = ?").get(1);
  assert.ok(row);
  assert.equal(Object.getPrototypeOf(row), Object.prototype);
  assert.deepEqual(row, { id: 1, name: "a" });
});

test("all() returns plain objects, not null-prototype rows", () => {
  const rows = db().prepare("SELECT * FROM t ORDER BY id").all();
  assert.equal(rows.length, 2);
  for (const row of rows) {
    assert.equal(Object.getPrototypeOf(row), Object.prototype);
  }
  assert.deepEqual(rows, [
    { id: 1, name: "a" },
    { id: 2, name: "b" },
  ]);
});

test("get() with no matching row still returns undefined", () => {
  assert.equal(db().prepare("SELECT * FROM t WHERE id = ?").get(99), undefined);
});
