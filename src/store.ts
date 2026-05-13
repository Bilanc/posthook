import { Database } from "bun:sqlite";
import { mkdirSync } from "node:fs";
import { randomUUID } from "node:crypto";
import { basename } from "node:path";
import { execSync } from "node:child_process";
import { DB_PATH, POSTHOOK_DIR } from "./config.ts";
import { findRepoRoot, relPathInRepo } from "./util/git.ts";

// org_id is "local" for the local-only MVP. When we migrate to Postgres for the
// SaaS, the same column accepts real org UUIDs without a schema change.
export const LOCAL_ORG_ID = "local";

const SCHEMA = `
CREATE TABLE IF NOT EXISTS schema_version (
  version INTEGER PRIMARY KEY,
  applied_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS repositories (
  id TEXT PRIMARY KEY,
  org_id TEXT NOT NULL DEFAULT 'local',
  remote_url TEXT,
  name TEXT NOT NULL,
  root_path TEXT NOT NULL UNIQUE,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_repositories_org ON repositories(org_id);

CREATE TABLE IF NOT EXISTS sessions (
  id TEXT PRIMARY KEY,
  org_id TEXT NOT NULL DEFAULT 'local',
  agent_slug TEXT NOT NULL,
  model_slug TEXT,
  repo_id TEXT REFERENCES repositories(id),
  branch TEXT,
  cwd TEXT,
  started_at TEXT NOT NULL,
  ended_at TEXT,
  tokens_in INTEGER NOT NULL DEFAULT 0,
  tokens_out INTEGER NOT NULL DEFAULT 0,
  tokens_cache_read INTEGER NOT NULL DEFAULT 0,
  tokens_cache_write INTEGER NOT NULL DEFAULT 0,
  cost_usd REAL NOT NULL DEFAULT 0,
  tool_calls_count INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_sessions_org_started ON sessions(org_id, started_at);
CREATE INDEX IF NOT EXISTS idx_sessions_agent ON sessions(agent_slug);
CREATE INDEX IF NOT EXISTS idx_sessions_repo ON sessions(repo_id);

CREATE TABLE IF NOT EXISTS events (
  id TEXT PRIMARY KEY,
  org_id TEXT NOT NULL DEFAULT 'local',
  session_id TEXT REFERENCES sessions(id),
  ts TEXT NOT NULL,
  event_type TEXT NOT NULL,
  agent_slug TEXT NOT NULL,
  cwd TEXT,
  file_path TEXT,
  repo_id TEXT REFERENCES repositories(id),
  rel_file_path TEXT,
  lines_added INTEGER,
  lines_removed INTEGER,
  tokens_in INTEGER,
  tokens_out INTEGER,
  payload TEXT
);
CREATE INDEX IF NOT EXISTS idx_events_org_ts ON events(org_id, ts);
CREATE INDEX IF NOT EXISTS idx_events_session ON events(session_id);
CREATE INDEX IF NOT EXISTS idx_events_type ON events(event_type);

CREATE TABLE IF NOT EXISTS commits (
  id TEXT PRIMARY KEY,
  org_id TEXT NOT NULL DEFAULT 'local',
  repo_id TEXT NOT NULL REFERENCES repositories(id),
  sha TEXT NOT NULL,
  parent_sha TEXT,
  author_email TEXT,
  author_name TEXT,
  committed_at TEXT NOT NULL,
  branch TEXT,
  message TEXT,
  lines_added INTEGER NOT NULL DEFAULT 0,
  lines_removed INTEGER NOT NULL DEFAULT 0,
  files_changed INTEGER NOT NULL DEFAULT 0,
  UNIQUE(repo_id, sha)
);
CREATE INDEX IF NOT EXISTS idx_commits_org_committed ON commits(org_id, committed_at);
CREATE INDEX IF NOT EXISTS idx_commits_repo ON commits(repo_id);

CREATE TABLE IF NOT EXISTS commit_files (
  commit_id TEXT NOT NULL REFERENCES commits(id),
  file_path TEXT NOT NULL,
  lines_added INTEGER NOT NULL DEFAULT 0,
  lines_removed INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (commit_id, file_path)
);
`;

const VERSION = 2;

let cachedDb: Database | null = null;

function applyMigrations(db: Database): void {
  // v1 → v2: events gains repo_id and rel_file_path. SQLite ADD COLUMN is idempotent only
  // when guarded, so we check pragma_table_info first.
  const cols = db
    .query<{ name: string }, []>("SELECT name FROM pragma_table_info('events')")
    .all();
  const names = new Set(cols.map((c) => c.name));
  if (!names.has("repo_id")) {
    db.exec("ALTER TABLE events ADD COLUMN repo_id TEXT REFERENCES repositories(id)");
  }
  if (!names.has("rel_file_path")) {
    db.exec("ALTER TABLE events ADD COLUMN rel_file_path TEXT");
  }
  db.exec("CREATE INDEX IF NOT EXISTS idx_events_repo ON events(repo_id)");
}

// Walk events with NULL repo_id, resolve their repo from cwd, and populate.
// Cheap to run on each open: we only touch rows that haven't been backfilled yet.
function backfillEventRepos(db: Database): number {
  const rows = db
    .query(
      `SELECT id, cwd, file_path
       FROM events
       WHERE repo_id IS NULL AND cwd IS NOT NULL AND cwd != ''`,
    )
    .all() as Array<{ id: string; cwd: string; file_path: string | null }>;
  if (rows.length === 0) return 0;

  const upsert = db.prepare(
    `INSERT INTO repositories (id, org_id, remote_url, name, root_path)
     VALUES (?, 'local', ?, ?, ?)
     ON CONFLICT(root_path) DO NOTHING`,
  );
  const update = db.prepare(
    `UPDATE events SET repo_id = ?, rel_file_path = ? WHERE id = ?`,
  );

  let touched = 0;
  // Cache repo lookups so we don't fs-walk for every event in the same cwd.
  const cwdToRepo = new Map<string, { id: string; root: string } | null>();
  for (const r of rows) {
    let resolved = cwdToRepo.get(r.cwd);
    if (resolved === undefined) {
      const root = findRepoRoot(r.cwd);
      if (root) {
        const existing = db
          .query("SELECT id FROM repositories WHERE root_path = ?")
          .get(root) as { id: string } | undefined;
        let id: string;
        if (existing) {
          id = existing.id;
        } else {
          id = randomUUID();
          let remoteUrl: string | null = null;
          try {
            remoteUrl = execSync(`git -C "${root}" config --get remote.origin.url`, {
              encoding: "utf8",
            }).trim();
          } catch {
            // no remote — fine
          }
          upsert.run(id, remoteUrl, basename(root), root);
        }
        resolved = { id, root };
      } else {
        resolved = null;
      }
      cwdToRepo.set(r.cwd, resolved);
    }
    if (!resolved) continue;
    const rel = r.file_path ? relPathInRepo(resolved.root, r.file_path) : null;
    update.run(resolved.id, rel ?? null, r.id);
    touched++;
  }
  return touched;
}

export function openDb(): Database {
  if (cachedDb) return cachedDb;
  mkdirSync(POSTHOOK_DIR, { recursive: true });
  const db = new Database(DB_PATH, { create: true });
  db.exec("PRAGMA journal_mode = WAL;");
  db.exec("PRAGMA foreign_keys = ON;");
  db.exec(SCHEMA);
  applyMigrations(db);
  backfillEventRepos(db);
  const row = db.query("SELECT MAX(version) AS v FROM schema_version").get() as { v: number | null };
  if ((row?.v ?? 0) < VERSION) {
    db.run("INSERT INTO schema_version (version) VALUES (?)", [VERSION]);
  }
  cachedDb = db;
  return db;
}

export function closeDb(): void {
  cachedDb?.close();
  cachedDb = null;
}
