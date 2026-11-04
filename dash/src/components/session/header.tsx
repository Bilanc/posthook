import Link from "next/link";
import { Chip } from "@/components/ui/panel";
import { fmtCompact, fmtDateTime, fmtMinutes, fmtNum, fmtPct, oneLine } from "@/lib/format";
import { agentColor } from "@/lib/palette";
import type { SessionDetail } from "@/lib/queries/sessions";
import type { Turn } from "@/lib/turns";
import type { SessionStats } from "./model";

export function SessionHeader({
  session,
  turns,
  stats,
}: {
  session: SessionDetail;
  turns: Turn[];
  stats: SessionStats;
}) {
  const firstUser = turns.flatMap((t) => t.prompts).find((p) => p.kind === "user");
  const title = firstUser ? oneLine(firstUser.text, 110) : session.id;
  const effort = turns.find((t) => t.effort)?.effort ?? null;
  const mode = turns.find((t) => t.permission_mode)?.permission_mode ?? null;
  const repo = session.repo_name
    ? `${session.repo_name}${session.branch ? ` · ${session.branch}` : ""}`
    : session.branch ?? null;
  const keep = stats.linesGenerated > 0 ? Math.min(1, stats.linesCommitted / stats.linesGenerated) : null;

  return (
    <div className="grid gap-2">
      <Link
        href="/sessions"
        className="text-xs text-[var(--color-fg-muted)] hover:text-[var(--color-accent)] w-fit"
      >
        ← Back to sessions
      </Link>
      <h1 className="text-[17px] font-semibold tracking-[-0.01em] leading-snug break-words" title={firstUser?.text}>
        {title}
      </h1>
      <div className="flex flex-wrap gap-1.5">
        <Chip dot={agentColor(session.agent_slug)}>{session.agent_slug}</Chip>
        {session.model_slug ? <Chip mono>{session.model_slug}</Chip> : null}
        {effort ? <Chip>effort {effort}</Chip> : null}
        {mode ? <Chip>{mode} mode</Chip> : null}
        {repo ? <Chip title={session.repo_remote_url ?? undefined}>{repo}</Chip> : null}
        <Chip>{fmtDateTime(session.started_at)}</Chip>
        <Chip mono title={session.id}>
          <span className="text-[var(--color-fg-muted)]">{session.id.slice(0, 8)}</span>
        </Chip>
      </div>
      <div className="flex flex-wrap gap-x-5 gap-y-1 text-[13px] text-[var(--color-fg-2)] tabular-nums">
        <Stat v={stats.wallMinutes == null ? "running" : fmtMinutes(stats.wallMinutes)} l="wall" />
        <Stat v={fmtMinutes(stats.activeMinutes)} l="active" />
        <Stat v={fmtNum(stats.prompts)} l={stats.prompts === 1 ? "prompt" : "prompts"} />
        <Stat v={fmtNum(stats.turns)} l={stats.turns === 1 ? "turn" : "turns"} />
        <Stat v={fmtNum(stats.toolCalls)} l={stats.toolCalls === 1 ? "tool call" : "tool calls"} />
        <Stat
          v={fmtNum(stats.edits)}
          l={`${stats.edits === 1 ? "edit" : "edits"} in ${fmtNum(stats.files)} ${stats.files === 1 ? "file" : "files"}`}
        />
        <Stat v={fmtNum(stats.commits)} l={stats.commits === 1 ? "commit" : "commits"} />
        <Stat
          v={fmtNum(stats.linesCommitted)}
          l={`of ${fmtNum(stats.linesGenerated)} lines committed${keep == null ? "" : ` · ${fmtPct(keep)}`}`}
        />
        <Stat v={stats.outputTokens == null ? "—" : fmtCompact(stats.outputTokens)} l="output tokens" />
      </div>
    </div>
  );
}

function Stat({ v, l }: { v: string; l: string }) {
  return (
    <span className="whitespace-nowrap">
      <b className="font-semibold text-[var(--color-fg)]">{v}</b> {l}
    </span>
  );
}
