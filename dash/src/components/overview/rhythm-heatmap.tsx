import { Empty, Panel } from "@/components/ui/panel";
import { fmtNum } from "@/lib/format";
import { SEQUENTIAL } from "@/lib/palette";
import type { HeatCell } from "@/lib/queries/heat";
import { plural } from "./series";

const ROWS = [1, 2, 3, 4, 5, 6, 0]; // Mon..Sun in strftime('%w') numbering
const NAMES = ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"];
const CW = 12;
const CH = 14;
const GAP = 2;
const ML = 30;
const MT = 16;
const W = ML + 24 * (CW + GAP);
const H = MT + 7 * (CH + GAP);
const RAMP = SEQUENTIAL.slice(1); // step 0 is reserved for empty cells

const hh = (h: number) => String(h).padStart(2, "0");

// Weekday × hour of AI edits: when the agent is in the loop.
export function RhythmHeatmap({ cells }: { cells: HeatCell[] }) {
  const m = new Map<string, number>();
  let max = 0;
  for (const c of cells) {
    m.set(`${c.dow}-${c.hour}`, c.n);
    if (c.n > max) max = c.n;
  }

  return (
    <Panel title="Rhythm" sub="AI edits in window">
      {max === 0 ? (
        <Empty>No AI edits in this range.</Empty>
      ) : (
        <>
          <svg viewBox={`0 0 ${W} ${H}`} className="block w-full h-auto" role="img" aria-label="AI edits by weekday and hour">
            {[0, 6, 12, 18].map((h) => (
              <text key={h} x={ML + h * (CW + GAP)} y={10} className="text-[11px] tabular-nums fill-[var(--color-fg-muted)]">
                {hh(h)}
              </text>
            ))}
            {ROWS.map((dow, ri) => (
              <g key={dow}>
                <text
                  x={ML - 6}
                  y={MT + ri * (CH + GAP) + 11}
                  textAnchor="end"
                  className="text-[11px] fill-[var(--color-fg-muted)]"
                >
                  {NAMES[ri]}
                </text>
                {Array.from({ length: 24 }, (_, h) => {
                  const v = m.get(`${dow}-${h}`) ?? 0;
                  const step = v ? Math.min(RAMP.length - 1, Math.floor((v / max) * RAMP.length)) : -1;
                  return (
                    <rect
                      key={h}
                      x={ML + h * (CW + GAP)}
                      y={MT + ri * (CH + GAP)}
                      width={CW}
                      height={CH}
                      rx={2}
                      fill={step < 0 ? "var(--color-bg-2)" : RAMP[step]}
                    >
                      <title>{`${NAMES[ri]} ${hh(h)}:00–${hh(h + 1)}:00 · ${fmtNum(v)} AI ${plural(v, "edit")}`}</title>
                    </rect>
                  );
                })}
              </g>
            ))}
          </svg>
          <div className="mt-2 flex items-center gap-1 text-xs text-[var(--color-fg-2)]">
            <span className="mr-1">fewer</span>
            {RAMP.map((c) => (
              <i key={c} className="inline-block h-2.5 w-2.5 rounded-sm" style={{ background: c }} />
            ))}
            <span className="ml-1">more</span>
          </div>
        </>
      )}
    </Panel>
  );
}
