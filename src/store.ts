import { Database } from "bun:sqlite";
import { existsSync, mkdirSync, readFileSync } from "node:fs";
import { createHash, randomUUID } from "node:crypto";
import { basename, isAbsolute, resolve } from "node:path";
import { execSync } from "node:child_process";
import { DB_PATH, POSTHOOK_DIR } from "./config.ts";
import { extractRanges, parseApplyPatch, type ApplyPatchFileEdit } from "./lineRanges.ts";
import { parseClaudeTranscriptSync } from "./transcript.ts";
import { canonicalize, findRepoRoot, gitBypassEnv, relPathInRepo } from "./util/git.ts";

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

CREATE TABLE IF NOT EXISTS commit_sessions (
  commit_id TEXT NOT NULL REFERENCES commits(id),
  session_id TEXT NOT NULL REFERENCES sessions(id),
  agent_slug TEXT NOT NULL,
  model_slug TEXT,
  first_event_ts TEXT NOT NULL,
  last_event_ts TEXT NOT NULL,
  event_count INTEGER NOT NULL DEFAULT 0,
  files_touched INTEGER NOT NULL DEFAULT 0,
  lines_attributed INTEGER NOT NULL DEFAULT 0,
  attribution_source TEXT NOT NULL DEFAULT 'event_line_ranges',
  confidence TEXT NOT NULL DEFAULT 'line_range_next_file_commit',
  PRIMARY KEY (commit_id, session_id)
);
CREATE INDEX IF NOT EXISTS idx_commit_sessions_session ON commit_sessions(session_id);
CREATE INDEX IF NOT EXISTS idx_commit_sessions_commit ON commit_sessions(commit_id);

CREATE TABLE IF NOT EXISTS commit_session_files (
  commit_id TEXT NOT NULL REFERENCES commits(id),
  session_id TEXT NOT NULL REFERENCES sessions(id),
  file_path TEXT NOT NULL,
  first_event_ts TEXT NOT NULL,
  last_event_ts TEXT NOT NULL,
  event_count INTEGER NOT NULL DEFAULT 0,
  lines_attributed INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (commit_id, session_id, file_path)
);
CREATE INDEX IF NOT EXISTS idx_commit_session_files_session ON commit_session_files(session_id);
CREATE INDEX IF NOT EXISTS idx_commit_session_files_commit ON commit_session_files(commit_id);

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

const VERSION = 6;

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

const ATTRIBUTED_RANGES_CTE = `
WITH attributed_ranges AS (
  SELECT
    c.id AS commit_id,
    cf.file_path AS rel_file_path,
    e.id AS event_id,
    e.session_id,
    e.agent_slug,
    COALESCE(s.model_slug, json_extract(e.payload, '$.model')) AS model_slug,
    e.ts AS event_ts,
    elr.new_text_lines
  FROM commits c
  JOIN commit_files cf ON cf.commit_id = c.id
  JOIN event_line_ranges elr ON elr.rel_file_path = cf.file_path
  JOIN events e ON e.id = elr.event_id
  JOIN sessions s ON s.id = e.session_id
  WHERE e.repo_id = c.repo_id
    AND e.session_id IS NOT NULL
    AND elr.rel_file_path IS NOT NULL
    AND datetime(c.committed_at) >= datetime(e.ts)
    AND (? IS NULL OR c.id = ?)
    AND NOT EXISTS (
      SELECT 1
      FROM commits c2
      JOIN commit_files cf2 ON cf2.commit_id = c2.id
      WHERE c2.repo_id = c.repo_id
        AND cf2.file_path = cf.file_path
        AND datetime(c2.committed_at) >= datetime(e.ts)
        AND datetime(c2.committed_at) < datetime(c.committed_at)
    )
)
`;

export function refreshCommitAttributions(db: Database, commitId?: string): number {
  if (commitId) {
    db.run("DELETE FROM commit_sessions WHERE commit_id = ?", [commitId]);
    db.run("DELETE FROM commit_session_files WHERE commit_id = ?", [commitId]);
  } else {
    db.run("DELETE FROM commit_sessions");
    db.run("DELETE FROM commit_session_files");
  }

  const commitFilter = commitId ?? null;
  const insertFiles = db.prepare(
    `${ATTRIBUTED_RANGES_CTE}
     INSERT INTO commit_session_files (
       commit_id, session_id, file_path, first_event_ts, last_event_ts,
       event_count, lines_attributed
     )
     SELECT
       commit_id,
       session_id,
       rel_file_path,
       MIN(event_ts),
       MAX(event_ts),
       COUNT(DISTINCT event_id),
       COALESCE(SUM(new_text_lines), 0)
     FROM attributed_ranges
     GROUP BY commit_id, session_id, rel_file_path`,
  );
  insertFiles.run(commitFilter, commitFilter);

  const insertSessions = db.prepare(
    `INSERT INTO commit_sessions (
       commit_id, session_id, agent_slug, model_slug, first_event_ts, last_event_ts,
       event_count, files_touched, lines_attributed, attribution_source, confidence
     )
     SELECT
       csf.commit_id,
       csf.session_id,
       COALESCE(s.agent_slug, (
         SELECT e.agent_slug
         FROM events e
         WHERE e.session_id = csf.session_id
         ORDER BY datetime(e.ts) DESC
         LIMIT 1
       ), 'unknown') AS agent_slug,
       COALESCE(s.model_slug, (
         SELECT json_extract(e.payload, '$.model')
         FROM events e
         WHERE e.session_id = csf.session_id
           AND json_extract(e.payload, '$.model') IS NOT NULL
         ORDER BY datetime(e.ts) DESC
         LIMIT 1
       )) AS model_slug,
       MIN(csf.first_event_ts),
       MAX(csf.last_event_ts),
       SUM(csf.event_count),
       COUNT(DISTINCT csf.file_path),
       COALESCE(SUM(csf.lines_attributed), 0),
       'event_line_ranges',
       'line_range_next_file_commit'
     FROM commit_session_files csf
     LEFT JOIN sessions s ON s.id = csf.session_id
     WHERE (? IS NULL OR csf.commit_id = ?)
     GROUP BY csf.commit_id, csf.session_id`,
  );
  insertSessions.run(commitFilter, commitFilter);

  const row = db
    .query("SELECT COUNT(*) AS n FROM commit_sessions WHERE (? IS NULL OR commit_id = ?)")
    .get(commitFilter, commitFilter) as { n: number };
  return row.n;
}

function normalizeEventTypes(db: Database): void {
  // Cursor hook names are camel-case (`postToolUse`), while Claude/Codex use
  // `PostToolUse`. Internally we use the canonical Claude/Codex casing so
  // cross-agent metrics and line-range capture share one predicate.
  db.run("UPDATE events SET event_type = 'PreToolUse' WHERE event_type = 'preToolUse'");
  db.run("UPDATE events SET event_type = 'PostToolUse' WHERE event_type = 'postToolUse'");
}

function backfillEventSessions(db: Database): number {
  const insertSession = db.prepare(
    `INSERT INTO sessions (id, org_id, agent_slug, model_slug, repo_id, cwd, started_at, ended_at)
     VALUES (?, 'local', ?, ?, ?, ?, ?, ?)
     ON CONFLICT(id) DO UPDATE SET
       model_slug = COALESCE(sessions.model_slug, excluded.model_slug),
       repo_id = COALESCE(sessions.repo_id, excluded.repo_id),
       cwd = COALESCE(sessions.cwd, excluded.cwd),
       started_at = CASE
         WHEN datetime(excluded.started_at) < datetime(sessions.started_at)
           THEN excluded.started_at
         ELSE sessions.started_at
       END,
       ended_at = CASE
         WHEN sessions.ended_at IS NULL THEN excluded.ended_at
         WHEN excluded.ended_at IS NULL THEN sessions.ended_at
         WHEN datetime(excluded.ended_at) > datetime(sessions.ended_at)
           THEN excluded.ended_at
         ELSE sessions.ended_at
       END`,
  );

  const groups = db
    .query(
      `SELECT
         e.agent_slug,
         e.session_id AS raw_session_id,
         CASE
           WHEN s.id IS NULL THEN e.session_id
           ELSE e.agent_slug || ':' || e.session_id
         END AS session_id,
         MIN(e.ts) AS first_ts,
         MAX(e.ts) AS last_ts,
         (
           SELECT json_extract(e2.payload, '$.model')
           FROM events e2
           WHERE e2.agent_slug = e.agent_slug
             AND e2.session_id = e.session_id
             AND json_extract(e2.payload, '$.model') IS NOT NULL
           ORDER BY datetime(e2.ts) DESC
           LIMIT 1
         ) AS model_slug,
         (
           SELECT e2.repo_id
           FROM events e2
           WHERE e2.agent_slug = e.agent_slug
             AND e2.session_id = e.session_id
             AND e2.repo_id IS NOT NULL
           ORDER BY datetime(e2.ts) DESC
           LIMIT 1
         ) AS repo_id,
         (
           SELECT e2.cwd
           FROM events e2
           WHERE e2.agent_slug = e.agent_slug
             AND e2.session_id = e.session_id
             AND e2.cwd IS NOT NULL
           ORDER BY datetime(e2.ts) DESC
           LIMIT 1
         ) AS cwd
       FROM events e
       LEFT JOIN sessions s ON s.id = e.session_id
       WHERE e.session_id IS NOT NULL
         AND (s.id IS NULL OR s.agent_slug != e.agent_slug)
       GROUP BY e.agent_slug, e.session_id, s.id`,
    )
    .all() as Array<{
    agent_slug: string;
    raw_session_id: string;
    session_id: string;
    first_ts: string;
    last_ts: string;
    model_slug: string | null;
    repo_id: string | null;
    cwd: string | null;
  }>;

  let touched = 0;
  for (const group of groups) {
    insertSession.run(
      group.session_id,
      group.agent_slug,
      group.model_slug,
      group.repo_id,
      group.cwd,
      group.first_ts,
      group.last_ts,
    );

    if (group.session_id !== group.raw_session_id) {
      db.run(
        `UPDATE commit_session_files
         SET session_id = ?
         WHERE session_id = ?
           AND EXISTS (
             SELECT 1
             FROM commit_sessions cs
             WHERE cs.commit_id = commit_session_files.commit_id
               AND cs.session_id = commit_session_files.session_id
               AND cs.agent_slug = ?
           )`,
        [group.session_id, group.raw_session_id, group.agent_slug],
      );
      db.run(
        `UPDATE commit_sessions
         SET session_id = ?
         WHERE session_id = ?
           AND agent_slug = ?`,
        [group.session_id, group.raw_session_id, group.agent_slug],
      );
      db.run(
        `UPDATE events
         SET session_id = ?
         WHERE agent_slug = ?
           AND session_id = ?`,
        [group.session_id, group.agent_slug, group.raw_session_id],
      );
    }
    touched++;
  }

  return touched;
}

// Walk events with NULL repo_id, resolve their repo from file/workspace/cwd, and populate.
// Cheap to run on each open: we only touch rows that haven't been backfilled yet.
function backfillEventRepos(db: Database): number {
  const rows = db
    .query(
      `SELECT id, cwd, file_path, payload
       FROM events
       WHERE repo_id IS NULL
         AND (
           (cwd IS NOT NULL AND cwd != '')
           OR (file_path IS NOT NULL AND file_path != '')
         )`,
    )
    .all() as Array<{
    id: string;
    cwd: string | null;
    file_path: string | null;
    payload: string | null;
  }>;
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
  // Cache repo lookups so we don't fs-walk for every event in the same project.
  const rootToRepo = new Map<string, { id: string; root: string }>();
  for (const r of rows) {
    const workspaceRoots = extractWorkspaceRoots(r.payload);
    const applyPatchFiles = applyPatchFilesForPayload(r.payload);
    const filePath = r.file_path ?? applyPatchFiles[0]?.file_path ?? null;
    const eventFilePath = filePath
      ? canonicalize(resolveEventFilePath(filePath, r.cwd, workspaceRoots))
      : null;
    const root = findEventRepoRoot(r.cwd, eventFilePath, workspaceRoots);
    if (!root) continue;

    let resolved = rootToRepo.get(root);
    if (resolved === undefined) {
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
      rootToRepo.set(root, resolved);
    }
    const rel = eventFilePath ? relPathInRepo(resolved.root, eventFilePath) : null;
    update.run(resolved.id, rel ?? null, r.id);
    touched++;
  }
  return touched;
}

function backfillLineRangeRelPaths(db: Database): number {
  const changes = db.run(
    `UPDATE event_line_ranges
     SET rel_file_path = (
       SELECT e.rel_file_path
       FROM events e
       WHERE e.id = event_line_ranges.event_id
     )
     WHERE rel_file_path IS NULL
       AND EXISTS (
         SELECT 1
         FROM events e
         WHERE e.id = event_line_ranges.event_id
           AND e.rel_file_path IS NOT NULL
       )`,
  );
  return changes.changes;
}

function backfillAfterFileEditLineRanges(db: Database): number {
  const rows = db
    .query(
      `SELECT e.id, e.file_path, e.rel_file_path, e.payload
       FROM events e
       WHERE e.event_type = 'afterFileEdit'
         AND e.file_path IS NOT NULL
         AND e.payload IS NOT NULL
         AND NOT EXISTS (
           SELECT 1 FROM event_line_ranges elr
           WHERE elr.event_id = e.id
         )`,
    )
    .all() as Array<{
    id: string;
    file_path: string;
    rel_file_path: string | null;
    payload: string;
  }>;
  if (rows.length === 0) return 0;

  const insert = db.prepare(
    `INSERT INTO event_line_ranges (
      id, event_id, file_path, rel_file_path, blob_sha_after,
      start_line, end_line, new_text_lines
    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
  );

  let inserted = 0;
  for (const row of rows) {
    if (!existsSync(row.file_path)) continue;

    let payload: Record<string, unknown>;
    try {
      payload = JSON.parse(row.payload) as Record<string, unknown>;
    } catch {
      continue;
    }

    const toolInput = lineRangeToolInput(payload);
    if (!toolInput) continue;

    const content = readFileSync(row.file_path, "utf8");
    const { ranges } = extractRanges("MultiEdit", toolInput, content);
    if (ranges.length === 0) continue;

    const sha = createHash("sha256").update(content).digest("hex");
    for (const range of ranges) {
      insert.run(
        randomUUID(),
        row.id,
        row.file_path,
        row.rel_file_path,
        sha,
        range.start_line,
        range.end_line,
        range.new_text_lines,
      );
      inserted++;
    }
  }

  return inserted;
}

function backfillApplyPatchLineRanges(db: Database): number {
  const rows = db
    .query(
      `SELECT e.id, e.cwd, e.payload
       FROM events e
       WHERE e.event_type = 'PostToolUse'
         AND e.payload IS NOT NULL
         AND json_extract(e.payload, '$.tool_name') = 'apply_patch'
         AND NOT EXISTS (
           SELECT 1 FROM event_line_ranges elr
           WHERE elr.event_id = e.id
         )`,
    )
    .all() as Array<{ id: string; cwd: string | null; payload: string }>;
  if (rows.length === 0) return 0;

  const insert = db.prepare(
    `INSERT INTO event_line_ranges (
      id, event_id, file_path, rel_file_path, blob_sha_after,
      start_line, end_line, new_text_lines
    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
  );
  const updateEvent = db.prepare(
    `UPDATE events
     SET file_path = COALESCE(file_path, ?),
         repo_id = COALESCE(repo_id, ?),
         rel_file_path = COALESCE(rel_file_path, ?),
         lines_added = COALESCE(lines_added, ?),
         lines_removed = COALESCE(lines_removed, ?)
     WHERE id = ?`,
  );

  let inserted = 0;
  for (const row of rows) {
    const files = applyPatchFilesForPayload(row.payload);
    if (files.length === 0) continue;

    const workspaceRoots = extractWorkspaceRoots(row.payload);
    const linesAdded = files.reduce((sum, file) => sum + file.lines_added, 0);
    const linesRemoved = files.reduce((sum, file) => sum + file.lines_removed, 0);
    let firstFilePath: string | null = null;
    let firstRepoId: string | null = null;
    let firstRelFilePath: string | null = null;

    for (const file of files) {
      if (file.edits.length === 0) continue;

      const resolvedFilePath = canonicalize(
        resolveEventFilePath(file.file_path, row.cwd, workspaceRoots),
      );
      if (!existsSync(resolvedFilePath)) continue;

      const repoRoot = findEventRepoRoot(row.cwd, resolvedFilePath, workspaceRoots);
      const relFilePath = repoRoot ? relPathInRepo(repoRoot, resolvedFilePath) : null;
      if (!firstFilePath) {
        firstFilePath = resolvedFilePath;
        firstRelFilePath = relFilePath;
        firstRepoId = repoRoot ? upsertRepositoryByRoot(db, repoRoot) : null;
      }

      const content = readFileSync(resolvedFilePath, "utf8");
      const { ranges } = extractRanges("MultiEdit", { edits: file.edits }, content);
      if (ranges.length === 0) continue;

      const sha = createHash("sha256").update(content).digest("hex");
      for (const range of ranges) {
        insert.run(
          randomUUID(),
          row.id,
          resolvedFilePath,
          relFilePath,
          sha,
          range.start_line,
          range.end_line,
          range.new_text_lines,
        );
        inserted++;
      }
    }

    updateEvent.run(
      firstFilePath,
      firstRepoId,
      firstRelFilePath,
      linesAdded,
      linesRemoved,
      row.id,
    );
  }

  return inserted;
}

function deleteDuplicateCursorPostToolUseEditRanges(db: Database): number {
  const changes = db.run(
    `DELETE FROM event_line_ranges
     WHERE event_id IN (
       SELECT p.id
       FROM events p
       WHERE p.agent_slug = 'cursor'
         AND p.event_type = 'PostToolUse'
         AND json_extract(p.payload, '$.tool_name') IN ('Edit', 'Write', 'MultiEdit')
         AND p.file_path IS NOT NULL
         AND EXISTS (
           SELECT 1
           FROM events afe
           WHERE afe.agent_slug = 'cursor'
             AND afe.event_type = 'afterFileEdit'
             AND afe.file_path = p.file_path
             AND (
               afe.session_id = p.session_id
               OR (afe.session_id IS NULL AND p.session_id IS NULL)
             )
             AND ABS((julianday(afe.ts) - julianday(p.ts)) * 86400.0) <= 5
         )
     )`,
  );
  return changes.changes;
}

function lineRangeToolInput(
  payload: Record<string, unknown>,
): Parameters<typeof extractRanges>[1] | null {
  const synthesized: Record<string, unknown> = {};
  for (const key of ["file_path", "old_string", "new_string", "content"] as const) {
    const value = payload[key];
    if (typeof value === "string") synthesized[key] = value;
  }
  if (Array.isArray(payload.edits)) synthesized.edits = payload.edits;
  return Object.keys(synthesized).length > 0
    ? (synthesized as Parameters<typeof extractRanges>[1])
    : null;
}

function applyPatchFilesForPayload(rawPayload: string | null): ApplyPatchFileEdit[] {
  if (!rawPayload) return [];
  try {
    const payload = JSON.parse(rawPayload) as Record<string, unknown>;
    if (payload.tool_name !== "apply_patch") return [];
    const toolInput = payload.tool_input;
    if (!toolInput || typeof toolInput !== "object" || Array.isArray(toolInput)) return [];
    const command = (toolInput as Record<string, unknown>).command;
    return typeof command === "string" ? parseApplyPatch(command) : [];
  } catch {
    return [];
  }
}

function extractWorkspaceRoots(rawPayload: string | null): string[] {
  if (!rawPayload) return [];
  try {
    const payload = JSON.parse(rawPayload) as Record<string, unknown>;
    const roots = payload.workspace_roots;
    if (!Array.isArray(roots)) return [];
    return roots.filter((root): root is string => typeof root === "string" && root.length > 0);
  } catch {
    return [];
  }
}

function resolveEventFilePath(
  filePath: string,
  cwd: string | null,
  workspaceRoots: string[],
): string {
  if (isAbsolute(filePath)) return filePath;

  const cwdRepo = cwd ? findRepoRoot(cwd) : null;
  const bases =
    cwdRepo && cwd ? [cwd, ...workspaceRoots] : [...workspaceRoots, ...(cwd ? [cwd] : [])];
  const candidates = bases.map((base) => resolve(base, filePath));
  return candidates[0] ?? filePath;
}

function findEventRepoRoot(
  cwd: string | null,
  canonicalFilePath: string | null,
  workspaceRoots: string[],
): string | null {
  const fileRepo = canonicalFilePath ? findRepoRoot(canonicalFilePath) : null;
  if (fileRepo) return fileRepo;

  for (const root of workspaceRoots) {
    const workspaceRepo = findRepoRoot(root);
    if (workspaceRepo) return workspaceRepo;
  }

  return cwd ? findRepoRoot(cwd) : null;
}

function upsertRepositoryByRoot(db: Database, root: string): string {
  const existing = db
    .query("SELECT id FROM repositories WHERE root_path = ?")
    .get(root) as { id: string } | undefined;
  if (existing) return existing.id;

  const id = randomUUID();
  let remoteUrl: string | null = null;
  try {
    remoteUrl = execSync(`git -C "${root}" config --get remote.origin.url`, {
      encoding: "utf8",
      env: gitBypassEnv(),
    }).trim();
  } catch {
    // no remote
  }
  db.run(
    `INSERT INTO repositories (id, org_id, remote_url, name, root_path)
     VALUES (?, 'local', ?, ?, ?)
     ON CONFLICT(root_path) DO NOTHING`,
    [id, remoteUrl, basename(root), root],
  );
  return id;
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
  const row = db.query("SELECT MAX(version) AS v FROM schema_version").get() as { v: number | null };
  const currentVersion = row?.v ?? 0;
  normalizeEventTypes(db);
  const eventSessionsBackfilled = backfillEventSessions(db);
  const eventReposBackfilled = backfillEventRepos(db);
  const lineRangeRelPathsBackfilled = backfillLineRangeRelPaths(db);
  const afterFileEditRangesBackfilled = backfillAfterFileEditLineRanges(db);
  const applyPatchRangesBackfilled = backfillApplyPatchLineRanges(db);
  const duplicateCursorRangesDeleted = deleteDuplicateCursorPostToolUseEditRanges(db);
  backfillSessionRepos(db);
  const sessionModelsBackfilled = backfillSessionModels(db);
  backfillSessionEngineers(db);
  if (
    currentVersion < VERSION ||
    eventSessionsBackfilled > 0 ||
    eventReposBackfilled > 0 ||
    lineRangeRelPathsBackfilled > 0 ||
    afterFileEditRangesBackfilled > 0 ||
    applyPatchRangesBackfilled > 0 ||
    duplicateCursorRangesDeleted > 0 ||
    sessionModelsBackfilled > 0
  ) {
    refreshCommitAttributions(db);
  }
  if (currentVersion < VERSION) {
    db.run("INSERT INTO schema_version (version) VALUES (?)", [VERSION]);
  }
  cachedDb = db;
  return db;
}

export function closeDb(): void {
  cachedDb?.close();
  cachedDb = null;
}
