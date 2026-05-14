import { execSync, spawnSync } from "node:child_process";
import { existsSync, realpathSync } from "node:fs";
import { resolve } from "node:path";
import { NOTES_REF } from "../config.ts";
import { openDb } from "../store.ts";
import { findPromptBefore } from "../transcript.ts";
import { findRepoRoot, gitBypassEnv, relPathInRepo } from "../util/git.ts";

interface BlameOptions {
  file: string;
}

interface BlameLine {
  sha: string;
  origLine: number;
  finalLine: number;
  content: string;
  // From git blame --porcelain header (only on first chunk per commit).
  author?: string;
  authorTime?: number;
  summary?: string;
}

interface MatchedRange {
  event_id: string;
  session_id: string | null;
  agent_slug: string;
  model_slug: string | null;
  event_ts: string;
  start_line: number;
  end_line: number;
}

const ZERO_SHA = "0000000000000000000000000000000000000000";

export async function runBlame(opts: BlameOptions): Promise<void> {
  const rawAbs = resolve(opts.file);
  if (!existsSync(rawAbs)) {
    process.stderr.write(`posthook blame: file not found: ${rawAbs}\n`);
    process.exit(1);
  }
  const absPath = (() => {
    try {
      return realpathSync(rawAbs);
    } catch {
      return rawAbs;
    }
  })();
  const repoRoot = findRepoRoot(absPath);
  if (!repoRoot) {
    process.stderr.write(`posthook blame: not inside a git repo: ${absPath}\n`);
    process.exit(1);
  }
  const relPath = relPathInRepo(repoRoot, absPath);
  if (!relPath) {
    process.stderr.write(`posthook blame: file is outside repo root\n`);
    process.exit(1);
  }

  let porcelain: string;
  try {
    porcelain = execSync(`git blame --porcelain ${JSON.stringify(relPath)}`, {
      cwd: repoRoot,
      encoding: "utf8",
      maxBuffer: 64 * 1024 * 1024,
      env: gitBypassEnv(),
    });
  } catch (err) {
    process.stderr.write(
      `posthook blame: git blame failed: ${err instanceof Error ? err.message : String(err)}\n`,
    );
    process.exit(1);
  }

  const lines = parsePorcelain(porcelain);
  const matches = lookupRanges(repoRoot, relPath, lines);

  printBlame(relPath, lines, matches);
}

interface NoteEntry {
  lines: string;
  agent?: string;
  session?: string | null;
  model?: string | null;
  ts?: string;
}
interface NoteBody {
  v?: number;
  commit?: string;
  files?: Record<string, NoteEntry[]>;
}

function readNoteForCommit(repoRoot: string, sha: string): NoteBody | null {
  const res = spawnSync("git", ["notes", `--ref=${NOTES_REF}`, "show", sha], {
    cwd: repoRoot,
    encoding: "utf8",
    env: gitBypassEnv(),
  });
  if (res.status !== 0 || !res.stdout) return null;
  try {
    return JSON.parse(res.stdout) as NoteBody;
  } catch {
    return null;
  }
}

function parseLineRange(spec: string): [number, number] | null {
  const m = spec.match(/^(\d+)(?:-(\d+))?$/);
  if (!m || !m[1]) return null;
  const start = parseInt(m[1], 10);
  const end = m[2] ? parseInt(m[2], 10) : start;
  return [start, end];
}

function parsePorcelain(out: string): BlameLine[] {
  const result: BlameLine[] = [];
  const rawLines = out.split("\n");
  let i = 0;
  // Track per-commit metadata; porcelain emits header fields once per commit
  // and only the SHA line for subsequent chunks of the same commit.
  const meta = new Map<
    string,
    { author?: string; authorTime?: number; summary?: string }
  >();

  while (i < rawLines.length) {
    const headerLine = rawLines[i];
    if (headerLine === undefined || headerLine === "") {
      i++;
      continue;
    }
    const header = headerLine.match(/^([0-9a-f]{40}) (\d+) (\d+)(?: (\d+))?$/);
    if (!header || !header[1] || !header[2] || !header[3]) {
      i++;
      continue;
    }
    const sha = header[1];
    const origLine = parseInt(header[2], 10);
    const finalLine = parseInt(header[3], 10);
    let author: string | undefined;
    let authorTime: number | undefined;
    let summary: string | undefined;
    i++;
    while (i < rawLines.length) {
      const cur = rawLines[i];
      if (cur === undefined || cur.startsWith("\t")) break;
      const space = cur.indexOf(" ");
      const k = space === -1 ? cur : cur.slice(0, space);
      const v = space === -1 ? "" : cur.slice(space + 1);
      if (k === "author") author = v;
      else if (k === "author-time") authorTime = parseInt(v, 10);
      else if (k === "summary") summary = v;
      i++;
    }
    const contentLine = rawLines[i];
    if (contentLine !== undefined && contentLine.startsWith("\t")) {
      const cached = meta.get(sha) ?? {};
      if (author) cached.author = author;
      if (authorTime !== undefined) cached.authorTime = authorTime;
      if (summary) cached.summary = summary;
      meta.set(sha, cached);
      result.push({
        sha,
        origLine,
        finalLine,
        content: contentLine.slice(1),
        author: cached.author,
        authorTime: cached.authorTime,
        summary: cached.summary,
      });
      i++;
    }
  }
  return result;
}

interface RangeRow {
  event_id: string;
  session_id: string | null;
  agent_slug: string;
  model_slug: string | null;
  event_ts: string;
  start_line: number;
  end_line: number;
  commit_sha: string | null;
}

function lookupRanges(
  repoRoot: string,
  relPath: string,
  lines: BlameLine[],
): Map<number, MatchedRange> {
  const db = openDb();
  const repo = db
    .query("SELECT id FROM repositories WHERE root_path = ?")
    .get(repoRoot) as { id: string } | undefined;

  let rows: RangeRow[] = [];
  if (repo) {
    rows = db
      .query(
        `WITH file_ranges AS (
           SELECT elr.event_id, elr.start_line, elr.end_line,
                  e.session_id, e.agent_slug, e.ts AS event_ts,
                  s.model_slug
           FROM event_line_ranges elr
           JOIN events e ON e.id = elr.event_id
           LEFT JOIN sessions s ON s.id = e.session_id
           WHERE e.repo_id = ? AND elr.rel_file_path = ?
         )
         SELECT fr.event_id, fr.session_id, fr.agent_slug, fr.model_slug, fr.event_ts,
                fr.start_line, fr.end_line,
                (SELECT c.sha FROM commits c
                    WHERE c.repo_id = ?
                      AND datetime(c.committed_at) >= datetime(fr.event_ts)
                    ORDER BY datetime(c.committed_at) ASC LIMIT 1) AS commit_sha
         FROM file_ranges fr
         ORDER BY datetime(fr.event_ts) ASC`,
      )
      .all(repo.id, relPath, repo.id) as RangeRow[];
  }

  // Fall back to refs/notes/posthook for any commit in this file's blame output that
  // has no local SQLite ranges. This is how blame works for teammates who clone the repo
  // and run posthook without having captured the AI events themselves.
  const localCommits = new Set(rows.map((r) => r.commit_sha).filter((s): s is string => !!s));
  const uniqueCommits = new Set(lines.map((l) => l.sha).filter((s) => s !== ZERO_SHA));
  for (const sha of uniqueCommits) {
    if (localCommits.has(sha)) continue;
    const note = readNoteForCommit(repoRoot, sha);
    if (!note?.files) continue;
    const entries = note.files[relPath];
    if (!entries) continue;
    for (const entry of entries) {
      const r = parseLineRange(entry.lines);
      if (!r) continue;
      rows.push({
        event_id: `note:${sha}:${entry.session ?? "?"}:${r[0]}-${r[1]}`,
        session_id: entry.session ?? null,
        agent_slug: entry.agent ?? "unknown",
        model_slug: entry.model ?? null,
        event_ts: entry.ts ?? "",
        start_line: r[0],
        end_line: r[1],
        commit_sha: sha,
      });
    }
  }

  if (process.env.POSTHOOK_DEBUG === "1") {
    process.stderr.write(`[posthook] blame: ${rows.length} candidate range(s)\n`);
    for (const r of rows) {
      process.stderr.write(
        `[posthook]   range lines=${r.start_line}-${r.end_line} ts=${r.event_ts} commit=${r.commit_sha?.slice(0, 7) ?? "null"}\n`,
      );
    }
  }
  // For each blame line, find the LAST AI range whose start..end covers it AND whose commit_sha matches
  // the blame's SHA. For uncommitted (zero-sha) lines, match ranges with no commit_sha yet.
  const matches = new Map<number, MatchedRange>();
  for (const line of lines) {
    const wantCommitted = line.sha !== ZERO_SHA;
    // Pick the most recent range that fits.
    let pick: RangeRow | null = null;
    for (const r of rows) {
      const inRange = r.start_line <= line.origLine && line.origLine <= r.end_line;
      if (!inRange) continue;
      if (wantCommitted) {
        if (r.commit_sha !== line.sha) continue;
      } else {
        if (r.commit_sha !== null) continue;
      }
      pick = r; // keep iterating; rows are ordered ASC so last winning row is most recent
    }
    if (pick) {
      matches.set(line.finalLine, {
        event_id: pick.event_id,
        session_id: pick.session_id,
        agent_slug: pick.agent_slug,
        model_slug: pick.model_slug,
        event_ts: pick.event_ts,
        start_line: pick.start_line,
        end_line: pick.end_line,
      });
    }
  }
  return matches;
}

function printBlame(
  relPath: string,
  lines: BlameLine[],
  matches: Map<number, MatchedRange>,
): void {
  const db = openDb();
  const prompts = resolvePromptsForEvents(db, matches);

  console.log(`posthook blame ${relPath}`);
  console.log("");
  const lineWidth = String(lines.length).length;
  const tagWidth = 28;

  let prevEventId: string | null = null;
  for (const l of lines) {
    const match = matches.get(l.finalLine);
    let tag: string;
    if (match) {
      // Show a one-line prompt header when entering a new AI-authored block.
      if (match.event_id !== prevEventId) {
        const prompt = prompts.get(match.event_id);
        if (prompt) {
          const header = formatPrompt(prompt);
          process.stdout.write(
            `  ${" ".repeat(lineWidth)}  ┌─ ${header}\n`,
          );
        }
      }
      prevEventId = match.event_id;
      const ts = match.event_ts.slice(11, 16);
      const session = match.session_id ? match.session_id.slice(0, 8) : "?";
      const model = compactModel(match.model_slug);
      tag = `AI ${model} ${ts} ${session}`;
    } else {
      prevEventId = null;
      if (l.sha === ZERO_SHA) {
        tag = "uncommitted";
      } else {
        const author = l.author ? l.author.split(" ")[0] : "?";
        tag = `human · ${author}`;
      }
    }
    const lineNum = String(l.finalLine).padStart(lineWidth);
    process.stdout.write(`  ${lineNum}  ${tag.padEnd(tagWidth)}  ${l.content}\n`);
  }
  console.log("");
  const aiLines = Array.from(matches.values()).length;
  const totalLines = lines.length;
  const pct = totalLines > 0 ? (aiLines / totalLines) * 100 : 0;
  console.log(`  ${aiLines}/${totalLines} lines AI-authored (${pct.toFixed(1)}%)`);
}

function resolvePromptsForEvents(
  db: ReturnType<typeof openDb>,
  matches: Map<number, MatchedRange>,
): Map<string, string> {
  const result = new Map<string, string>();
  const eventIds = new Set<string>();
  for (const m of matches.values()) eventIds.add(m.event_id);
  if (eventIds.size === 0) return result;

  // Bulk-load (event_id, ts, transcript_path) for the events we care about.
  const placeholders = Array.from(eventIds).map(() => "?").join(",");
  const rows = db
    .query(
      `SELECT id, ts, json_extract(payload, '$.transcript_path') AS path
       FROM events
       WHERE id IN (${placeholders})`,
    )
    .all(...eventIds) as Array<{ id: string; ts: string; path: string | null }>;

  // Cache per-transcript prompts to avoid re-reading the same JSONL multiple times.
  const cache = new Map<string, Map<string, string | null>>();
  for (const r of rows) {
    if (!r.path) continue;
    let perTranscript = cache.get(r.path);
    if (!perTranscript) {
      perTranscript = new Map();
      cache.set(r.path, perTranscript);
    }
    let prompt = perTranscript.get(r.ts);
    if (prompt === undefined) {
      prompt = findPromptBefore(r.path, r.ts);
      perTranscript.set(r.ts, prompt ?? null);
    }
    if (prompt) result.set(r.id, prompt);
  }
  return result;
}

function formatPrompt(text: string): string {
  // Strip carriage returns and collapse whitespace for one-line display.
  // Truncate to a width that fits in standard terminals.
  const cleaned = text.replace(/\s+/g, " ").trim();
  const limit = 96;
  return cleaned.length > limit ? `${cleaned.slice(0, limit - 1)}…` : cleaned;
}

function compactModel(model: string | null): string {
  if (!model) return "?";
  // Trim version suffixes for display: "claude-opus-4-7-20250101" → "claude-opus-4-7"
  const trimmed = model.replace(/-\d{8,}$/, "");
  return trimmed.length > 16 ? trimmed.slice(0, 16) : trimmed;
}
