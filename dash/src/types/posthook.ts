// Row shapes mirror posthook's SQLite schema (posthook/src/store.ts).
// Keep nullability matching the schema — every column documented here.

export interface SessionRow {
  id: string;
  org_id: string;
  agent_slug: string;
  model_slug: string | null;
  repo_id: string | null;
  branch: string | null;
  cwd: string | null;
  started_at: string;
  ended_at: string | null;
  engineer_email: string | null;
  engineer_name: string | null;
}

export interface RepositoryRow {
  id: string;
  org_id: string;
  remote_url: string | null;
  name: string;
  root_path: string;
  created_at: string;
}

export interface CommitRow {
  id: string;
  org_id: string;
  repo_id: string;
  sha: string;
  parent_sha: string | null;
  author_email: string | null;
  author_name: string | null;
  committed_at: string;
  branch: string | null;
  message: string | null;
  lines_added: number;
  lines_removed: number;
  files_changed: number;
}

export interface BreakdownRow {
  key: string;
  display_name: string | null;
  edits: number;
  lines_generated: number;
  lines_committed: number;
  sessions: number;
  top_model: string | null;
}

export interface OverviewSummary {
  edits: number;
  lines_generated: number;
  lines_replaced: number;
  lines_committed: number;
  sessions: number;
  working_hours: number;
  max_concurrent: number;
  top_model: string | null;
  top_model_edits: number;
  commit_count: number;
  commit_lines_added: number;
  commit_lines_removed: number;
  ai_code_pct: number;
}
