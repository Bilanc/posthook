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
      //  - Without a split, an edit whose line ranges sit in several files
      //    is split by each range's own path. Its row names only the first.
      //    If the client measured it, its files must still add up to its
      //    count, and without a split nothing says which file the difference
      //    belongs to. Lines it wrote that have no range go to the file the
      //    event names. Rework (a negative difference) comes off the file
      //    with the most located lines, the likeliest place for it and the
      //    one it cannot erase. Only builds from before the split existed
      //    wrote such rows.
      //  - Otherwise the whole count goes to one file. For an edit the
      //    client measured that is the file its range sits in, when it has
      //    one: a shell command's row names its first target, which need not
      //    be the file it measurably wrote. Else the file the event names.
      // Without a split the repository is the event's own, and only for a
      // file with a repo-relative path: events.repo_id falls back to the
      // session's folder for a file that is in no repository, and that file
      // must not be shown under it.
      `WITH edit_events AS (
         SELECT e.id,
                e.repo_id,
                e.rel_file_path AS event_rel,
                e.lines_generated AS measured,
                CASE WHEN json_valid(e.file_lines) THEN e.file_lines END AS file_lines,
                ${FILE_EXPR} AS event_file,
                (SELECT COALESCE(x.rel_file_path, x.file_path)
                 FROM event_line_ranges x WHERE x.event_id = e.id LIMIT 1) AS range_file,
                (SELECT x.rel_file_path FROM event_line_ranges x
                 WHERE x.event_id = e.id AND x.rel_file_path IS NOT NULL LIMIT 1) AS range_rel,
                ${LINES_GENERATED} AS lines,
                (SELECT COUNT(DISTINCT COALESCE(x.rel_file_path, x.file_path))
                 FROM event_line_ranges x WHERE x.event_id = e.id) AS range_files
         FROM events e
         WHERE e.session_id = ? AND ${AI_EDIT_EVENT_CONDITION}
       ),
       located AS (
         SELECT ee.id,
                MAX(CASE WHEN x.rel_file_path IS NOT NULL THEN ee.repo_id END) AS repo_id,
                COALESCE(x.rel_file_path, x.file_path) AS f,
                SUM(x.new_text_lines) AS lines
         FROM edit_events ee JOIN event_line_ranges x ON x.event_id = ee.id
         WHERE ee.file_lines IS NULL AND ee.range_files > 1
         GROUP BY ee.id, COALESCE(x.rel_file_path, x.file_path)
       ),
       located_totals AS (
         SELECT id, SUM(lines) AS lines FROM located GROUP BY id
       ),
       largest_located AS (
         SELECT id, repo_id, f
         FROM (SELECT id, repo_id, f, ROW_NUMBER() OVER (PARTITION BY id ORDER BY lines DESC, f) AS rn FROM located)
         WHERE rn = 1
       ),
       pieces AS (
         -- A split written before elements carried repo_id has no such key:
         -- fall back to the event's repository. A key that is there and null
         -- means the file is in no repository.
         SELECT ee.id,
                CASE WHEN json_type(j.value, '$.repo_id') IS NULL THEN ee.repo_id
                     ELSE json_extract(j.value, '$.repo_id') END AS repo_id,
                COALESCE(json_extract(j.value, '$.rel_file_path'), json_extract(j.value, '$.file_path')) AS f,
                COALESCE(json_extract(j.value, '$.lines_generated'), 0)
                  - COALESCE(json_extract(j.value, '$.lines_reworked'), 0) AS lines
         FROM edit_events ee, json_each(ee.file_lines) j
         WHERE ee.file_lines IS NOT NULL
         UNION ALL
         SELECT id, repo_id, f, lines FROM located
         UNION ALL
         SELECT ee.id,
                CASE WHEN ee.lines < lt.lines THEN lg.repo_id
                     WHEN ee.event_rel IS NOT NULL THEN ee.repo_id END AS repo_id,
                CASE WHEN ee.lines < lt.lines THEN lg.f ELSE ee.event_file END AS f,
                ee.lines - lt.lines AS lines
         FROM edit_events ee
         JOIN located_totals lt ON lt.id = ee.id
         JOIN largest_located lg ON lg.id = ee.id
         WHERE ee.file_lines IS NULL AND ee.range_files > 1 AND ee.measured IS NOT NULL
         UNION ALL
         SELECT id,
                CASE WHEN COALESCE(event_rel, range_rel) IS NOT NULL THEN repo_id END AS repo_id,
                CASE WHEN measured IS NOT NULL THEN COALESCE(range_file, event_file) ELSE event_file END AS f,
                lines
         FROM edit_events
         WHERE file_lines IS NULL AND range_files <= 1
       ),
       per_event_file AS (
         -- One edit can contribute two pieces to a file; it is still one edit.
         SELECT id, repo_id, f, SUM(lines) AS lines
         FROM pieces
         WHERE f IS NOT NULL
         GROUP BY id, repo_id, f
       )
       SELECT pf.f AS file, pf.repo_id, r.name AS repo_name, r.root_path AS repo_root,
              COUNT(*) AS edits, MAX(COALESCE(SUM(pf.lines), 0), 0) AS lines_generated
       FROM per_event_file pf
       LEFT JOIN repositories r ON r.id = pf.repo_id
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
