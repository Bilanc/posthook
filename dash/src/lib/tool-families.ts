// Every tool an agent can call folds into one of seven families. The family is
// what the dashboard charts (tool mix, time-in-tools, timeline ticks); the raw
// tool name is kept for the per-call rows. Add new tools here on purpose so
// they land in a family instead of "other".

export type ToolFamily =
  | "shell"
  | "read"
  | "edit"
  | "agent"
  | "mcp"
  | "web"
  | "other";

export const FAMILY_ORDER: ToolFamily[] = [
  "shell",
  "read",
  "edit",
  "agent",
  "mcp",
  "web",
  "other",
];

export const FAMILY_LABEL: Record<ToolFamily, string> = {
  shell: "Shell",
  read: "Read",
  edit: "Edit",
  agent: "Subagents",
  mcp: "MCP",
  web: "Web",
  other: "Other",
};

const SHELL = ["Bash", "exec", "shell"];
const READ = ["Read", "Grep", "Glob", "view_image", "NotebookRead", "LS"];
const EDIT = ["Edit", "Write", "MultiEdit", "apply_patch", "NotebookEdit"];
const AGENT = ["Agent", "Task", "TaskOutput", "SubagentHandback", "TaskCreate", "TaskUpdate"];
const WEB = ["WebSearch", "WebFetch", "webrun"];

export function toolFamily(toolName: string | null | undefined): ToolFamily {
  if (!toolName) return "other";
  if (SHELL.includes(toolName)) return "shell";
  if (EDIT.includes(toolName)) return "edit";
  if (READ.includes(toolName)) return "read";
  if (AGENT.includes(toolName) || toolName.startsWith("collaboration")) return "agent";
  if (toolName.startsWith("mcp__")) return "mcp";
  if (WEB.includes(toolName)) return "web";
  return "other";
}

const list = (xs: string[]) => xs.map((x) => `'${x}'`).join(",");

// Same mapping as toolFamily(), as a SQL CASE over `expr` (a tool-name
// expression such as json_extract(e.payload, '$.tool_name')). Keep the two in
// sync: the SQL version feeds GROUP BYs, the TS version labels rows.
export function toolFamilySql(expr: string): string {
  return `CASE
    WHEN ${expr} IN (${list(SHELL)}) THEN 'shell'
    WHEN ${expr} IN (${list(EDIT)}) THEN 'edit'
    WHEN ${expr} IN (${list(READ)}) THEN 'read'
    WHEN ${expr} IN (${list(AGENT)}) OR ${expr} LIKE 'collaboration%' THEN 'agent'
    WHEN ${expr} LIKE 'mcp\\_\\_%' ESCAPE '\\' THEN 'mcp'
    WHEN ${expr} IN (${list(WEB)}) THEN 'web'
    ELSE 'other'
  END`;
}
