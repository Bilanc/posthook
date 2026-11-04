import { Panel } from "@/components/ui/panel";
import { fmtCompact, fmtNum, fmtPct } from "@/lib/format";
import { ORDINAL } from "@/lib/palette";
import type { Funnel, LeadTime } from "@/lib/queries/outcomes";
import { ratio, roundedEndRect } from "./series";

const W = 520;
const BH = 20;
const GAP = 10;
const LAB = 130; // room for the stage name
const TIP = 110; // room for the value at the bar tip

interface Stage {
  label: string;
  value: number;
  color: string;
}

// Two funnels: sessions by what they left behind, and lines generated vs
// committed. Each later stage is labelled with its share of the first.
export function Outcomes({ funnel, leadTime }: { funnel: Funnel; leadTime: LeadTime }) {
  const sessions: Stage[] = [
    { label: "Started", value: funnel.sessions, color: ORDINAL[0] },
    { label: "Edited a file", value: funnel.with_edits, color: ORDINAL[1] },
    { label: "Shipped a commit", value: funnel.with_commits, color: ORDINAL[2] },
  ];
  const lines: Stage[] = [
    { label: "Generated", value: funnel.lines_generated, color: ORDINAL[0] },
    { label: "Committed", value: funnel.lines_committed, color: ORDINAL[2] },
  ];
  const under = ratio(leadTime.under_15m, leadTime.total);
  const over = ratio(leadTime.over_1d, leadTime.total);

  return (
    <Panel title="Outcomes" sub="sessions started in window">
      <div className="text-xs text-[var(--color-fg-muted)] mb-1.5">Sessions</div>
      <Bars stages={sessions} unit="sessions" />
      <div className="text-xs text-[var(--color-fg-muted)] mt-3.5 mb-1.5">Lines</div>
      <Bars stages={lines} unit="lines" />
      {leadTime.total > 0 && under != null && over != null ? (
        <p className="mt-3 text-xs text-[var(--color-fg-muted)]">
          Time from an agent&apos;s last edit to the commit: under 15 minutes for {fmtPct(under)} of
          attributed commits, over a day for {fmtPct(over)}.
        </p>
      ) : null}
    </Panel>
  );
}

function Bars({ stages, unit }: { stages: Stage[]; unit: string }) {
  const max = stages[0]?.value ?? 0;
  const span = W - LAB - TIP;
  const height = stages.length * (BH + GAP) - GAP;
  return (
    <svg viewBox={`0 0 ${W} ${height}`} className="block w-full h-auto" role="img" aria-label={`${unit} funnel`}>
      {stages.map((s, i) => {
        const share = ratio(s.value, max);
        const w = share == null ? 0 : Math.max(s.value > 0 ? 4 : 0, Math.min(span, share * span));
        const y = i * (BH + GAP);
        const tip = share == null ? fmtCompact(s.value) : `${fmtCompact(s.value)}${i ? ` · ${fmtPct(share)}` : ""}`;
        return (
          <g key={s.label}>
            <text x={0} y={y + 14} className="text-[12px] fill-[var(--color-fg-2)]">
              {s.label}
            </text>
            {w > 0 ? (
              <path d={roundedEndRect(LAB, y, w, BH, 4, "right")} fill={s.color}>
                <title>{`${s.label}: ${fmtNum(s.value)} ${unit}`}</title>
              </path>
            ) : (
              <line x1={LAB} x2={LAB} y1={y} y2={y + BH} stroke="var(--color-axis)" strokeWidth={1} />
            )}
            <text x={(LAB + w + 8).toFixed(1)} y={y + 14} className="text-[12px] tabular-nums fill-[var(--color-fg-2)]">
              {tip}
            </text>
          </g>
        );
      })}
    </svg>
  );
}
