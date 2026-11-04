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
export const LINES_GENERATED = `CASE
  WHEN ${LINE_RANGE_LINES} > 0 THEN ${LINE_RANGE_LINES}
  WHEN json_extract(e.payload, '$.tool_name') = 'apply_patch' THEN COALESCE(e.lines_added, 0)
  ELSE ${NL("COALESCE(json_extract(e.payload, '$.tool_input.new_string'), '')")}
    + ${NL("COALESCE(json_extract(e.payload, '$.tool_input.content'), '')")}
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
