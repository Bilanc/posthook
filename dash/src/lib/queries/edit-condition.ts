// The SQL that decides what counts as an AI edit and how many lines it wrote.
// Shared by every query that scores edits; kept in its own module so the
// per-session, outcome and model queries can import it without forming an
// import cycle through sessions.ts.

export const NL = (col: string) => `(length(${col}) - length(replace(${col}, char(10), '')))`;
export const LINE_RANGE_LINES = `COALESCE((
  SELECT SUM(elr.new_text_lines)
  FROM event_line_ranges elr
  WHERE elr.event_id = e.id
), 0)`;
// How edits were scored before posthook measured them itself: the located
// line ranges, else the patch's own count, else the newlines in the text the
// tool wrote. Still used for events an older client recorded.
const LEGACY_LINES = `CASE
  WHEN ${LINE_RANGE_LINES} > 0 THEN ${LINE_RANGE_LINES}
  WHEN json_extract(e.payload, '$.tool_name') = 'apply_patch' THEN COALESCE(e.lines_added, 0)
  ELSE ${NL("COALESCE(json_extract(e.payload, '$.tool_input.new_string'), '')")}
    + ${NL("COALESCE(json_extract(e.payload, '$.tool_input.content'), '')")}
END`;
// posthook now records, per edit event, the lines it added or changed from an
// exact diff (events.lines_generated, 0 for a file outside the repo or
// ignored by git) and how many lines it removed that the same session had
// written earlier (events.lines_reworked). Both are NULL on older events.
//
// LINES_WRITTEN is what one edit wrote. LINES_GENERATED is that net of the
// session's own rework, the figure to SUM: an edit that replaces 30 of the
// session's lines with 5 contributes -25, so a session's total is the lines
// it has standing, not the churn it took to get there.
export const LINES_WRITTEN = `CASE
  WHEN e.lines_generated IS NOT NULL THEN e.lines_generated
  ELSE ${LEGACY_LINES}
END`;
export const LINES_GENERATED = `CASE
  WHEN e.lines_generated IS NOT NULL THEN e.lines_generated - COALESCE(e.lines_reworked, 0)
  ELSE ${LEGACY_LINES}
END`;
// Cursor reports an edit through both PostToolUse and afterFileEdit. Keep the
// afterFileEdit event and suppress PostToolUse when a matching afterFileEdit
// exists for the same file/session within five seconds.
export const AI_EDIT_EVENT_CONDITION = `(
  (
    e.event_type IN ('PostToolUse', 'postToolUse')
    AND (
      json_extract(e.payload, '$.tool_name') IN ('Edit', 'Write', 'MultiEdit', 'apply_patch')
      -- shell commands (Bash heredocs …) count once lines were located in the file
      OR EXISTS (SELECT 1 FROM event_line_ranges selr WHERE selr.event_id = e.id)
      -- … or measured without a position (the text had moved by the time it was
      -- read), or measured as only removing the session's own lines: its rework
      -- has to be subtracted even though it wrote nothing
      OR COALESCE(e.lines_generated, 0) > 0
      OR COALESCE(e.lines_reworked, 0) > 0
    )
    AND NOT (
      e.agent_slug = 'cursor'
      AND EXISTS (
        SELECT 1
        FROM events afe
        WHERE afe.agent_slug = 'cursor'
          AND afe.event_type = 'afterFileEdit'
          AND afe.file_path = e.file_path
          AND (
            afe.session_id = e.session_id
            OR (afe.session_id IS NULL AND e.session_id IS NULL)
          )
          AND ABS((julianday(afe.ts) - julianday(e.ts)) * 86400.0) <= 5
      )
    )
  )
  OR e.event_type = 'afterFileEdit'
)`;
