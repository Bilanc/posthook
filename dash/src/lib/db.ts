import Database from "better-sqlite3";
import { homedir } from "node:os";
import { resolve } from "node:path";

// Single shared connection. better-sqlite3 is synchronous; safe across server
// components in a single-process Next.js server. Concurrent reads on the same
// connection are fine in WAL mode (posthook sets PRAGMA journal_mode=WAL).
let cached: Database.Database | null = null;

export function dbPath(): string {
  return process.env.POSTHOOK_DB ?? resolve(homedir(), ".posthook", "posthook.db");
}

export function db(): Database.Database {
  if (cached) return cached;
  const conn = new Database(dbPath(), { readonly: true, fileMustExist: true });
  conn.pragma("journal_mode = WAL");
  cached = conn;
  return conn;
}
