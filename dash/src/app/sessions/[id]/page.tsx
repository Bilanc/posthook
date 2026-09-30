import Link from "next/link";
import { notFound } from "next/navigation";
import { format, parseISO } from "date-fns";
import { PromptsList } from "@/components/prompts-list";
import {
  filesTouchedInSession,
  getSessionDetail,
  storedPromptsForSession,
  transcriptPathForSession,
  type SessionDetail,
} from "@/lib/queries/sessions";
import { commitsForSession } from "@/lib/queries/commits";
import { readPrompts } from "@/lib/transcript";

export const dynamic = "force-dynamic";

function fmtNum(n: number): string {
  return new Intl.NumberFormat("en-US").format(Math.round(n));
}

const compact = new Intl.NumberFormat("en-US", {
  notation: "compact",
  maximumFractionDigits: 1,
});

function fmtDateTime(iso: string | null): string {
  if (!iso) return "—";
  try {
    return format(parseISO(iso), "MMM d, yyyy HH:mm:ss");
  } catch {
    return iso;
  }
}

function fmtDuration(startedAt: string, endedAt: string | null): string {
  if (!endedAt) return "—";
  try {
    const ms = parseISO(endedAt).getTime() - parseISO(startedAt).getTime();
    if (ms <= 0) return "0m";
    const minutes = ms / 60000;
    if (minutes < 60) return `${minutes.toFixed(0)}m`;
    return `${(minutes / 60).toFixed(1)}h`;
  } catch {
    return "—";
  }
}

function commitGithubUrl(remoteUrl: string | null, sha: string): string | null {
  if (!remoteUrl) return null;
  // Match git@github.com:owner/repo.git or https://github.com/owner/repo(.git).
  const ssh = remoteUrl.match(/^git@github\.com:([^/]+)\/(.+?)(?:\.git)?$/);
  const https = remoteUrl.match(/^https:\/\/github\.com\/([^/]+)\/(.+?)(?:\.git)?$/);
  const m = ssh ?? https;
  if (!m) return null;
  return `https://github.com/${m[1]}/${m[2]}/commit/${sha}`;
}

export default async function SessionDetailPage({
  params,
}: {
  params: Promise<{ id: string }>;
}) {
  const { id } = await params;
  const session = getSessionDetail(id);
  if (!session) notFound();

  const commits = commitsForSession(id);
  const files = filesTouchedInSession(id);
  const transcriptPath = transcriptPathForSession(id);
  const transcriptPrompts = transcriptPath ? readPrompts(transcriptPath) : [];
  const prompts =
    transcriptPrompts.length > 0
      ? transcriptPrompts
      : storedPromptsForSession(id);

  return (
    <div>
      <Link
        href="/sessions"
        className="text-xs text-[var(--color-fg-muted)] hover:text-[var(--color-accent)]"
      >
        ← Back to sessions
      </Link>
      <h1 className="text-2xl font-semibold mt-2 mb-1">Session</h1>
      <div className="text-xs font-mono text-[var(--color-fg-muted)] mb-6 break-all">
        {session.id}
      </div>

      {/* Header KPIs */}
      <section className="grid grid-cols-2 md:grid-cols-4 lg:grid-cols-7 gap-3 mb-8">
        <Kpi label="Agent" value={session.agent_slug} />
        <Kpi label="Model" value={session.model_slug ?? "—"} />
        <Kpi
          label="Engineer"
          value={session.engineer_name || session.engineer_email || "—"}
        />
        <Kpi label="Repo" value={session.repo_name ?? "—"} />
        <Kpi label="Lines generated" value={fmtNum(session.lines_generated)} />
        <Kpi
          label="Duration"
          value={fmtDuration(session.started_at, session.ended_at)}
        />
        <Kpi
          label="Tokens in / out"
          value={
            session.input_tokens == null && session.output_tokens == null
              ? "—"
              : `${compact.format(session.input_tokens ?? 0)} / ${compact.format(session.output_tokens ?? 0)}`
          }
        />
      </section>

      {session.input_tokens != null ? <TokenBreakdown session={session} /> : null}

      <section className="mb-8 text-sm text-[var(--color-fg-muted)] grid grid-cols-1 md:grid-cols-2 gap-2">
        <div>
          <span className="uppercase text-xs tracking-wider mr-2">Started</span>
          <span className="text-[var(--color-fg)] tabular-nums">
            {fmtDateTime(session.started_at)}
          </span>
        </div>
        <div>
          <span className="uppercase text-xs tracking-wider mr-2">Ended</span>
          <span className="text-[var(--color-fg)] tabular-nums">
            {fmtDateTime(session.ended_at)}
          </span>
        </div>
      </section>

      {/* Three-panel grid */}
      <section className="grid grid-cols-1 lg:grid-cols-3 gap-4">
        <Panel title={`Prompts ${prompts.length > 0 ? `(${prompts.length})` : ""}`}>
          <PromptsList
            prompts={prompts}
            transcriptAvailable={transcriptPath !== null || prompts.length > 0}
          />
        </Panel>

        <Panel
          title={`Attributed commits${commits.length > 0 ? ` (${commits.length})` : ""}`}
        >
          {commits.length === 0 ? (
            <div className="text-sm text-[var(--color-fg-muted)]">
              No commits attributed to this session.
            </div>
          ) : (
            <ul className="space-y-3">
              {commits.map((c) => {
                const url = commitGithubUrl(session.repo_remote_url, c.sha);
                const shortSha = c.sha.slice(0, 7);
                const firstLine = (c.message ?? "").split("\n")[0] ?? "";
                return (
                  <li
                    key={c.id}
                    className="rounded border border-[var(--color-border)] bg-[var(--color-bg)] p-3"
                  >
                    <div className="flex items-center gap-2 text-xs font-mono">
                      {url ? (
                        <a
                          href={url}
                          target="_blank"
                          rel="noreferrer"
                          className="text-[var(--color-accent)] hover:underline"
                        >
                          {shortSha}
                        </a>
                      ) : (
                        <span className="text-[var(--color-fg-muted)]">{shortSha}</span>
                      )}
                      <span className="text-[var(--color-fg-muted)] tabular-nums">
                        {fmtDateTime(c.committed_at)}
                      </span>
                    </div>
                    <div className="mt-1 text-sm">{firstLine || "(no message)"}</div>
                    <div className="mt-1 text-xs text-[var(--color-fg-muted)] tabular-nums">
                      +{c.lines_added} / -{c.lines_removed} · {c.files_changed} file
                      {c.files_changed === 1 ? "" : "s"}
                    </div>
                  </li>
                );
              })}
            </ul>
          )}
        </Panel>

        <Panel
          title={`Files touched${files.length > 0 ? ` (${files.length})` : ""}`}
        >
          {files.length === 0 ? (
            <div className="text-sm text-[var(--color-fg-muted)]">
              No file edits captured.
            </div>
          ) : (
            <ul className="space-y-2 text-sm font-mono">
              {files.map((f) => (
                <li
                  key={f.rel_file_path}
                  className="flex items-baseline justify-between gap-3 border-b border-[var(--color-border)] pb-2 last:border-0"
                >
                  <span className="break-all">{f.rel_file_path}</span>
                  <span className="text-xs text-[var(--color-fg-muted)] tabular-nums whitespace-nowrap">
                    +{fmtNum(f.lines_generated)} · {f.edits} edit{f.edits === 1 ? "" : "s"}
                  </span>
                </li>
              ))}
            </ul>
          )}
        </Panel>
      </section>
    </div>
  );
}

function TokenBreakdown({
  session,
}: {
  session: Pick<
    SessionDetail,
    "input_tokens" | "output_tokens" | "cache_read_tokens" | "cache_creation_tokens" | "lines_generated"
  >;
}) {
  const input = session.input_tokens ?? 0;
  const output = session.output_tokens ?? 0;
  const cacheRead = session.cache_read_tokens ?? 0;
  const cacheWrite = session.cache_creation_tokens ?? 0;
  const total = input + output + cacheRead + cacheWrite;
  const prompt = input + cacheRead + cacheWrite;
  const hitRate = prompt > 0 ? (cacheRead / prompt) * 100 : null;
  const outPerLine = session.lines_generated > 0 ? output / session.lines_generated : null;
  const buckets = [
    { label: "Input", value: input, color: "#6366f1" },
    { label: "Output", value: output, color: "#14b8a6" },
    { label: "Cache read", value: cacheRead, color: "#0ea5e9" },
    { label: "Cache write", value: cacheWrite, color: "#f59e0b" },
  ];
  return (
    <section className="mb-8 rounded-lg border border-[var(--color-border)] bg-[var(--color-bg-elevated)] p-5">
      <div className="flex flex-wrap items-baseline justify-between gap-3 mb-3">
        <div className="text-xs uppercase tracking-wider text-[var(--color-fg-muted)]">
          Token usage
        </div>
        <div className="text-xs text-[var(--color-fg-muted)] tabular-nums">
          {fmtNum(total)} total · cache hit{" "}
          {hitRate == null ? "—" : `${hitRate.toFixed(1)}%`} ·{" "}
          {outPerLine == null ? "—" : outPerLine.toFixed(0)} output tokens / AI line
        </div>
      </div>
      <div className="flex h-3 w-full overflow-hidden rounded bg-[var(--color-bg)] mb-3">
        {buckets.map((b) =>
          b.value > 0 && total > 0 ? (
            <div
              key={b.label}
              style={{ width: `${(b.value / total) * 100}%`, background: b.color }}
              title={`${b.label}: ${fmtNum(b.value)}`}
            />
          ) : null,
        )}
      </div>
      <div className="grid grid-cols-2 md:grid-cols-4 gap-3">
        {buckets.map((b) => (
          <div key={b.label} className="rounded border border-[var(--color-border)] bg-[var(--color-bg)] p-3">
            <div className="flex items-center gap-1.5 text-[10px] uppercase tracking-wider text-[var(--color-fg-muted)]">
              <span className="inline-block h-2 w-2 rounded-sm" style={{ background: b.color }} />
              {b.label}
            </div>
            <div className="mt-1 text-sm font-medium tabular-nums">{fmtNum(b.value)}</div>
            <div className="text-[11px] text-[var(--color-fg-muted)] tabular-nums">
              {total > 0 ? `${((b.value / total) * 100).toFixed(1)}% of total` : "—"}
            </div>
          </div>
        ))}
      </div>
    </section>
  );
}

function Kpi({ label, value }: { label: string; value: string }) {
  return (
    <div className="rounded border border-[var(--color-border)] bg-[var(--color-bg-elevated)] p-3">
      <div className="text-[10px] uppercase tracking-wider text-[var(--color-fg-muted)]">
        {label}
      </div>
      <div className="mt-1 text-sm font-medium truncate" title={value}>
        {value}
      </div>
    </div>
  );
}

function Panel({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div className="rounded-lg border border-[var(--color-border)] bg-[var(--color-bg-elevated)] p-5">
      <div className="text-xs uppercase tracking-wider text-[var(--color-fg-muted)] mb-3">
        {title}
      </div>
      {children}
    </div>
  );
}
