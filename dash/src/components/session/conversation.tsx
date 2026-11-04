import type { ReactNode } from "react";
import { fmtCompact, fmtMs, fmtNum, fmtPct, fmtTime } from "@/lib/format";
import { ACCENT, FAMILY_COLOR, MUTED_MARK } from "@/lib/palette";
import type { SessionCommit } from "@/lib/queries/turns";
import { FAMILY_LABEL, type ToolFamily } from "@/lib/tool-families";
import { turnDurationMs, type Turn, type TurnPrompt, type TurnToolCall } from "@/lib/turns";
import { clampText, commitGithubUrl, shortToolName, spanLabel } from "./model";

// The spine of the page: one <article> per turn, in time order. Tool calls
// fold under the prompt that caused them (native <details>, open on the first
// turn), edits and commits hang off the turn they happened in.

export interface ConversationProps {
  turns: Turn[];
  agent: string;
  running: boolean; // session has no ended_at
  committedFiles: string[];
  remoteUrl: string | null;
  hasUsage: boolean;
  resendDelays: Record<number, number>;
  orphanCommits: SessionCommit[]; // attributed commits that fell between turns
}

export function Conversation(props: ConversationProps) {
  const committed = new Set(props.committedFiles);
  return (
    <div className="grid min-w-0 grid-cols-[minmax(0,1fr)] gap-5">
      {props.turns.map((t) => (
        <TurnArticle key={t.n} turn={t} committed={committed} {...props} />
      ))}
      {props.orphanCommits.length > 0 ? (
        <section className="grid gap-2">
          <div className="text-xs text-[var(--color-fg-muted)]">Commits between turns</div>
          {props.orphanCommits.map((c) => (
            <CommitCard key={c.id} commit={c} remoteUrl={props.remoteUrl} />
          ))}
        </section>
      ) : null}
    </div>
  );
}

function TurnArticle({
  turn: t,
  committed,
  agent,
  running,
  remoteUrl,
  hasUsage,
  resendDelays,
}: ConversationProps & { turn: Turn; committed: Set<string> }) {
  const inProgress = t.open && running;
  return (
    <article id={`turn-${t.n}`} className="grid min-w-0 grid-cols-[minmax(0,1fr)] scroll-mt-4 gap-2">
      <header className="flex flex-wrap items-baseline gap-x-3.5 gap-y-1 text-xs text-[var(--color-fg-muted)] tabular-nums">
        <b className="font-mono text-[11px] font-semibold text-[var(--color-fg)]">TURN {t.n}</b>
        <span>
          {fmtTime(t.start)} → {inProgress ? "now" : fmtTime(t.end)}
        </span>
        <span>{inProgress ? "in progress" : spanLabel(turnDurationMs(t))}</span>
        <span>{t.tools.length} tool calls</span>
        <span>{t.edits.length} edits</span>
      </header>

      {t.prompts.map((p) =>
        p.kind === "user" ? (
          <UserBubble key={p.seq} prompt={p} />
        ) : (
          <SysRow key={p.seq} time={p.ts}>
            {p.kind === "compaction"
              ? "Context compacted; the conversation continued from a summary."
              : resendDelays[p.seq] != null
                ? `Prompt re-sent after ${resendDelays[p.seq]}s.`
                : "Prompt re-sent."}
          </SysRow>
        ),
      )}

      {t.tools.length > 0 ? <ToolsDetails turn={t} hasUsage={hasUsage} /> : null}
      {t.edits.length > 0 ? <EditChips edits={t.edits} committed={committed} /> : null}

      {t.open ? (
        <SysRow>{running ? "in progress" : "No Stop event recorded; the reply was not captured."}</SysRow>
      ) : t.reply ? (
        <ReplyBubble turn={t} agent={agent} />
      ) : null}

      {t.commits.map((c) => (
        <CommitCard key={c.id} commit={c} remoteUrl={remoteUrl} />
      ))}
    </article>
  );
}

function SysRow({ time, children }: { time?: string | null; children: ReactNode }) {
  return (
    <div className="border-l-2 border-[var(--color-border-2)] px-2.5 py-0.5 text-xs italic text-[var(--color-fg-muted)]">
      {children}
      {time ? <span className="ml-2 font-mono not-italic">{fmtTime(time)}</span> : null}
    </div>
  );
}

function Who({ children }: { children: ReactNode }) {
  return (
    <div className="mb-1 text-[11px] uppercase tracking-[0.06em] text-[var(--color-fg-muted)] tabular-nums">
      {children}
    </div>
  );
}

function UserBubble({ prompt }: { prompt: TurnPrompt }) {
  return (
    <div className="min-w-0 rounded-lg border border-[rgba(99,102,241,0.25)] bg-[var(--color-accent-soft)] px-3 py-2.5 text-[13px] leading-relaxed break-words [overflow-wrap:anywhere]">
      <Who>you · {fmtTime(prompt.ts)}</Who>
      <ClampedText text={prompt.text} maxChars={400} maxLines={6} />
    </div>
  );
}

function ReplyBubble({ turn: t, agent }: { turn: Turn; agent: string }) {
  const tokens =
    t.output_tokens != null && t.output_tokens > 0
      ? ` · ${fmtCompact(t.output_tokens)} out${t.thinking_tokens ? ` · ${fmtCompact(t.thinking_tokens)} thinking` : ""}`
      : "";
  return (
    <div className="min-w-0 rounded-lg border border-[var(--color-border)] bg-[var(--color-bg-2)] px-3 py-2.5 text-[13px] leading-relaxed text-[var(--color-fg-2)] break-words [overflow-wrap:anywhere]">
      <Who>
        {agent} · {fmtTime(t.end)}
        {tokens}
      </Who>
      <ClampedText text={t.reply ?? ""} maxChars={600} maxLines={8} />
    </div>
  );
}

// Long text folds behind a native <details>: the summary carries the visible
// head and the "show more" hint, the body the full text; `group-open:` swaps
// the hint without any client JS.
function ClampedText({ text, maxChars, maxLines }: { text: string; maxChars: number; maxLines: number }) {
  const { head, tail } = clampText(text, maxChars, maxLines);
  if (!tail) return <div className="whitespace-pre-wrap">{text}</div>;
  return (
    <details className="group">
      <summary className="cursor-pointer list-none [&::-webkit-details-marker]:hidden">
        <span className="whitespace-pre-wrap group-open:hidden">{head}</span>
        <span className="ml-1 text-[var(--color-fg-muted)] group-open:hidden">… show more</span>
        <span className="hidden text-[var(--color-fg-muted)] group-open:inline">show less</span>
      </summary>
      <div className="whitespace-pre-wrap">{text}</div>
    </details>
  );
}

function ToolsDetails({ turn: t, hasUsage }: { turn: Turn; hasUsage: boolean }) {
  const fams = (Object.entries(t.families) as Array<[ToolFamily, number]>).sort((a, b) => b[1] - a[1]);
  const meta: string[] = [];
  if (t.tool_ms > 0) meta.push(`${fmtMs(t.tool_ms)} in tools`);
  if (hasUsage) meta.push(`${fmtNum(t.model_messages)} model ${t.model_messages === 1 ? "message" : "messages"}`);
  return (
    <details className="tools rounded-lg border border-[var(--color-border)] bg-[var(--color-bg)]" open={t.n === 1}>
      <summary className="flex flex-wrap items-center gap-x-3 gap-y-1.5 px-3 py-2 text-xs text-[var(--color-fg-2)]">
        <span className="caret font-mono text-[var(--color-fg-muted)]">▸</span>
        {fams.map(([f, n]) => (
          <span key={f} className="inline-flex items-center gap-1.5">
            <i className="inline-block h-2 w-2 rounded-sm" style={{ background: FAMILY_COLOR[f] }} />
            {FAMILY_LABEL[f]} ×{n}
          </span>
        ))}
        {meta.length ? <span className="ml-auto text-[var(--color-fg-muted)] tabular-nums">{meta.join(" · ")}</span> : null}
      </summary>
      <ul className="border-t border-[var(--color-border)] py-1">
        {t.tools.map((x) => (
          <ToolRow key={x.id} call={x} />
        ))}
      </ul>
    </details>
  );
}

function ToolRow({ call: x }: { call: TurnToolCall }) {
  const raw =
    x.family === "edit"
      ? `${x.file ?? ""}${x.lines_generated ? `  +${fmtNum(x.lines_generated)}` : ""}`
      : (x.summary ?? x.file ?? "");
  const summary = raw.replace(/\s+/g, " ").trim();
  return (
    <li className="grid grid-cols-[42px_104px_minmax(0,1fr)_76px] items-baseline gap-2.5 px-3 py-[3px] text-xs">
      <span className="font-mono text-[11px] text-[var(--color-fg-muted)]">{fmtTime(x.ts)}</span>
      <span className="inline-flex min-w-0 items-center gap-1.5 text-[var(--color-fg-2)]" title={x.tool_name ?? undefined}>
        <i className="inline-block h-[7px] w-[7px] shrink-0 rounded-sm" style={{ background: FAMILY_COLOR[x.family] }} />
        <span className="truncate">{shortToolName(x.tool_name ?? (x.family === "edit" ? "edit" : null))}</span>
      </span>
      <span className="truncate font-mono text-[11px] text-[var(--color-fg-2)]" title={summary}>
        {summary}
      </span>
      <span className="text-right font-mono text-[11px] text-[var(--color-fg-muted)] whitespace-nowrap">
        {x.has_stderr ? <span className="text-[var(--color-warn)]">stderr </span> : null}
        {fmtMs(x.duration_ms)}
      </span>
    </li>
  );
}

function EditChips({ edits, committed }: { edits: TurnToolCall[]; committed: Set<string> }) {
  const byFile = new Map<string, number>();
  for (const e of edits) {
    const f = e.file ?? "(unknown file)";
    byFile.set(f, (byFile.get(f) ?? 0) + e.lines_generated);
  }
  return (
    <div className="flex flex-wrap gap-1.5">
      {[...byFile].map(([f, lines]) => {
        const ok = committed.has(f);
        return (
          <span
            key={f}
            className="inline-flex max-w-full items-center gap-1.5 rounded border border-[var(--color-border-2)] px-1.5 py-px font-mono text-[11px] text-[var(--color-fg-2)]"
            title={ok ? `${f} — in a commit attributed to this session` : `${f} — not committed`}
          >
            <i
              className="inline-block h-[7px] w-[7px] shrink-0 rounded-full"
              style={ok ? { background: ACCENT, border: `1px solid ${ACCENT}` } : { border: `1px solid ${MUTED_MARK}` }}
            />
            <span className="truncate">{f}</span>
            <b className="font-medium text-[var(--color-good)]">+{fmtNum(lines)}</b>
          </span>
        );
      })}
    </div>
  );
}

function CommitCard({ commit: c, remoteUrl }: { commit: SessionCommit; remoteUrl: string | null }) {
  const url = commitGithubUrl(remoteUrl, c.sha);
  const sha = c.sha.slice(0, 7);
  const first = (c.message ?? "").split("\n")[0]?.trim() ?? "";
  const share = fmtPct(Math.min(1, c.lines_attributed / Math.max(1, c.lines_added)));
  return (
    <div className="grid grid-cols-[14px_minmax(0,1fr)] items-start gap-2.5 rounded-lg border border-[rgba(99,102,241,0.45)] bg-[rgba(99,102,241,0.08)] px-3 py-2 text-[13px]">
      <i className="mt-[5px] block h-2.5 w-2.5 rotate-45 bg-[var(--color-accent)]" aria-hidden />
      <div className="min-w-0">
        <span className="font-mono text-[#c7c8fb]">
          {url ? (
            <a href={url} target="_blank" rel="noreferrer" className="hover:underline">
              {sha}
            </a>
          ) : (
            sha
          )}
        </span>{" "}
        <span className="text-[var(--color-fg)] break-words">{first || "(no message)"}</span>
        <div className="mt-0.5 text-xs text-[var(--color-fg-muted)] tabular-nums">
          {fmtTime(c.committed_at)} · +{fmtNum(c.lines_added)} −{fmtNum(c.lines_removed)} in {fmtNum(c.files_changed)}{" "}
          {c.files_changed === 1 ? "file" : "files"} ·{" "}
          <b className="font-medium text-[var(--color-fg)]">{fmtNum(c.lines_attributed)} lines from this session</b>{" "}
          ({share} of added)
        </div>
      </div>
    </div>
  );
}
