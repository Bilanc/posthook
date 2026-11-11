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

export interface CommitFile {
  commit_id: string;
  file_path: string;
  lines_attributed: number;
}

export interface SessionFile {
  file: string;
  edits: number;
  lines_generated: number;
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

// The file a line range sits in, for the per-file query below (ee = the edit
// event, x = one of its ranges). The oldest ranges carry only an absolute
// path; when that is the event's own file, use the event's name for it so
// the file is not listed twice.
const RANGE_FILE = `COALESCE(
  x.rel_file_path,
  CASE WHEN x.file_path = ee.abs_file THEN ee.event_file END,
  x.file_path
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
      `SELECT commit_id, file_path, lines_attributed FROM commit_session_files WHERE session_id = ?`,
    )
    .all(sessionId) as CommitFile[];

  const files = conn
    .prepare(
      // One row per file an edit touched. Lines with a position go to the
      // file their range sits in, whichever file the event row names: an
      // apply_patch across files carries one path, the first it named. The
      // rest of the event's count (lines measured without a position, less
      // the session's own rework) goes to the file the event names. So a
      // patch that could place its lines in only one of its files no longer
      // puts everything on the first, and the files still add up to the
      // session's total.
      `WITH edit_events AS (
         SELECT e.id,
                e.file_path AS abs_file,
                ${FILE_EXPR} AS event_file,
                ${LINES_GENERATED} AS lines
         FROM events e
         WHERE e.session_id = ? AND ${AI_EDIT_EVENT_CONDITION}
       ),
       located AS (
         SELECT ee.id, ${RANGE_FILE} AS f, SUM(x.new_text_lines) AS lines
         FROM edit_events ee JOIN event_line_ranges x ON x.event_id = ee.id
         GROUP BY ee.id, ${RANGE_FILE}
       ),
       per_event_file AS (
         SELECT id, f, SUM(lines) AS lines
         FROM (
           SELECT id, f, lines FROM located
           UNION ALL
           SELECT ee.id, ee.event_file AS f,
                  ee.lines - COALESCE((SELECT SUM(l.lines) FROM located l WHERE l.id = ee.id), 0) AS lines
           FROM edit_events ee
         )
         WHERE f IS NOT NULL
         GROUP BY id, f
       )
       SELECT f AS file, COUNT(*) AS edits, MAX(COALESCE(SUM(lines), 0), 0) AS lines_generated
       FROM per_event_file
       GROUP BY f
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
