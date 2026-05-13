import { execSync } from "node:child_process";
import { randomUUID } from "node:crypto";
import { basename } from "node:path";
import { computeCost, type TokenCounts } from "../pricing.ts";
import { LOCAL_ORG_ID, openDb } from "../store.ts";
import { parseClaudeTranscript } from "../transcript.ts";
import { findRepoRoot, relPathInRepo } from "../util/git.ts";
import { debug, warn } from "../util/log.ts";

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
    debug(`ingest --agent ${agentSlug}: no stdin payload, skipping`);
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
  const model = pickString(payload, ["model", "model_id"]);

  // Resolve repo from cwd (walks up looking for .git). Auto-registers if found.
  const repoRoot = findRepoRoot(cwd);
  const repoId = repoRoot ? upsertRepositoryByCwd(db, repoRoot) : null;
  const relFilePath = repoRoot && filePath ? relPathInRepo(repoRoot, filePath) : null;

  if (sessionId) {
    db.run(
      `INSERT INTO sessions (id, org_id, agent_slug, model_slug, repo_id, cwd, started_at)
       VALUES (?, ?, ?, ?, ?, ?, ?)
       ON CONFLICT(id) DO UPDATE SET
         model_slug = COALESCE(excluded.model_slug, sessions.model_slug),
         repo_id = COALESCE(sessions.repo_id, excluded.repo_id),
         cwd = COALESCE(sessions.cwd, excluded.cwd)`,
      [sessionId, LOCAL_ORG_ID, agentSlug, model ?? null, repoId, cwd, ts],
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
      filePath ?? null,
      repoId,
      relFilePath ?? null,
      raw,
    ],
  );
  debug(`ingested ${agentSlug} event ${eventType} (session=${sessionId ?? "?"})`);

  // Stop event: parse the transcript and update the session with tokens/cost.
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
               ended_at = COALESCE(?, ended_at),
               tokens_in = ?,
               tokens_out = ?,
               tokens_cache_read = ?,
               tokens_cache_write = ?,
               cost_usd = ?
             WHERE id = ?`,
            [
              summary.model,
              summary.first_ts,
              summary.last_ts,
              summary.tokens.input,
              summary.tokens.output,
              summary.tokens.cacheRead,
              summary.tokens.cacheWrite,
              summary.cost_usd,
              sessionId,
            ],
          );
          debug(
            `updated session ${sessionId} from transcript: model=${summary.model} tokens_in=${summary.tokens.input} cost=$${summary.cost_usd.toFixed(4)}`,
          );
        }
      } catch (err) {
        warn(`transcript parse failed: ${err instanceof Error ? err.message : String(err)}`);
      }
    }
  }
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
  const repoRoot = opts.repoRoot;
  const sha = opts.sha;

  const fmt = "%H%x1f%P%x1f%ae%x1f%an%x1f%cI%x1f%s";
  let line: string;
  try {
    line = execSync(`git log -1 --format=${fmt} ${sha}`, {
      cwd: repoRoot,
      encoding: "utf8",
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

// Re-exported so other callers can use the safeGit helper inline.
export { computeCost };
export type { TokenCounts };

function safeGit(args: string[], cwd: string): string | null {
  try {
    return execSync(`git ${args.map((a) => JSON.stringify(a)).join(" ")}`, {
      cwd,
      encoding: "utf8",
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
