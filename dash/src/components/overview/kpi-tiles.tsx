import type { ReactNode } from "react";
import { fmtMinutes, fmtNum, fmtPct } from "@/lib/format";
import { ACCENT } from "@/lib/palette";
import type { ActivityTotals, DailyActivityRow } from "@/lib/queries/activity";
import type { Funnel } from "@/lib/queries/outcomes";
import { ratio, sumByDay } from "./series";

// Five stat tiles: value, change against the prior window, and a one-point-
// per-day sparkline. Shipped sessions has no prior comparison; it carries the
// share that reached a commit instead.
export function KpiTiles({
  rows,
  days,
  totals,
  prior,
  funnel,
}: {
  rows: DailyActivityRow[];
  days: string[];
  totals: ActivityTotals;
  prior: ActivityTotals | null;
  funnel: Funnel;
}) {
  const keep = ratio(totals.lines_committed, totals.lines_generated);
  const priorKeep = prior ? ratio(prior.lines_committed, prior.lines_generated) : null;
  const gen = sumByDay(rows, days, (r) => r.lines_generated);
  const com = sumByDay(rows, days, (r) => r.lines_committed);
  const keepSeries = days.map((_, i) => {
    const r = ratio(com[i], gen[i]);
    return r == null ? 0 : Math.min(1, r);
  });
  const shipped = ratio(funnel.with_commits, funnel.sessions);

  return (
    <div className="grid gap-3.5 grid-cols-[repeat(auto-fit,minmax(150px,1fr))]">
      <Tile
        label="Sessions"
        value={fmtNum(totals.sessions)}
        foot={<Delta cur={totals.sessions} prev={prior?.sessions ?? null} fmt={fmtNum} />}
        series={sumByDay(rows, days, (r) => r.sessions)}
      />
      <Tile
        label="Active time"
        value={fmtMinutes(totals.active_minutes)}
        foot={
          <Delta cur={totals.active_minutes} prev={prior?.active_minutes ?? null} fmt={fmtMinutes} />
        }
        series={sumByDay(rows, days, (r) => r.active_minutes)}
      />
      <Tile
        label="Prompts"
        value={fmtNum(totals.prompts)}
        foot={<Delta cur={totals.prompts} prev={prior?.prompts ?? null} fmt={fmtNum} />}
        series={sumByDay(rows, days, (r) => r.prompts)}
      />
      <Tile
        label="Shipped sessions"
        value={
          <>
            {fmtNum(funnel.with_commits)}{" "}
            <span className="text-[13px] font-normal text-[var(--color-fg-muted)]">
              of {fmtNum(funnel.sessions)}
            </span>
          </>
        }
        foot={
          <span className="text-[var(--color-fg-muted)]">
            {shipped == null ? "no sessions" : `${fmtPct(shipped)} reached a commit`}
          </span>
        }
        series={sumByDay(rows, days, (r) => r.commits)}
      />
      <Tile
        label="Keep rate"
        value={fmtPct(keep)}
        foot={<Delta cur={keep} prev={priorKeep} fmt={(v) => fmtPct(v)} pts />}
        series={keepSeries}
      />
    </div>
  );
}

function Tile({
  label,
  value,
  foot,
  series,
}: {
  label: string;
  value: ReactNode;
  foot: ReactNode;
  series: number[];
}) {
  return (
    <div className="rounded-lg border border-[var(--color-border)] bg-[var(--color-bg-elevated)] p-4 min-w-0">
      <div className="text-[11px] uppercase tracking-wider text-[var(--color-fg-muted)] font-medium">
        {label}
      </div>
      <div className="mt-1 text-2xl font-semibold leading-tight">{value}</div>
      <div className="mt-1 text-xs flex items-center gap-1.5 min-h-4">{foot}</div>
      <Sparkline values={series} />
    </div>
  );
}

// Change vs the prior window. Percent of the prior value, or percentage
// points when `pts` is set (rates). Under 2% either way reads as flat.
function Delta({
  cur,
  prev,
  fmt,
  pts = false,
}: {
  cur: number | null;
  prev: number | null;
  fmt: (n: number) => string;
  pts?: boolean;
}) {
  if (cur == null || prev == null || prev === 0 || !Number.isFinite(prev) || !Number.isFinite(cur)) {
    return <span className="text-[var(--color-fg-muted)]">no prior data</span>;
  }
  const ch = pts ? cur - prev : (cur - prev) / prev;
  const flat = Math.abs(ch) < 0.02;
  const mag = Math.abs(Math.round(ch * 100));
  const txt = pts ? `${mag} pts` : `${mag}%`;
  const sym = flat ? "•" : ch > 0 ? "▲" : "▼";
  const color = flat
    ? "text-[var(--color-fg-muted)]"
    : ch > 0
      ? "text-[var(--color-good)]"
      : "text-[var(--color-bad)]";
  return (
    <>
      <span className={color}>
        {sym} {txt}
      </span>
      <span className="text-[var(--color-fg-muted)]">vs {fmt(prev)}</span>
    </>
  );
}

// One point per day; the area wash and end dot mark the current period.
// Scales uniformly with the tile width so the dot stays round.
function Sparkline({ values }: { values: number[] }) {
  const w = 160;
  const h = 32;
  const n = values.length;
  if (n === 0) return <div className="mt-2 h-8" aria-hidden="true" />;
  const mx = Math.max(...values, 1);
  const pts = values.map((v, i) => {
    const x = n === 1 ? w / 2 : 2 + (i / (n - 1)) * (w - 4);
    const y = h - 3 - (Math.max(0, v) / mx) * (h - 8);
    return [x, y] as const;
  });
  const line = pts.map(([x, y], i) => `${i ? "L" : "M"}${x.toFixed(1)} ${y.toFixed(1)}`).join(" ");
  const last = pts[pts.length - 1];
  const first = pts[0];
  const area = `${line} L${last[0].toFixed(1)} ${h} L${first[0].toFixed(1)} ${h} Z`;
  return (
    <svg
      viewBox={`0 0 ${w} ${h}`}
      className="mt-2 block w-full h-auto"
      aria-hidden="true"
      focusable="false"
    >
      {n > 1 ? (
        <>
          <path d={area} fill={ACCENT} opacity={0.12} />
          <path
            d={line}
            fill="none"
            stroke={ACCENT}
            strokeWidth={1.5}
            strokeLinejoin="round"
            strokeLinecap="round"
            vectorEffect="non-scaling-stroke"
          />
        </>
      ) : null}
      <circle
        cx={last[0].toFixed(1)}
        cy={last[1].toFixed(1)}
        r={3}
        fill={ACCENT}
        stroke="var(--color-bg-elevated)"
        strokeWidth={2}
        vectorEffect="non-scaling-stroke"
      />
    </svg>
  );
}
