import type { CommitFile, PromptEntry, SessionCommit, SessionFile } from "@/lib/queries/turns";
import type { SessionDetail } from "@/lib/queries/sessions";
import type { Turn } from "@/lib/turns";
import type { UsageEntry } from "@/lib/transcript";

// Pure helpers for the session viewer: everything that is arithmetic rather
// than markup lives here so it can be unit-tested under plain Node (hence the
// type-only imports; keep this module free of runtime dependencies).

// git@github.com:owner/repo.git or https://github.com/owner/repo(.git) → commit URL.
export function commitGithubUrl(remoteUrl: string | null, sha: string): string | null {
  if (!remoteUrl) return null;
  const ssh = remoteUrl.match(/^git@github\.com:([^/]+)\/(.+?)(?:\.git)?$/);
  const https = remoteUrl.match(/^https:\/\/github\.com\/([^/]+)\/(.+?)(?:\.git)?$/);
  const m = ssh ?? https;
  if (!m) return null;
  return `https://github.com/${m[1]}/${m[2]}/commit/${sha}`;
}

// "mcp__linear__create_issue" → "mcp:linear__cr…"; rows show the full name in a title.
export function shortToolName(name: string | null | undefined, max = 14): string {
  const n = (name ?? "tool").replace(/^mcp__/, "mcp:");
  return n.length > max ? `${n.slice(0, max - 1)}…` : n;
}

export function ms(iso: string | null | undefined): number {
  return iso ? new Date(iso).getTime() : NaN;
}

// ---- Files: generated vs committed -------------------------------------

export interface FileRow {
  path: string;
  generated: number; // lines the agent wrote, floored at the committed count
  committed: number; // lines from this session that landed in a commit
  edits: number;
}

export interface FileSummary {
  rows: FileRow[];
  generated: number; // Σ per-file max(generated, committed)
  committed: number; // Σ commit lines_attributed
}

// Union of the session's edited files and the files its commits attribute to
// it. Shell-written files only carry line ranges, so a file can show more
// committed than generated lines; the generated count is floored at committed.
export function fileRows(
  files: SessionFile[],
  commitFiles: CommitFile[],
  commits: SessionCommit[],
): FileSummary {
  // A file is a path in a repository: one session can write README.md in two
  // repositories, and those are two files. The repository name is shown in
  // front of the path only when the session's files span more than one, so a
  // single-repository session reads exactly as it always has.
  const repos = new Set<string>();
  for (const f of files) if (f.repo_id) repos.add(f.repo_id);
  for (const c of commitFiles) if (c.repo_id) repos.add(c.repo_id);
  const label = (path: string, repoId?: string | null, repoName?: string | null) =>
    repos.size > 1 && repoId ? `${repoName || repoId}/${path}` : path;
  const key = (path: string, repoId?: string | null) => `${repoId ?? ""}\u0000${path}`;

  const by = new Map<string, FileRow>();
  for (const f of files) {
    by.set(key(f.file, f.repo_id), {
      path: label(f.file, f.repo_id, f.repo_name),
      generated: f.lines_generated,
      committed: 0,
      edits: f.edits,
    });
  }
  for (const c of commitFiles) {
    const k = key(c.file_path, c.repo_id);
    const row = by.get(k) ?? { path: label(c.file_path, c.repo_id, c.repo_name), generated: 0, committed: 0, edits: 0 };
    row.committed += c.lines_attributed;
    by.set(k, row);
  }
  for (const r of by.values()) if (r.committed > r.generated) r.generated = r.committed;
  const rows = [...by.values()].sort(
    (a, b) => Math.max(b.generated, b.committed) - Math.max(a.generated, a.committed) || a.path.localeCompare(b.path),
  );
  return {
    rows,
    generated: rows.reduce((a, r) => a + r.generated, 0),
    committed: commits.reduce((a, c) => a + c.lines_attributed, 0),
  };
}

// ---- Header stats ---------------------------------------------------------

export interface SessionStats {
  wallMinutes: number | null; // null while the session is still running
  activeMinutes: number;
  prompts: number;
  turns: number;
  toolCalls: number;
  edits: number;
  files: number;
  commits: number;
  linesCommitted: number;
  linesGenerated: number;
  outputTokens: number | null;
}

export function sessionStats(
  session: Pick<SessionDetail, "started_at" | "ended_at" | "output_tokens">,
  turns: Turn[],
  files: FileSummary,
  commits: SessionCommit[],
  activeMinutes: number,
  usage: UsageEntry[] | null,
): SessionStats {
  const wall = session.ended_at ? (ms(session.ended_at) - ms(session.started_at)) / 60000 : null;
  const outputTokens =
    session.output_tokens ?? (usage ? usage.reduce((a, u) => a + u.output_tokens, 0) : null);
  return {
    wallMinutes: wall != null && Number.isFinite(wall) ? Math.max(0, wall) : null,
    activeMinutes,
    prompts: turns.reduce((a, t) => a + t.prompts.filter((p) => p.kind === "user").length, 0),
    turns: turns.length,
    toolCalls: turns.reduce((a, t) => a + t.tools.length, 0),
    edits: turns.reduce((a, t) => a + t.edits.length, 0),
    files: files.rows.length,
    commits: commits.length,
    linesCommitted: files.committed,
    linesGenerated: files.generated,
    outputTokens,
  };
}

// Seconds between a re-sent prompt and the prompt it duplicates, keyed by seq.
export function resendDelays(prompts: PromptEntry[]): Record<number, number> {
  const sorted = [...prompts].sort((a, b) => a.seq - b.seq);
  const out: Record<number, number> = {};
  sorted.forEach((p, i) => {
    const prev = sorted[i - 1];
    if (!prev || !p.ts || !prev.ts) return;
    const s = (ms(p.ts) - ms(prev.ts)) / 1000;
    if (Number.isFinite(s) && s >= 0) out[p.seq] = Math.round(s);
  });
  return out;
}

// ---- Long text ------------------------------------------------------------

// Cut text for a clamped bubble: at most `maxChars` or `maxLines`, whichever
// comes first, backing off to a whitespace so words stay whole. `tail` is ""
// when nothing was cut.
export function clampText(text: string, maxChars: number, maxLines: number): { head: string; tail: string } {
  let cut = text.length;
  if (text.length > maxChars) cut = maxChars;
  let idx = -1;
  let lines = 0;
  while (lines < maxLines) {
    idx = text.indexOf("\n", idx + 1);
    if (idx === -1) break;
    lines++;
  }
  if (idx !== -1 && lines === maxLines) cut = Math.min(cut, idx);
  if (cut >= text.length) return { head: text, tail: "" };
  const ws = Math.max(text.lastIndexOf(" ", cut), text.lastIndexOf("\n", cut));
  if (ws > cut * 0.6) cut = ws;
  const head = text.slice(0, cut).trimEnd();
  const tail = text.slice(cut).trim();
  return tail ? { head, tail } : { head: text, tail: "" };
}

// ---- Timeline with idle compression ----------------------------------------

export interface TimelineGap {
  x: number;
  w: number;
  ms: number;
}

export interface TimelineLayout {
  width: number;
  x: (ts: number) => number;
  gaps: TimelineGap[];
  activeMs: number;
  x0: number; // x of the first event
  x1: number; // x of the last event
}

export interface TimelineOptions {
  width: number; // target width; grows when the gap breaks alone would not fit
  padL: number;
  padR: number;
  gapMs: number; // idle longer than this collapses to one break
  gapWidth: number; // px per break
  minActivePx?: number; // room kept for active time when breaks dominate
}

// Positions timestamps on an x axis where any idle stretch longer than gapMs
// is drawn as a fixed-width break and the rest of the width is shared
// proportionally by active time. x(ts) interpolates between known events and
// clamps outside them.
export function layoutTimeline(timestamps: number[], opts: TimelineOptions): TimelineLayout {
  const ts = [...new Set(timestamps.filter((t) => Number.isFinite(t)))].sort((a, b) => a - b);
  const { padL, padR, gapMs, gapWidth } = opts;
  let active = 0;
  let gapCount = 0;
  for (let i = 1; i < ts.length; i++) {
    const g = ts[i] - ts[i - 1];
    if (g > gapMs) gapCount++;
    else active += g;
  }
  const width = Math.max(opts.width, padL + padR + gapCount * gapWidth + (opts.minActivePx ?? 240));
  const scale = (width - padL - padR - gapCount * gapWidth) / Math.max(active, 1);
  const map: Array<[number, number]> = [];
  const gaps: TimelineGap[] = [];
  let x = padL;
  ts.forEach((t, i) => {
    if (i > 0) {
      const g = t - ts[i - 1];
      if (g > gapMs) {
        gaps.push({ x, w: gapWidth, ms: g });
        x += gapWidth;
      } else {
        x += g * scale;
      }
    }
    map.push([t, x]);
  });
  const last = map.length ? map[map.length - 1] : null;
  const X = (t: number): number => {
    if (!map.length || !Number.isFinite(t)) return padL;
    if (t <= map[0][0]) return map[0][1];
    if (last && t >= last[0]) return last[1];
    let lo = 0;
    let hi = map.length - 1;
    while (hi - lo > 1) {
      const mid = (lo + hi) >> 1;
      if (map[mid][0] <= t) lo = mid;
      else hi = mid;
    }
    const [ta, xa] = map[lo];
    const [tb, xb] = map[hi];
    return tb === ta ? xa : xa + ((t - ta) / (tb - ta)) * (xb - xa);
  };
  return { width, x: X, gaps, activeMs: active, x0: map.length ? map[0][1] : padL, x1: last ? last[1] : padL };
}

// Round up to 1 / 2 / 5 × 10^k for a chart's top tick.
export function niceCeil(n: number): number {
  if (!(n > 0) || !Number.isFinite(n)) return 1;
  const p = Math.pow(10, Math.floor(Math.log10(n)));
  const m = n / p;
  const f = m <= 1 ? 1 : m <= 2 ? 2 : m <= 5 ? 5 : 10;
  return f * p;
}

// Turn / gap length: "42s" under a minute, then "12m", "1h 05m", "14h".
export function spanLabel(durationMs: number): string {
  if (!Number.isFinite(durationMs) || durationMs < 0) return "—";
  if (durationMs < 60_000) return `${Math.round(durationMs / 1000)}s`;
  const m = Math.round(durationMs / 60_000);
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  if (h >= 10) return `${h}h`;
  return `${h}h ${String(m % 60).padStart(2, "0")}m`;
}
