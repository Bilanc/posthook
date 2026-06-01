import { DatabaseSync } from "node:sqlite";
import { homedir } from "node:os";
import { resolve } from "node:path";

// Statement surface the query layer uses (prepare → get/all). node:sqlite types
// its bind params as SQLInputValue[]; the query builders pass loosely-typed
// unknown[] (always string|number|null at runtime), so we expose a permissive
// facade here and keep the driver entirely contained to this file — the query
// files never reference the concrete driver type.
export interface Statement {
  get(...params: unknown[]): unknown;
  all(...params: unknown[]): unknown[];
}
export interface DB {
  prepare(sql: string): Statement;
}

// Single shared read-only connection. node:sqlite (built into Node >=24) is
// synchronous and backed by real SQLite, so it reads the live WAL-mode database
// correctly while the posthook CLI writes to it concurrently — the same
// guarantee better-sqlite3 gave us, but with no native build, so one bundle
// runs on every platform. The CLI creates the DB and keeps it in WAL mode; the
// dashboard only ever reads, hence readOnly (no journal_mode pragma needed).
let cached: DatabaseSync | null = null;

export function dbPath(): string {
  return process.env.POSTHOOK_DB ?? resolve(homedir(), ".posthook", "posthook.db");
}

export function db(): DB {
  if (!cached) {
    cached = new DatabaseSync(dbPath(), { readOnly: true });
  }
  const conn = cached;
  return {
    prepare(sql: string): Statement {
      return conn.prepare(sql) as unknown as Statement;
    },
  };
}
