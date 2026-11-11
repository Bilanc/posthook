import { db } from "../db.ts";
import { AI_EDIT_EVENT_CONDITION, LINES_GENERATED, LINES_WRITTEN } from "./edit-condition.ts";
import { IDLE_GAP_MINUTES } from "./activity.ts";
import { ATTRIBUTED } from "./outcomes.ts";

// Everything the session page needs, in time order. The grouping into turns
// happens in ../turns.ts so it can be unit-tested without a database.

export interface StopEvent {
  ts: string;
  last_assistant_message: string | null;
  effort: string | null;
  permission_mode: string | null;
}

export interface PromptEntry {
  ts: string | null;
  seq: number;
  text: string;
}

export interface ToolCall {
  id: string;
  ts: string;
  tool_name: string;
  duration_ms: number | null;
  file: string | null; // repo-relative path for edits (line ranges first)
  lines_generated: number;
  summary: string | null; // command / description / pattern, first 200 chars
  has_stderr: number; // 1 when the shell call wrote to stderr
  ranged: number; // 1 when posthook located written lines in the file (shell-written edits)
}

export interface SessionCommit {
  id: string;
  sha: string;
  committed_at: string;
  message: string | null;
  lines_added: number;
  lines_removed: number;
  files_changed: number;
  lines_attributed: number;
  files_touched: number;
}

// repo_id / repo_name say which repository a file is in. One session can
// write files in several, and a repo-relative path alone (README.md) does not
// say which. Both are null for a file outside any repository.
// repo_root is the repository's folder on disk: two checkouts of one
// repository (a worktree beside the main clone) share a name, not a root.
export interface CommitFile {
  commit_id: string;
  file_path: string;
  lines_attributed: number;
  repo_id?: string | null;
  repo_name?: string | null;
  repo_root?: string | null;
}

export interface SessionFile {
  file: string;
  edits: number;
  lines_generated: number;
  repo_id?: string | null;
  repo_name?: string | null;
  repo_root?: string | null;
}

export interface SessionTimelineData {
  stops: StopEvent[];
  prompts: PromptEntry[];
  tools: ToolCall[];
  commits: SessionCommit[];
  commit_files: CommitFile[];
  files: SessionFile[];
  active_minutes: number;
  transcript_path: string | null;
}

const V = "json_valid(e.payload)";
// Repo-relative path first; shell-written edits only carry a path on their
// line ranges, and the oldest events only an absolute path.
const FILE_EXPR = `COALESCE(
  e.rel_file_path,
  (SELECT x.rel_file_path FROM event_line_ranges x WHERE x.event_id = e.id AND x.rel_file_path IS NOT NULL LIMIT 1),
  (SELECT x.file_path FROM event_line_ranges x WHERE x.event_id = e.id LIMIT 1),
  e.file_path
)`;

export function sessionTimelineData(sessionId: string): SessionTimelineData {
  const conn = db();
  const stops = conn
    .prepare(
      `SELECT e.ts,
              json_extract(e.payload, '$.last_assistant_message') AS last_assistant_message,
              json_extract(e.payload, '$.effort.level') AS effort,
              json_extract(e.payload, '$.permission_mode') AS permission_mode
       FROM events e
       WHERE e.session_id = ? AND e.event_type = 'Stop' AND ${V}
       ORDER BY e.ts`,
    )
    .all(sessionId) as StopEvent[];

  const prompts = conn
    .prepare(
      `SELECT ts, seq, prompt_text AS text FROM session_prompts WHERE session_id = ? ORDER BY seq`,
    )
    .all(sessionId) as PromptEntry[];

  const tools = conn
    .prepare(
      `SELECT e.id, e.ts,
              json_extract(e.payload, '$.tool_name') AS tool_name,
              COALESCE(json_extract(e.payload, '$.duration_ms'), json_extract(e.payload, '$.duration')) AS duration_ms,
              ${FILE_EXPR} AS file,
              CASE WHEN ${AI_EDIT_EVENT_CONDITION} THEN ${LINES_WRITTEN} ELSE 0 END AS lines_generated,
              substr(COALESCE(json_extract(e.payload, '$.tool_input.command'),
                              json_extract(e.payload, '$.tool_input.description'),
                              json_extract(e.payload, '$.tool_input.pattern'),
                              json_extract(e.payload, '$.tool_input.prompt'),
                              ${FILE_EXPR}), 1, 200) AS summary,
              (length(COALESCE(json_extract(e.payload, '$.tool_response.stderr'), '')) > 0) AS has_stderr,
              EXISTS (SELECT 1 FROM event_line_ranges x WHERE x.event_id = e.id) AS ranged
       FROM events e
       WHERE e.session_id = ? AND e.event_type IN ('PostToolUse', 'postToolUse', 'afterFileEdit') AND ${V}
       ORDER BY e.ts`,
    )
    .all(sessionId) as ToolCall[];

  const commits = conn
    .prepare(
      `SELECT c.id, c.sha, c.committed_at, c.message, c.lines_added, c.lines_removed, c.files_changed,
              cs.lines_attributed, cs.files_touched
       FROM commit_sessions cs JOIN commits c ON c.id = cs.commit_id
       WHERE cs.session_id = ? AND ${ATTRIBUTED}
       ORDER BY datetime(c.committed_at)`,
    )
    .all(sessionId) as SessionCommit[];

  const commit_files = conn
    .prepare(
      `SELECT csf.commit_id, csf.file_path, csf.lines_attributed, c.repo_id,
              r.name AS repo_name, r.root_path AS repo_root
       FROM commit_session_files csf
       JOIN commits c ON c.id = csf.commit_id
       LEFT JOIN repositories r ON r.id = c.repo_id
       WHERE csf.session_id = ?`,
    )
    .all(sessionId) as CommitFile[];

  const files = conn
    .prepare(
      // One row per file an edit touched, keyed by repository and path.
      //  - An edit that carries its own split (events.file_lines) gives one
      //    row per file in it: that file's lines less that file's rework, in
      //    that file's repository. So a patch that adds a line in one file
      //    and rewrites the session's work in another is not netted on the
      //    first, and one command that writes README.md in two repositories
      //    gives two rows.
      //  - An edit the client measured that carries no split is one row, on
      //    the event's own file, with its whole count. That is the client's
      //    contract: it writes a split whenever the event's own file does not
      //    have all of it. (A build from before the split existed could
      //    measure a multi-file patch without one; it shows under its first
      //    file. Its ranges are not its exact counts and its rework has no
      //    file, so no split of it would be right.)
      //  - An edit from a client that does not measure is split by each line
      //    range's own path when its ranges sit in several files, since its
      //    row names only the first. Otherwise it goes to the event's file.
      // The event's file is the path its row names, or its range's path when
      // the row has none. Without a split the repository is the event's own,
      // and only for a file with a repo-relative path: events.repo_id falls
      // back to the session's folder for a file that is in no repository,
      // and that file must not be shown under it.
      `WITH edit_events AS (
         SELECT e.id,
                e.repo_id,
                e.lines_generated AS measured,
                CASE WHEN json_valid(e.file_lines) THEN e.file_lines END AS file_lines,
                ${FILE_EXPR} AS event_file,
                COALESCE(e.rel_file_path,
                         (SELECT x.rel_file_path FROM event_line_ranges x
                          WHERE x.event_id = e.id AND x.rel_file_path IS NOT NULL LIMIT 1)) AS event_rel,
                ${LINES_GENERATED} AS lines,
                (SELECT COUNT(DISTINCT COALESCE(x.rel_file_path, x.file_path))
                 FROM event_line_ranges x WHERE x.event_id = e.id) AS range_files
         FROM events e
         WHERE e.session_id = ? AND ${AI_EDIT_EVENT_CONDITION}
       ),
       per_event_file AS (
         -- A split written before elements carried repo_id has no such key:
         -- fall back to the event's repository. A key that is there and null
         -- means the file is in no repository.
         SELECT CASE WHEN json_type(j.value, '$.repo_id') IS NULL THEN ee.repo_id
                     ELSE json_extract(j.value, '$.repo_id') END AS repo_id,
                COALESCE(json_extract(j.value, '$.rel_file_path'), json_extract(j.value, '$.file_path')) AS f,
                COALESCE(json_extract(j.value, '$.lines_generated'), 0)
                  - COALESCE(json_extract(j.value, '$.lines_reworked'), 0) AS lines
         FROM edit_events ee, json_each(ee.file_lines) j
         WHERE ee.file_lines IS NOT NULL
         UNION ALL
         SELECT MAX(CASE WHEN x.rel_file_path IS NOT NULL THEN ee.repo_id END) AS repo_id,
                COALESCE(x.rel_file_path, x.file_path) AS f,
                SUM(x.new_text_lines) AS lines
         FROM edit_events ee JOIN event_line_ranges x ON x.event_id = ee.id
         WHERE ee.file_lines IS NULL AND ee.measured IS NULL AND ee.range_files > 1
         GROUP BY ee.id, COALESCE(x.rel_file_path, x.file_path)
         UNION ALL
         SELECT CASE WHEN event_rel IS NOT NULL THEN repo_id END AS repo_id,
                event_file AS f,
                lines
         FROM edit_events
         WHERE file_lines IS NULL AND (measured IS NOT NULL OR range_files <= 1)
       )
       SELECT pf.f AS file, pf.repo_id, r.name AS repo_name, r.root_path AS repo_root,
              COUNT(*) AS edits, MAX(COALESCE(SUM(pf.lines), 0), 0) AS lines_generated
       FROM per_event_file pf
       LEFT JOIN repositories r ON r.id = pf.repo_id
       WHERE pf.f IS NOT NULL
       GROUP BY pf.repo_id, pf.f
       ORDER BY lines_generated DESC, edits DESC`,
    )
    .all(sessionId) as SessionFile[];

  const active = conn
    .prepare(
      `SELECT COALESCE(SUM(MIN((julianday(ts) - julianday(prev)) * 1440.0, ?)), 0) AS m
       FROM (SELECT ts, LAG(ts) OVER (ORDER BY ts) AS prev FROM events WHERE session_id = ?)
       WHERE prev IS NOT NULL`,
    )
    .get(IDLE_GAP_MINUTES, sessionId) as { m: number };

  const transcript = conn
    .prepare(
      `SELECT json_extract(e.payload, '$.transcript_path') AS path
       FROM events e
       WHERE e.session_id = ? AND ${V} AND json_extract(e.payload, '$.transcript_path') IS NOT NULL
       ORDER BY e.ts DESC LIMIT 1`,
    )
    .get(sessionId) as { path: string | null } | undefined;

  return {
    stops,
    prompts,
    tools,
    commits,
    commit_files,
    files,
    active_minutes: active.m,
    transcript_path: transcript?.path ?? null,
  };
}
