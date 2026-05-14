import { Database } from "bun:sqlite";
import { mkdirSync } from "node:fs";
import { randomUUID } from "node:crypto";
import { basename } from "node:path";
import { execSync } from "node:child_process";
import { DB_PATH, POSTHOOK_DIR } from "./config.ts";
import { parseClaudeTranscriptSync } from "./transcript.ts";
import { findRepoRoot, gitBypassEnv, relPathInRepo } from "./util/git.ts";

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
  engineer_email TEXT,
  engineer_name TEXT
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

CREATE TABLE IF NOT EXISTS event_line_ranges (
  id TEXT PRIMARY KEY,
  event_id TEXT NOT NULL REFERENCES events(id),
  file_path TEXT NOT NULL,
  rel_file_path TEXT,
  blob_sha_after TEXT,
  start_line INTEGER NOT NULL,
  end_line INTEGER NOT NULL,
  new_text_lines INTEGER NOT NULL,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_elr_event ON event_line_ranges(event_id);
CREATE INDEX IF NOT EXISTS idx_elr_relpath ON event_line_ranges(rel_file_path);
`;

const VERSION = 5;

let cachedDb: Database | null = null;

function colNames(db: Database, table: string): Set<string> {
  const rows = db
    .query<{ name: string }, []>(`SELECT name FROM pragma_table_info('${table}')`)
    .all();
  return new Set(rows.map((c) => c.name));
}

function applyMigrations(db: Database): void {
  // v1 → v2: events gains repo_id and rel_file_path.
  const eventCols = colNames(db, "events");
  if (!eventCols.has("repo_id")) {
    db.exec("ALTER TABLE events ADD COLUMN repo_id TEXT REFERENCES repositories(id)");
  }
  if (!eventCols.has("rel_file_path")) {
    db.exec("ALTER TABLE events ADD COLUMN rel_file_path TEXT");
  }
  db.exec("CREATE INDEX IF NOT EXISTS idx_events_repo ON events(repo_id)");

  // v2 → v3: drop cost/token columns. Cross-agent token counts and static pricing
  // both produced misleading numbers, so we stop tracking them at the schema level.
  // Tokens for Claude Code still live in the on-disk transcript file and in events.payload
  // — anything that wants them can re-parse from there.
  const sessionCols = colNames(db, "sessions");
  for (const dead of [
    "tokens_in",
    "tokens_out",
    "tokens_cache_read",
    "tokens_cache_write",
    "cost_usd",
    "tool_calls_count",
  ]) {
    if (sessionCols.has(dead)) {
      db.exec(`ALTER TABLE sessions DROP COLUMN ${dead}`);
    }
  }
  const eventColsV3 = colNames(db, "events");
  for (const dead of ["tokens_in", "tokens_out"]) {
    if (eventColsV3.has(dead)) {
      db.exec(`ALTER TABLE events DROP COLUMN ${dead}`);
    }
  }

  // v4 → v5: sessions gains engineer_email and engineer_name. Captured from
  // `git config user.email` / `user.name` at session creation time so the
  // dashboard can break down metrics by engineer.
  const sessionColsV5 = colNames(db, "sessions");
  if (!sessionColsV5.has("engineer_email")) {
    db.exec("ALTER TABLE sessions ADD COLUMN engineer_email TEXT");
  }
  if (!sessionColsV5.has("engineer_name")) {
    db.exec("ALTER TABLE sessions ADD COLUMN engineer_name TEXT");
  }
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
              env: gitBypassEnv(),
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

// Sessions with NULL model_slug typically belong to in-flight Claude Code sessions
// where Stop hasn't fired yet. Claude Code hook payloads don't carry `model`, so the
// only authoritative source mid-session is the transcript JSONL. We walk each NULL
// session, find its most recent event with a transcript_path, and parse to backfill.
// Cheap to re-run: only touches sessions that are still NULL.
function backfillSessionModels(db: Database): number {
  const sessions = db
    .query(
      `SELECT id FROM sessions WHERE model_slug IS NULL OR model_slug = ''`,
    )
    .all() as Array<{ id: string }>;
  if (sessions.length === 0) return 0;

  const findTranscript = db.prepare(
    `SELECT json_extract(payload, '$.transcript_path') AS path
     FROM events
     WHERE session_id = ?
       AND json_extract(payload, '$.transcript_path') IS NOT NULL
     ORDER BY ts DESC
     LIMIT 1`,
  );
  const update = db.prepare(`UPDATE sessions SET model_slug = ? WHERE id = ?`);

  let touched = 0;
  for (const s of sessions) {
    const row = findTranscript.get(s.id) as { path: string | null } | undefined;
    if (!row?.path) continue;
    const summary = parseClaudeTranscriptSync(row.path);
    if (summary?.model) {
      update.run(summary.model, s.id);
      touched++;
    }
  }
  return touched;
}

// Sessions created before events were repo-resolved (or before the engineer-capture
// migration shipped) may have NULL repo_id even when their events do. Walk those
// sessions and pick the most common events.repo_id. Prerequisite for engineer
// backfill, which joins sessions → commits via repo_id.
function backfillSessionRepos(db: Database): number {
  const sessions = db
    .query(
      `SELECT id FROM sessions WHERE repo_id IS NULL`,
    )
    .all() as Array<{ id: string }>;
  if (sessions.length === 0) return 0;

  const pickRepo = db.prepare(
    `SELECT repo_id, COUNT(*) AS n
     FROM events
     WHERE session_id = ? AND repo_id IS NOT NULL
     GROUP BY repo_id
     ORDER BY n DESC
     LIMIT 1`,
  );
  const update = db.prepare(`UPDATE sessions SET repo_id = ? WHERE id = ?`);

  let touched = 0;
  for (const s of sessions) {
    const row = pickRepo.get(s.id) as { repo_id: string; n: number } | undefined;
    if (!row?.repo_id) continue;
    update.run(row.repo_id, s.id);
    touched++;
  }
  return touched;
}

// Backfill engineer_email/engineer_name for sessions that pre-date the v5
// migration (no git-config snapshot at creation time). Strategy: for each
// NULL-engineer session, find commits in the same repo whose committed_at
// falls inside the session's time window. If exactly one distinct author
// shows up, attribute the session to them. If zero or more than one author
// shows up, leave NULL — we'd rather have missing data than misattribute on
// a shared dev box.
function backfillSessionEngineers(db: Database): number {
  const sessions = db
    .query(
      `SELECT s.id, s.repo_id, s.started_at,
              COALESCE(s.ended_at, (SELECT MAX(ts) FROM events e WHERE e.session_id = s.id)) AS end_ts
       FROM sessions s
       WHERE s.engineer_email IS NULL
         AND s.repo_id IS NOT NULL
         AND s.started_at IS NOT NULL`,
    )
    .all() as Array<{
    id: string;
    repo_id: string;
    started_at: string;
    end_ts: string | null;
  }>;
  if (sessions.length === 0) return 0;

  const findAuthors = db.prepare(
    `SELECT author_email, author_name, COUNT(*) AS n
     FROM commits
     WHERE repo_id = ?
       AND author_email IS NOT NULL
       AND datetime(committed_at) >= datetime(?)
       AND datetime(committed_at) <= datetime(?)
     GROUP BY author_email, author_name`,
  );
  const update = db.prepare(
    `UPDATE sessions SET engineer_email = ?, engineer_name = ? WHERE id = ?`,
  );

  let touched = 0;
  for (const s of sessions) {
    // Skip zero-width windows — single-event sessions with no Stop event give
    // us no temporal signal, so any commit match would be coincidence.
    if (!s.end_ts || s.end_ts === s.started_at) continue;
    const authors = findAuthors.all(s.repo_id, s.started_at, s.end_ts) as Array<{
      author_email: string;
      author_name: string | null;
      n: number;
    }>;
    if (authors.length !== 1) continue;
    const author = authors[0]!;
    update.run(author.author_email, author.author_name, s.id);
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
  backfillSessionRepos(db);
  backfillSessionModels(db);
  backfillSessionEngineers(db);
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
