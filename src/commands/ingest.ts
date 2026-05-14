import { execSync } from "node:child_process";
import { createHash, randomUUID } from "node:crypto";
import { existsSync, readFileSync } from "node:fs";
import { basename } from "node:path";
import { NOTES_REF } from "../config.ts";
import { extractRanges } from "../lineRanges.ts";
import { LOCAL_ORG_ID, openDb } from "../store.ts";
import { parseClaudeTranscript } from "../transcript.ts";
import { canonicalize, findRepoRoot, relPathInRepo } from "../util/git.ts";
import { debug, warn } from "../util/log.ts";

const EDIT_TOOLS = new Set(["Edit", "Write", "MultiEdit"]);

interface IngestOptions {
  agent?: string;
  kind?: string;
  repoRoot?: string;
  sha?: string;
}

export async function runIngest(opts: IngestOptions): Promise<void> {
  if (opts.kind === "git-commit") {
    await ingestGitCommit(opts);
    return;
  }
  if (opts.agent) {
    await ingestAgentEvent(opts.agent);
    return;
  }
  throw new Error("ingest requires either --agent <slug> or --kind git-commit");
}

async function readStdin(): Promise<string> {
  // bun: process.stdin is a Readable stream.
  if (process.stdin.isTTY) return "";
  const chunks: Buffer[] = [];
  for await (const chunk of process.stdin) {
    chunks.push(Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk));
  }
  return Buffer.concat(chunks).toString("utf8");
}

async function ingestAgentEvent(agentSlug: string): Promise<void> {
  const raw = await readStdin();
  if (raw.trim() === "") {
    // Hook fired but no payload arrived — usually a misconfigured hook command
    // or an agent invoking the hook outside its expected flow. Recording these
    // lets `posthook status` show data-quality issues instead of silently dropping.
    recordHookMisfire(agentSlug);
    debug(`ingest --agent ${agentSlug}: no stdin payload, recorded misfire`);
    return;
  }
  let payload: Record<string, unknown> = {};
  try {
    payload = JSON.parse(raw);
  } catch {
    // Some agents send non-JSON payloads or wrap them. Store raw text.
    payload = { raw };
  }

  const db = openDb();
  const id = randomUUID();
  const ts = new Date().toISOString();
  const eventType = pickString(payload, ["hook_event_name", "event", "type"]) ?? "unknown";
  const sessionId = pickString(payload, ["session_id", "sessionId"]);
  const cwd = pickString(payload, ["cwd"]) ?? process.cwd();
  const filePath = extractFilePath(payload);
  const canonicalFilePath = filePath ? canonicalize(filePath) : null;
  const model = pickString(payload, ["model", "model_id"]);

  // Resolve repo from cwd (walks up looking for .git). Auto-registers if found.
  // findRepoRoot returns the canonical path, so we canonicalize filePath before computing
  // a relative path — otherwise /var/folders/... and /private/var/... mismatch on macOS.
  const repoRoot = findRepoRoot(cwd);
  const repoId = repoRoot ? upsertRepositoryByCwd(db, repoRoot) : null;
  const relFilePath =
    repoRoot && canonicalFilePath ? relPathInRepo(repoRoot, canonicalFilePath) : null;

  // `git config --get` already resolves local → global, so one call per key.
  const engineerEmail = repoRoot ? safeGit(["config", "--get", "user.email"], repoRoot) : null;
  const engineerName = repoRoot ? safeGit(["config", "--get", "user.name"], repoRoot) : null;

  if (sessionId) {
    db.run(
      `INSERT INTO sessions (
         id, org_id, agent_slug, model_slug, repo_id, cwd, started_at,
         engineer_email, engineer_name
       )
       VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
       ON CONFLICT(id) DO UPDATE SET
         model_slug     = COALESCE(excluded.model_slug, sessions.model_slug),
         repo_id        = COALESCE(sessions.repo_id, excluded.repo_id),
         cwd            = COALESCE(sessions.cwd, excluded.cwd),
         engineer_email = COALESCE(sessions.engineer_email, excluded.engineer_email),
         engineer_name  = COALESCE(sessions.engineer_name, excluded.engineer_name)`,
      [
        sessionId,
        LOCAL_ORG_ID,
        agentSlug,
        model ?? null,
        repoId,
        cwd,
        ts,
        engineerEmail,
        engineerName,
      ],
    );
  }

  db.run(
    `INSERT INTO events (
      id, org_id, session_id, ts, event_type, agent_slug, cwd,
      file_path, repo_id, rel_file_path, payload
    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
    [
      id,
      LOCAL_ORG_ID,
      sessionId ?? null,
      ts,
      eventType,
      agentSlug,
      cwd,
      canonicalFilePath ?? filePath ?? null,
      repoId,
      relFilePath ?? null,
      raw,
    ],
  );
  debug(`ingested ${agentSlug} event ${eventType} (session=${sessionId ?? "?"})`);

  // Capture line ranges for AI edits. Fail-soft: any failure here must not block ingest.
  if (eventType === "PostToolUse" && canonicalFilePath) {
    const toolName = readPayloadString(payload, ["tool_name"]);
    if (toolName && EDIT_TOOLS.has(toolName)) {
      try {
        captureLineRanges(db, id, canonicalFilePath, relFilePath, toolName, payload);
      } catch (err) {
        warn(`line-range capture failed: ${err instanceof Error ? err.message : String(err)}`);
      }
    }
  }

  // Stop event: parse the transcript to backfill model + session span.
  // Tokens/cost are intentionally not recorded — see transcript.ts.
  if (eventType === "Stop" && sessionId) {
    const transcriptPath = pickString(payload, ["transcript_path", "transcriptPath"]);
    if (transcriptPath) {
      try {
        const summary = await parseClaudeTranscript(transcriptPath);
        if (summary) {
          db.run(
            `UPDATE sessions SET
               model_slug = COALESCE(?, model_slug),
               started_at = COALESCE(?, started_at),
               ended_at = COALESCE(?, ended_at)
             WHERE id = ?`,
            [summary.model, summary.first_ts, summary.last_ts, sessionId],
          );
          debug(
            `updated session ${sessionId} from transcript: model=${summary.model} messages=${summary.assistant_message_count}`,
          );
        }
      } catch (err) {
        warn(`transcript parse failed: ${err instanceof Error ? err.message : String(err)}`);
      }
    }
  }
}

function captureLineRanges(
  db: ReturnType<typeof openDb>,
  eventId: string,
  filePath: string,
  relFilePath: string | null,
  toolName: string,
  payload: Record<string, unknown>,
): void {
  if (!existsSync(filePath)) {
    debug(`line-range: file gone, skipping (${filePath})`);
    return;
  }
  const content = readFileSync(filePath, "utf8");
  const toolInput = payload.tool_input;
  if (!toolInput || typeof toolInput !== "object" || Array.isArray(toolInput)) return;

  const { ranges, unlocated } = extractRanges(
    toolName,
    toolInput as Parameters<typeof extractRanges>[1],
    content,
  );
  if (ranges.length === 0) {
    if (unlocated > 0) debug(`line-range: ${unlocated} edits unlocated in ${filePath}`);
    return;
  }

  const sha = createHash("sha256").update(content).digest("hex");
  const insert = db.prepare(
    `INSERT INTO event_line_ranges (
      id, event_id, file_path, rel_file_path, blob_sha_after,
      start_line, end_line, new_text_lines
    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
  );
  for (const r of ranges) {
    insert.run(
      randomUUID(),
      eventId,
      filePath,
      relFilePath,
      sha,
      r.start_line,
      r.end_line,
      r.new_text_lines,
    );
  }
  debug(
    `line-range: captured ${ranges.length} range(s) for ${toolName} on ${relFilePath ?? filePath}` +
      (unlocated > 0 ? ` (${unlocated} unlocated)` : ""),
  );
}

function readPayloadString(obj: Record<string, unknown>, keys: string[]): string | null {
  for (const k of keys) {
    const v = obj[k];
    if (typeof v === "string" && v.length > 0) return v;
  }
  return null;
}

function recordHookMisfire(agentSlug: string): void {
  const db = openDb();
  const id = randomUUID();
  const ts = new Date().toISOString();
  const cwd = process.cwd();
  const repoRoot = findRepoRoot(cwd);
  const repoId = repoRoot ? upsertRepositoryByCwd(db, repoRoot) : null;
  db.run(
    `INSERT INTO events (
      id, org_id, session_id, ts, event_type, agent_slug, cwd,
      file_path, repo_id, rel_file_path, payload
    ) VALUES (?, ?, NULL, ?, 'hook_misfire', ?, ?, NULL, ?, NULL, NULL)`,
    [id, LOCAL_ORG_ID, ts, agentSlug, cwd, repoId],
  );
}

function upsertRepositoryByCwd(db: ReturnType<typeof openDb>, rootPath: string): string {
  const existing = db
    .query("SELECT id FROM repositories WHERE root_path = ?")
    .get(rootPath) as { id: string } | undefined;
  if (existing) return existing.id;
  const id = randomUUID();
  const remoteUrl = safeGit(["config", "--get", "remote.origin.url"], rootPath);
  const name = remoteUrl ? guessRepoNameFromRemote(remoteUrl) : basename(rootPath);
  db.run(
    `INSERT INTO repositories (id, org_id, remote_url, name, root_path)
     VALUES (?, ?, ?, ?, ?)`,
    [id, LOCAL_ORG_ID, remoteUrl ?? null, name, rootPath],
  );
  return id;
}

async function ingestGitCommit(opts: IngestOptions): Promise<void> {
  if (!opts.repoRoot || !opts.sha) {
    throw new Error("git-commit ingest requires --repo-root and --sha");
  }
  const db = openDb();
  const repoRoot = canonicalize(opts.repoRoot);
  const sha = opts.sha;

  const fmt = "%H%x1f%P%x1f%ae%x1f%an%x1f%cI%x1f%s";
  let line: string;
  try {
    line = execSync(`git log -1 --format=${fmt} ${sha}`, {
      cwd: repoRoot,
      encoding: "utf8",
      env: { ...process.env, POSTHOOK_BYPASS: "1" },
    }).trim();
  } catch (err) {
    warn(`git log failed for ${sha}: ${err instanceof Error ? err.message : String(err)}`);
    return;
  }
  const [_h, parents, email, name, committedAt, subject] = line.split("\x1f");
  const parentSha = (parents ?? "").split(" ")[0] || null;
  const branch = safeGit(["rev-parse", "--abbrev-ref", "HEAD"], repoRoot);
  const remoteUrl = safeGit(["config", "--get", "remote.origin.url"], repoRoot);
  const repoName = remoteUrl ? guessRepoNameFromRemote(remoteUrl) : basename(repoRoot);

  const repoId = upsertRepository(db, repoRoot, remoteUrl, repoName);

  // shortstat for totals. git show works on root commits too, unlike git diff.
  let added = 0;
  let removed = 0;
  let filesChanged = 0;
  const numstat = safeGit(["show", "--numstat", "--format=", sha], repoRoot) ?? "";
  const perFile: Array<{ path: string; added: number; removed: number }> = [];
  for (const row of numstat.split("\n")) {
    if (!row.trim()) continue;
    const [a, r, ...pathParts] = row.split("\t");
    const path = pathParts.join("\t");
    if (!path) continue;
    const ai = a === "-" ? 0 : parseInt(a ?? "0", 10) || 0;
    const ri = r === "-" ? 0 : parseInt(r ?? "0", 10) || 0;
    added += ai;
    removed += ri;
    filesChanged += 1;
    perFile.push({ path, added: ai, removed: ri });
  }

  const commitId = randomUUID();
  try {
    db.run(
      `INSERT INTO commits (
        id, org_id, repo_id, sha, parent_sha, author_email, author_name,
        committed_at, branch, message, lines_added, lines_removed, files_changed
      ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
      ON CONFLICT(repo_id, sha) DO NOTHING`,
      [
        commitId,
        LOCAL_ORG_ID,
        repoId,
        sha,
        parentSha,
        email ?? null,
        name ?? null,
        committedAt ?? new Date().toISOString(),
        branch ?? null,
        subject ?? null,
        added,
        removed,
        filesChanged,
      ],
    );
  } catch (err) {
    warn(`commit insert failed: ${err instanceof Error ? err.message : String(err)}`);
    return;
  }

  const existing = db
    .query("SELECT id FROM commits WHERE repo_id = ? AND sha = ?")
    .get(repoId, sha) as { id: string } | undefined;
  const finalCommitId = existing?.id ?? commitId;

  const insertFile = db.prepare(
    `INSERT INTO commit_files (commit_id, file_path, lines_added, lines_removed)
     VALUES (?, ?, ?, ?)
     ON CONFLICT(commit_id, file_path) DO NOTHING`,
  );
  for (const f of perFile) {
    insertFile.run(finalCommitId, f.path, f.added, f.removed);
  }
  debug(`ingested commit ${sha.slice(0, 7)} in ${repoName} (+${added}/-${removed})`);

  // Serialize AI line ranges into refs/notes/posthook so blame works for teammates
  // who clone the repo. Only metadata (line ranges, agent, session ID, model, ts) —
  // not prompt content. Failures here are non-fatal: blame still works from local SQLite.
  try {
    writeNoteForCommit(db, repoRoot, repoId, sha);
  } catch (err) {
    warn(`note write failed for ${sha.slice(0, 7)}: ${err instanceof Error ? err.message : String(err)}`);
  }
}

interface NoteRangeRow {
  rel_file_path: string;
  session_id: string | null;
  agent_slug: string;
  model_slug: string | null;
  event_ts: string;
  start_line: number;
  end_line: number;
}

function writeNoteForCommit(
  db: ReturnType<typeof openDb>,
  repoRoot: string,
  repoId: string,
  sha: string,
): void {
  // Pull all AI line ranges that resolve to this commit (event ts < commit ts, no later commit between).
  const ranges = db
    .query(
      `WITH file_ranges AS (
         SELECT elr.rel_file_path, e.session_id, e.agent_slug, e.ts AS event_ts,
                s.model_slug, elr.start_line, elr.end_line
         FROM event_line_ranges elr
         JOIN events e ON e.id = elr.event_id
         LEFT JOIN sessions s ON s.id = e.session_id
         WHERE e.repo_id = ? AND elr.rel_file_path IS NOT NULL
       )
       SELECT fr.rel_file_path, fr.session_id, fr.agent_slug, fr.model_slug,
              fr.event_ts, fr.start_line, fr.end_line
       FROM file_ranges fr
       WHERE (
         SELECT c.sha FROM commits c
         WHERE c.repo_id = ?
           AND datetime(c.committed_at) >= datetime(fr.event_ts)
         ORDER BY datetime(c.committed_at) ASC LIMIT 1
       ) = ?
       ORDER BY fr.rel_file_path, fr.start_line`,
    )
    .all(repoId, repoId, sha) as NoteRangeRow[];
  if (ranges.length === 0) {
    debug(`note: no AI ranges for ${sha.slice(0, 7)}, skipping`);
    return;
  }

  const files: Record<string, Array<Record<string, unknown>>> = {};
  for (const r of ranges) {
    const arr = files[r.rel_file_path] ?? (files[r.rel_file_path] = []);
    arr.push({
      lines: r.start_line === r.end_line ? `${r.start_line}` : `${r.start_line}-${r.end_line}`,
      agent: r.agent_slug,
      session: r.session_id,
      model: r.model_slug,
      ts: r.event_ts,
    });
  }
  const note = JSON.stringify({ v: 1, commit: sha, files });
  try {
    execSync(`git notes --ref=${NOTES_REF} add -f -F - ${sha}`, {
      cwd: repoRoot,
      input: note,
      encoding: "utf8",
      env: { ...process.env, POSTHOOK_BYPASS: "1" },
    });
    debug(`note: wrote ${ranges.length} range(s) for ${sha.slice(0, 7)}`);
  } catch (err) {
    throw err instanceof Error ? err : new Error(String(err));
  }
}

function upsertRepository(
  db: ReturnType<typeof openDb>,
  rootPath: string,
  remoteUrl: string | null,
  name: string,
): string {
  const existing = db
    .query("SELECT id FROM repositories WHERE root_path = ?")
    .get(rootPath) as { id: string } | undefined;
  if (existing) return existing.id;
  const id = randomUUID();
  db.run(
    `INSERT INTO repositories (id, org_id, remote_url, name, root_path)
     VALUES (?, ?, ?, ?, ?)`,
    [id, LOCAL_ORG_ID, remoteUrl ?? null, name, rootPath],
  );
  return id;
}

function safeGit(args: string[], cwd: string): string | null {
  try {
    // POSTHOOK_BYPASS=1 prevents the git shadow (if installed) from recursing into
    // ourselves when we're already inside an ingest path. Plain pass-through to
    // real git is what we want for these read-only queries.
    return execSync(`git ${args.map((a) => JSON.stringify(a)).join(" ")}`, {
      cwd,
      encoding: "utf8",
      env: { ...process.env, POSTHOOK_BYPASS: "1" },
    }).trim();
  } catch {
    return null;
  }
}

function guessRepoNameFromRemote(remote: string): string {
  const m = remote.match(/[\/:]([^/]+?)(\.git)?$/);
  return m?.[1] ?? remote;
}

function pickString(obj: Record<string, unknown>, keys: string[]): string | null {
  for (const k of keys) {
    const v = obj[k];
    if (typeof v === "string" && v.length > 0) return v;
  }
  return null;
}

function extractFilePath(payload: Record<string, unknown>): string | null {
  // Claude Code: tool_input.file_path; Cursor: file_paths[0]; Codex: varies.
  const ti = payload.tool_input;
  if (ti && typeof ti === "object" && !Array.isArray(ti)) {
    const fp = (ti as Record<string, unknown>).file_path;
    if (typeof fp === "string") return fp;
  }
  const fps = payload.file_paths;
  if (Array.isArray(fps) && typeof fps[0] === "string") return fps[0];
  const fp = payload.file_path;
  if (typeof fp === "string") return fp;
  return null;
}
