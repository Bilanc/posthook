import { DatabaseSync } from "node:sqlite";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

// A throwaway posthook.db with the tables the dashboard reads (mirrors
// posthook's internal/store/schema.go) and a small, hand-checkable dataset:
//
//   Line counts follow posthook's convention: newlines in the written text.
//   session A  claude-code  10 Oct  2 prompts, Bash + Edit, Stop, Write, Stop;
//                            one commit takes 4 of its 5 generated lines.
//   session B  codex        11 Oct  1 prompt, one Bash call without duration,
//                            no edits, no commit          -> explored
//   session C  claude-code  12 Oct  1 prompt, two edits to the same file,
//                            no commit                    -> edited
//
// Timestamps sit at 10:00 UTC so every local day is the same calendar day for
// any machine between UTC-10 and UTC+12.
export const SCHEMA = `
CREATE TABLE repositories (id TEXT PRIMARY KEY, org_id TEXT NOT NULL DEFAULT 'local', remote_url TEXT, name TEXT NOT NULL, root_path TEXT NOT NULL UNIQUE, created_at TEXT NOT NULL DEFAULT (datetime('now')), synced_at TEXT);
CREATE TABLE sessions (id TEXT PRIMARY KEY, org_id TEXT NOT NULL DEFAULT 'local', agent_slug TEXT NOT NULL, model_slug TEXT, repo_id TEXT REFERENCES repositories(id), branch TEXT, cwd TEXT, started_at TEXT NOT NULL, ended_at TEXT, engineer_email TEXT, engineer_name TEXT, synced_at TEXT, input_tokens INTEGER, output_tokens INTEGER, cache_read_tokens INTEGER, cache_creation_tokens INTEGER);
CREATE TABLE events (id TEXT PRIMARY KEY, org_id TEXT NOT NULL DEFAULT 'local', session_id TEXT REFERENCES sessions(id), ts TEXT NOT NULL, event_type TEXT NOT NULL, agent_slug TEXT NOT NULL, cwd TEXT, file_path TEXT, lines_added INTEGER, lines_removed INTEGER, payload TEXT, repo_id TEXT REFERENCES repositories(id), rel_file_path TEXT, synced_at TEXT);
CREATE TABLE commits (id TEXT PRIMARY KEY, org_id TEXT NOT NULL DEFAULT 'local', repo_id TEXT NOT NULL REFERENCES repositories(id), sha TEXT NOT NULL, parent_sha TEXT, author_email TEXT, author_name TEXT, committed_at TEXT NOT NULL, branch TEXT, message TEXT, lines_added INTEGER NOT NULL DEFAULT 0, lines_removed INTEGER NOT NULL DEFAULT 0, files_changed INTEGER NOT NULL DEFAULT 0, synced_at TEXT, UNIQUE(repo_id, sha));
CREATE TABLE commit_files (commit_id TEXT NOT NULL REFERENCES commits(id), file_path TEXT NOT NULL, lines_added INTEGER NOT NULL DEFAULT 0, lines_removed INTEGER NOT NULL DEFAULT 0, synced_at TEXT, PRIMARY KEY (commit_id, file_path));
CREATE TABLE event_line_ranges (id TEXT PRIMARY KEY, event_id TEXT NOT NULL REFERENCES events(id), file_path TEXT NOT NULL, rel_file_path TEXT, blob_sha_after TEXT, start_line INTEGER NOT NULL, end_line INTEGER NOT NULL, new_text_lines INTEGER NOT NULL, created_at TEXT NOT NULL DEFAULT (datetime('now')), synced_at TEXT);
CREATE TABLE commit_sessions (commit_id TEXT NOT NULL REFERENCES commits(id), session_id TEXT NOT NULL REFERENCES sessions(id), agent_slug TEXT NOT NULL, model_slug TEXT, first_event_ts TEXT NOT NULL, last_event_ts TEXT NOT NULL, event_count INTEGER NOT NULL DEFAULT 0, files_touched INTEGER NOT NULL DEFAULT 0, lines_attributed INTEGER NOT NULL DEFAULT 0, attribution_source TEXT NOT NULL DEFAULT 'event_line_ranges', confidence TEXT NOT NULL DEFAULT 'line_range_next_file_commit', synced_at TEXT, PRIMARY KEY (commit_id, session_id));
CREATE TABLE commit_session_files (commit_id TEXT NOT NULL REFERENCES commits(id), session_id TEXT NOT NULL REFERENCES sessions(id), file_path TEXT NOT NULL, first_event_ts TEXT NOT NULL, last_event_ts TEXT NOT NULL, event_count INTEGER NOT NULL DEFAULT 0, lines_attributed INTEGER NOT NULL DEFAULT 0, synced_at TEXT, PRIMARY KEY (commit_id, session_id, file_path));
CREATE TABLE session_prompts (id TEXT PRIMARY KEY, org_id TEXT NOT NULL DEFAULT 'local', session_id TEXT NOT NULL REFERENCES sessions(id), ts TEXT, seq INTEGER NOT NULL, agent_slug TEXT, source TEXT NOT NULL DEFAULT 'transcript', prompt_text TEXT NOT NULL, created_at TEXT NOT NULL DEFAULT (datetime('now')), synced_at TEXT, UNIQUE(session_id, seq));
`;

const J = (o: unknown) => JSON.stringify(o);
const post = (tool: string, input: Record<string, unknown>, extra: Record<string, unknown> = {}) =>
  J({ hook_event_name: "PostToolUse", tool_name: tool, tool_input: input, ...extra });

export const FIXTURE_DATA = `
INSERT INTO repositories (id, name, root_path, remote_url) VALUES ('r1', 'demo', '/tmp/demo', 'git@github.com:acme/demo.git');
INSERT INTO sessions (id, agent_slug, model_slug, repo_id, branch, started_at, ended_at, engineer_email, engineer_name, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens) VALUES
  ('A', 'claude-code', 'claude-fable-5-1', 'r1', 'main', '2026-10-10T10:00:00Z', '2026-10-10T10:30:00Z', 'dev@acme.io', 'Dev', 1000, 5000, 20000, 3000),
  ('B', 'codex', 'gpt-6', 'r1', 'main', '2026-10-11T10:00:00Z', '2026-10-11T10:05:00Z', 'dev@acme.io', 'Dev', NULL, NULL, NULL, NULL),
  ('C', 'claude-code', 'claude-fable-5-1', 'r1', 'feat', '2026-10-12T10:00:00Z', '2026-10-12T10:01:00Z', 'dev@acme.io', 'Dev', 500, 2000, 1000, 100);
INSERT INTO events (id, session_id, ts, event_type, agent_slug, repo_id, rel_file_path, payload) VALUES
  ('e1', 'A', '2026-10-10T10:00:10Z', 'PostToolUse', 'claude-code', 'r1', NULL, '${post("Bash", { command: "ls -la" }, { duration_ms: 2000, tool_response: { stdout: "", stderr: "" } })}'),
  ('e2', 'A', '2026-10-10T10:00:20Z', 'PostToolUse', 'claude-code', 'r1', 'src/a.ts', '${post("Edit", { file_path: "/tmp/demo/src/a.ts", old_string: "", new_string: "a\nb\nc\n" }, { duration_ms: 50 })}'),
  ('e3', 'A', '2026-10-10T10:00:30Z', 'Stop', 'claude-code', 'r1', NULL, '${J({ hook_event_name: "Stop", last_assistant_message: "done", effort: { level: "high" }, permission_mode: "auto", transcript_path: "/nonexistent/a.jsonl" })}'),
  ('e4', 'A', '2026-10-20T10:20:30Z', 'PostToolUse', 'claude-code', 'r1', 'src/b.ts', '${post("Write", { file_path: "/tmp/demo/src/b.ts", content: "x\ny\n" }, { duration_ms: 40 })}'),
  ('e5', 'A', '2026-10-20T10:20:40Z', 'Stop', 'claude-code', 'r1', NULL, '${J({ hook_event_name: "Stop", last_assistant_message: "wrote b", effort: { level: "high" }, permission_mode: "auto" })}'),
  ('e6', 'B', '2026-10-11T10:00:10Z', 'PostToolUse', 'codex', 'r1', NULL, '${post("Bash", { command: "cat README.md" })}'),
  ('e7', 'B', '2026-10-11T10:00:20Z', 'Stop', 'codex', 'r1', NULL, '${J({ hook_event_name: "Stop", last_assistant_message: "looked" })}'),
  ('e8', 'C', '2026-10-12T10:00:10Z', 'PostToolUse', 'claude-code', 'r1', 'src/c.ts', '${post("Edit", { old_string: "", new_string: "q\n" }, { duration_ms: 30 })}'),
  ('e9', 'C', '2026-10-12T10:00:20Z', 'PostToolUse', 'claude-code', 'r1', 'src/c.ts', '${post("Edit", { old_string: "q", new_string: "r\ns\n" }, { duration_ms: 30 })}'),
  ('e10', 'C', '2026-10-12T10:00:30Z', 'Stop', 'claude-code', 'r1', NULL, '${J({ hook_event_name: "Stop", last_assistant_message: "edited c" })}');
INSERT INTO session_prompts (id, session_id, ts, seq, agent_slug, prompt_text) VALUES
  ('p1', 'A', '2026-10-10T10:00:05Z', 1, 'claude-code', 'Fix the thing'),
  ('p2', 'A', '2026-10-20T10:20:25Z', 2, 'claude-code', 'Now the other'),
  ('p3', 'B', '2026-10-11T10:00:05Z', 1, 'codex', 'look around'),
  ('p4', 'C', '2026-10-12T10:00:05Z', 1, 'claude-code', 'Touch c');
INSERT INTO commits (id, repo_id, sha, committed_at, message, lines_added, lines_removed, files_changed) VALUES
  ('c1', 'r1', 'abc1234', '2026-10-20T10:25:00Z', 'ship it', 10, 1, 2);
INSERT INTO commit_sessions (commit_id, session_id, agent_slug, model_slug, first_event_ts, last_event_ts, event_count, files_touched, lines_attributed) VALUES
  ('c1', 'A', 'claude-code', 'claude-fable-5-1', '2026-10-10T10:00:20Z', '2026-10-20T10:20:30Z', 2, 2, 4);
INSERT INTO commit_session_files (commit_id, session_id, file_path, first_event_ts, last_event_ts, event_count, lines_attributed) VALUES
  ('c1', 'A', 'src/a.ts', '2026-10-10T10:00:20Z', '2026-10-10T10:00:20Z', 1, 3),
  ('c1', 'A', 'src/b.ts', '2026-10-20T10:20:30Z', '2026-10-20T10:20:30Z', 1, 1);
`;

// Creates the fixture DB and points db.ts at it. Call before importing any
// query module (db() opens lazily, reading POSTHOOK_DB on first use).
export function installFixture(): string {
  const dir = mkdtempSync(join(tmpdir(), "posthook-dash-fixture-"));
  const path = join(dir, "posthook.db");
  const conn = new DatabaseSync(path);
  conn.exec(SCHEMA);
  conn.exec(FIXTURE_DATA);
  conn.close();
  process.env.POSTHOOK_DB = path;
  return path;
}
