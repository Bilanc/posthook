package store

// LocalOrgID is the org_id used in the local-only mode. When the same
// schema migrates to Postgres for the SaaS, the column accepts real org
// UUIDs without a schema change.
const LocalOrgID = "local"

const schemaVersion = 7

// SyncableTables lists every table the cloud sync flush replicates upstream,
// in FK-safe insert order. Keep this in lockstep with the synced_at columns
// declared in schemaSQL and the v7 migration in applyMigrations.
var SyncableTables = []string{
	"repositories",
	"sessions",
	"events",
	"commits",
	"commit_files",
	"commit_sessions",
	"commit_session_files",
	"event_line_ranges",
}

const schemaSQL = `
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
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  synced_at TEXT
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
  engineer_name TEXT,
  synced_at TEXT
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
  payload TEXT,
  synced_at TEXT
);
CREATE INDEX IF NOT EXISTS idx_events_org_ts ON events(org_id, ts);
CREATE INDEX IF NOT EXISTS idx_events_session ON events(session_id);
CREATE INDEX IF NOT EXISTS idx_events_type ON events(event_type);
CREATE INDEX IF NOT EXISTS idx_events_repo ON events(repo_id);

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
  synced_at TEXT,
  UNIQUE(repo_id, sha)
);
CREATE INDEX IF NOT EXISTS idx_commits_org_committed ON commits(org_id, committed_at);
CREATE INDEX IF NOT EXISTS idx_commits_repo ON commits(repo_id);

CREATE TABLE IF NOT EXISTS commit_files (
  commit_id TEXT NOT NULL REFERENCES commits(id),
  file_path TEXT NOT NULL,
  lines_added INTEGER NOT NULL DEFAULT 0,
  lines_removed INTEGER NOT NULL DEFAULT 0,
  synced_at TEXT,
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
  synced_at TEXT,
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
  synced_at TEXT,
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
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  synced_at TEXT
);
CREATE INDEX IF NOT EXISTS idx_elr_event ON event_line_ranges(event_id);
CREATE INDEX IF NOT EXISTS idx_elr_relpath ON event_line_ranges(rel_file_path);

CREATE TABLE IF NOT EXISTS sync_state (
  table_name      TEXT PRIMARY KEY,
  last_attempt_at TEXT,
  last_success_at TEXT,
  last_error      TEXT,
  last_row_count  INTEGER
);
`

// attributedRangesCTE is the workhorse query for joining commits to AI line
// ranges. The boundary for attribution is the next commit that touches the
// same file (NOT the next repo commit), so back-to-back commits on different
// files don't steal attribution from each other.
const attributedRangesCTE = `
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
`
