import Link from "next/link";
import { Panel } from "@/components/ui/panel";
import { fmtCompact, fmtDay, fmtMinutes, fmtNum } from "@/lib/format";
import { ACCENT, MUTED_MARK, agentColor } from "@/lib/palette";
import { IDLE_GAP_MINUTES, type DailyActivityRow } from "@/lib/queries/activity";
import type { SearchParams } from "@/lib/filters";
import {
  fmtDayShort,
  groupByDay,
  hrefWithMeasure,
  isMonday,
  niceMax,
  roundedEndRect,
  type Measure,
} from "./series";

const W = 720;
const H = 210;
const ML = 52;
const MR = 8;
const MT = 10;
const MB = 26;
const PW = W - ML - MR;
const PH = H - MT - MB;
const MIN_LABEL_GAP = 44; // px between x labels before we start skipping

// One column per day. "active": minutes with an agent working, stacked by
// agent. "lines": generated beside committed, grouped.
export function ActivityChart({
  rows,
  days,
  measure,
  sp,
}: {
  rows: DailyActivityRow[];
  days: string[];
  measure: Measure;
  sp: SearchParams;
}) {
  const toggle = (
    <span className="inline-flex rounded border border-[var(--color-border-2)] overflow-hidden text-[11px]">
      <ToggleLink href={hrefWithMeasure(sp, "active")} on={measure === "active"}>
        Active time
      </ToggleLink>
      <ToggleLink href={hrefWithMeasure(sp, "lines")} on={measure === "lines"}>
        Lines
      </ToggleLink>
    </span>
  );
  const sub = (
    <span className="inline-flex items-center gap-3">
      <span>
        {measure === "active"
          ? "minutes with an agent working, by agent"
          : "AI lines generated and committed, by day"}
      </span>
      {toggle}
    </span>
  );

  return (
    <Panel title={measure === "active" ? "Active time per day" : "AI lines per day"} sub={sub}>
      {measure === "active" ? <ActiveColumns rows={rows} days={days} /> : <LineColumns rows={rows} days={days} />}
      <p className="mt-2 text-[11px] text-[var(--color-fg-faint)]">
        {measure === "active"
          ? `Active time counts gaps between agent events of up to ${IDLE_GAP_MINUTES} minutes.`
          : "Committed lines are bucketed by commit date, so a day can commit more than it generated."}
      </p>
    </Panel>
  );
}

function ToggleLink({ href, on, children }: { href: string; on: boolean; children: string }) {
  return (
    <Link
      href={href}
      aria-current={on ? "true" : undefined}
      className={`px-2 py-0.5 ${
        on
          ? "bg-[var(--color-accent)] text-white"
          : "text-[var(--color-fg-muted)] hover:text-[var(--color-fg)]"
      }`}
    >
      {children}
    </Link>
  );
}

// Which day indexes get an x label: every day on short windows, otherwise
// Mondays, thinned so labels never collide.
function labelDays(days: string[], slot: number): Set<number> {
  const out = new Set<number>();
  if (days.length <= 14) {
    const every = Math.max(1, Math.ceil(MIN_LABEL_GAP / slot));
    days.forEach((_, i) => {
      if (i % every === 0) out.add(i);
    });
    return out;
  }
  let lastX = -Infinity;
  days.forEach((d, i) => {
    if (!isMonday(d)) return;
    const x = i * slot;
    if (x - lastX >= MIN_LABEL_GAP) {
      out.add(i);
      lastX = x;
    }
  });
  return out;
}

function Axes({ y, ticks, fmt }: { y: (v: number) => number; ticks: number[]; fmt: (v: number) => string }) {
  return (
    <>
      {ticks.map((t) => (
        <g key={t}>
          <line x1={ML} x2={W - MR} y1={y(t).toFixed(1)} y2={y(t).toFixed(1)} stroke="var(--color-grid)" strokeWidth={1} />
          <text
            x={ML - 6}
            y={(y(t) + 4).toFixed(1)}
            textAnchor="end"
            className="text-[11px] tabular-nums fill-[var(--color-fg-muted)]"
          >
            {t ? fmt(t) : "0"}
          </text>
        </g>
      ))}
      <line x1={ML} x2={W - MR} y1={y(0).toFixed(1)} y2={y(0).toFixed(1)} stroke="var(--color-axis)" strokeWidth={1} />
    </>
  );
}

function XLabels({ days, slot }: { days: string[]; slot: number }) {
  const show = labelDays(days, slot);
  return (
    <>
      {days.map((d, i) =>
        show.has(i) ? (
          <text
            key={d}
            x={(ML + i * slot + slot / 2).toFixed(1)}
            y={H - 8}
            textAnchor="middle"
            className="text-[11px] tabular-nums fill-[var(--color-fg-muted)]"
          >
            {fmtDayShort(d)}
          </text>
        ) : null,
      )}
    </>
  );
}

function ActiveColumns({ rows, days }: { rows: DailyActivityRow[]; days: string[] }) {
  const byDay = groupByDay(rows);
  const agents = [...new Set(rows.filter((r) => r.active_minutes > 0).map((r) => r.agent))].sort();
  const perDay = days.map((d) => {
    const m = new Map<string, number>();
    for (const r of byDay.get(d) ?? []) m.set(r.agent, (m.get(r.agent) ?? 0) + r.active_minutes);
    return m;
  });
  const dayTotal = perDay.map((m) => [...m.values()].reduce((a, b) => a + b, 0));
  const max = niceMax(Math.max(...dayTotal, 60));
  const y = (v: number) => MT + PH - (v / max) * PH;
  const slot = PW / Math.max(1, days.length);
  const bw = Math.max(1, Math.min(24, slot - Math.min(4, slot * 0.3)));

  return (
    <>
      <svg viewBox={`0 0 ${W} ${H}`} className="block w-full h-auto" role="img" aria-label="Active minutes per day by agent">
        <Axes y={y} ticks={[0, max / 2, max]} fmt={fmtMinutes} />
        {days.map((d, i) => {
          const x = ML + i * slot + (slot - bw) / 2;
          const tot = dayTotal[i];
          let acc = 0;
          const segs = agents
            .map((a) => ({ a, v: perDay[i].get(a) ?? 0 }))
            .filter((s) => s.v > 0);
          const title =
            `${fmtDay(d)}\n` +
            (segs.length ? segs.map((s) => `${s.a}: ${fmtMinutes(s.v)}`).join("\n") : "no activity");
          return (
            <g key={d}>
              {segs.map((s, si) => {
                const isTop = si === segs.length - 1;
                const top = y(acc + s.v);
                const bot = y(acc) - (acc > 0 ? 2 : 0);
                const hgt = Math.max(0, bot - top);
                acc += s.v;
                return isTop ? (
                  <path key={s.a} d={roundedEndRect(x, top, bw, hgt, 4, "top")} fill={agentColor(s.a)} />
                ) : (
                  <rect key={s.a} x={x.toFixed(1)} y={top.toFixed(1)} width={bw.toFixed(1)} height={hgt.toFixed(1)} fill={agentColor(s.a)} />
                );
              })}
              <rect x={(ML + i * slot).toFixed(1)} y={MT} width={slot.toFixed(1)} height={PH} fill="transparent">
                <title>{tot > 0 ? `${title}\ntotal: ${fmtMinutes(tot)}` : title}</title>
              </rect>
            </g>
          );
        })}
        <XLabels days={days} slot={slot} />
      </svg>
      {agents.length ? (
        <div className="mt-2 flex flex-wrap gap-x-3.5 gap-y-1.5 text-xs text-[var(--color-fg-2)]">
          {agents.map((a) => (
            <span key={a} className="inline-flex items-center gap-1.5">
              <i className="inline-block h-2.5 w-2.5 rounded-sm" style={{ background: agentColor(a) }} />
              {a}
            </span>
          ))}
        </div>
      ) : null}
    </>
  );
}

function LineColumns({ rows, days }: { rows: DailyActivityRow[]; days: string[] }) {
  const byDay = groupByDay(rows);
  const gen = days.map((d) => (byDay.get(d) ?? []).reduce((a, r) => a + r.lines_generated, 0));
  const com = days.map((d) => (byDay.get(d) ?? []).reduce((a, r) => a + r.lines_committed, 0));
  const max = niceMax(Math.max(...gen, ...com, 10));
  const y = (v: number) => MT + PH - (v / max) * PH;
  const slot = PW / Math.max(1, days.length);
  const inner = Math.max(2, Math.min(50, slot - Math.min(4, slot * 0.3)));
  const bw = Math.max(1, (inner - 2) / 2);
  const series = [
    { key: "generated", color: MUTED_MARK, vals: gen },
    { key: "committed", color: ACCENT, vals: com },
  ];

  return (
    <>
      <svg viewBox={`0 0 ${W} ${H}`} className="block w-full h-auto" role="img" aria-label="AI lines generated and committed per day">
        <Axes y={y} ticks={[0, max / 2, max]} fmt={fmtCompact} />
        {days.map((d, i) => {
          const x0 = ML + i * slot + (slot - inner) / 2;
          return (
            <g key={d}>
              {series.map((s, si) => {
                const v = s.vals[i];
                if (v <= 0) return null;
                const x = x0 + si * (bw + 2);
                const top = y(v);
                return <path key={s.key} d={roundedEndRect(x, top, bw, y(0) - top, 4, "top")} fill={s.color} />;
              })}
              <rect x={(ML + i * slot).toFixed(1)} y={MT} width={slot.toFixed(1)} height={PH} fill="transparent">
                <title>{`${fmtDay(d)}\ngenerated: ${fmtNum(gen[i])} lines\ncommitted: ${fmtNum(com[i])} lines`}</title>
              </rect>
            </g>
          );
        })}
        <XLabels days={days} slot={slot} />
      </svg>
      <div className="mt-2 flex flex-wrap gap-x-3.5 gap-y-1.5 text-xs text-[var(--color-fg-2)]">
        {series.map((s) => (
          <span key={s.key} className="inline-flex items-center gap-1.5">
            <i className="inline-block h-2.5 w-2.5 rounded-sm" style={{ background: s.color }} />
            {s.key}
          </span>
        ))}
      </div>
    </>
  );
}
