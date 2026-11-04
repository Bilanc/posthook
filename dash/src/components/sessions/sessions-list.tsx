import Link from "next/link";
import { format, parseISO } from "date-fns";
import type { SessionListRow } from "@/lib/queries/sessions";
import { fmtDay, fmtMinutes, fmtNum, fmtTime, oneLine } from "@/lib/format";
import { Empty } from "@/components/ui/panel";
import { ActivityStrip } from "./activity-strip";
import { OutcomeChip } from "./outcome-chip";

// Row grid: start · session · activity · outcome · prompts/active · gen→committed.
// Under 860px the strip and the numeric columns drop out.
const GRID =
  "grid grid-cols-[48px_1fr_110px] min-[860px]:grid-cols-[48px_1fr_190px_120px_90px_110px] gap-3 items-center px-2.5";
const WIDE = "hidden min-[860px]:block";
const NUM = `${WIDE} text-right tabular-nums text-[var(--color-fg-2)]`;
const SEP = "text-[var(--color-fg-muted)]";

function localDay(iso: string): string {
  try {
    return format(parseISO(iso), "yyyy-MM-dd");
  } catch {
    return iso.slice(0, 10);
  }
}

function Hd({ children, className = "" }: { children: React.ReactNode; className?: string }) {
  return (
    <span
      className={`text-[11px] uppercase tracking-wider text-[var(--color-fg-muted)] ${className}`}
    >
      {children}
    </span>
  );
}

function DayDivider({ day }: { day: string }) {
  return (
    <div className="px-2.5 pt-3 pb-1.5 text-[11px] uppercase tracking-[0.08em] text-[var(--color-fg-muted)]">
      {fmtDay(day).toUpperCase()}
    </div>
  );
}

// "agent · model · repo · branch". Each token is unbreakable so a narrow column
// wraps between tokens, never inside a hyphenated slug.
function Meta({ tokens }: { tokens: Array<string | null> }) {
  const parts = tokens.filter((t): t is string => !!t);
  return (
    <div className="mt-0.5 flex flex-wrap gap-x-1.5 text-xs text-[var(--color-fg-muted)]">
      {parts.map((t, i) => (
        <span key={i} className="whitespace-nowrap">
          {i > 0 ? <span className="mr-1.5">·</span> : null}
          {t}
        </span>
      ))}
    </div>
  );
}

function Row({ r }: { r: SessionListRow }) {
  const prompt = r.first_prompt ? oneLine(r.first_prompt, 90) : null;
  return (
    <div
      className={`${GRID} relative border-t border-[var(--color-border)] py-[9px] text-[13px] hover:bg-[var(--color-bg-2)]`}
    >
      <span className="tabular-nums text-[var(--color-fg-muted)]">{fmtTime(r.started_at)}</span>
      <div className="min-w-0">
        {/* The link's pseudo-element stretches over the row so the whole row is the target. */}
        <Link
          href={`/sessions/${r.id}`}
          title={r.first_prompt ? oneLine(r.first_prompt, 500) : r.id}
          className={`block truncate after:absolute after:inset-0 after:content-[''] ${
            prompt ? "text-[var(--color-fg)]" : "italic text-[var(--color-fg-muted)]"
          }`}
        >
          {prompt ?? "(no prompt captured)"}
        </Link>
        <Meta
          tokens={[r.agent_slug, r.model_slug ?? "—", r.repo_name ?? "no repo", r.branch]}
        />
      </div>
      {/* Raised above the stretched link so the strip's <title> still shows on hover. */}
      <span className={`${WIDE} relative z-10 leading-none`}>
        <ActivityStrip
          startedAt={r.started_at}
          endedAt={r.ended_at}
          stopTs={r.stop_ts}
          editTs={r.edit_ts}
          commitTs={r.commit_ts}
        />
      </span>
      <span>
        <OutcomeChip outcome={r.outcome} commits={r.commits_attributed} />
      </span>
      <span className={NUM}>
        {fmtNum(r.prompts)} <span className={SEP}>·</span> {fmtMinutes(r.active_minutes)}
      </span>
      <span className={NUM}>
        {fmtNum(r.lines_generated)} <span className={SEP}>→</span> {fmtNum(r.lines_committed)}
      </span>
    </div>
  );
}

export function SessionsList({
  rows,
  emptyMessage = "No sessions match the current filters.",
}: {
  rows: SessionListRow[];
  emptyMessage?: string;
}) {
  if (rows.length === 0) {
    return (
      <div className="rounded-lg border border-[var(--color-border)] bg-[var(--color-bg-elevated)] p-8 text-center">
        <Empty>{emptyMessage}</Empty>
      </div>
    );
  }

  // Rows arrive newest first; a divider opens each new local day.
  const items: React.ReactNode[] = [];
  let day: string | null = null;
  for (const r of rows) {
    const d = localDay(r.started_at);
    if (d !== day) {
      day = d;
      items.push(<DayDivider key={`day-${d}`} day={d} />);
    }
    items.push(<Row key={r.id} r={r} />);
  }

  return (
    <div className="rounded-lg border border-[var(--color-border)] bg-[var(--color-bg-elevated)] px-1.5 py-1">
      <div className={`${GRID} py-2`}>
        <Hd>Start</Hd>
        <Hd>Session</Hd>
        <Hd className={WIDE}>Activity</Hd>
        <Hd>Outcome</Hd>
        <Hd className={`${WIDE} text-right`}>Prompts · active</Hd>
        <Hd className={`${WIDE} text-right`}>Lines gen → committed</Hd>
      </div>
      {items}
    </div>
  );
}
