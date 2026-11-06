import { ACCENT, MUTED_MARK } from "@/lib/palette";
import { fmtCompact, fmtNum, fmtPct } from "@/lib/format";
import { Empty } from "@/components/ui/panel";
import type { ModelMetrics } from "./model-metrics";

// Round a maximum up to a tidy step (1, 1.2, 1.5, 2, 2.5, 3, 4, 5, 6, 8 × 10^n)
// so the ticks read cleanly without leaving half the axis empty.
const NICE_STEPS = [1, 1.2, 1.5, 2, 2.5, 3, 4, 5, 6, 8, 10];
function nice(v: number): number {
  if (!(v > 0) || !Number.isFinite(v)) return 1;
  const p = 10 ** Math.floor(Math.log10(v));
  const f = v / p;
  const n = NICE_STEPS.find((s) => f <= s) ?? 10;
  return n * p;
}

const W = 520;
const ROW = 34;
const LABEL = 170;
const RIGHT = 60;
const SURFACE = "var(--color-bg-elevated)";
const MUTED = "var(--color-fg-muted)";

interface Props {
  rows: ModelMetrics[];
}

// Generated and committed lines per model on one axis. The gap between the
// two dots is what did not survive to a commit; the line is the thing to
// compare across rows.
export function Dumbbell({ rows }: Props) {
  if (rows.length === 0) return <Empty>No models in this range.</Empty>;

  // Label by model; when one model slug shows up under two agents, prefix the
  // agent so the rows stay tellable apart.
  const seen = new Map<string, number>();
  for (const m of rows) seen.set(m.row.model, (seen.get(m.row.model) ?? 0) + 1);
  const label = (m: ModelMetrics) =>
    (seen.get(m.row.model) ?? 0) > 1 ? `${m.row.agent}/${m.row.model}` : m.row.model;

  const max = nice(Math.max(...rows.map((m) => Math.max(m.row.lines_generated, m.row.lines_committed))));
  const x = (v: number) => LABEL + (Math.max(0, v) / max) * (W - LABEL - RIGHT);
  const axisY = rows.length * ROW + 6;
  const H = axisY + 18;
  const ticks = [0, max / 2, max];

  return (
    <div>
      <svg
        viewBox={`0 0 ${W} ${H}`}
        className="block h-auto w-full"
        role="img"
        aria-label="Generated versus committed lines per model"
        fontSize={11}
      >
        {ticks.map((t) => (
          <line
            key={t}
            x1={x(t).toFixed(1)}
            x2={x(t).toFixed(1)}
            y1={4}
            y2={axisY}
            stroke="var(--color-grid)"
            strokeWidth={1}
          />
        ))}
        {rows.map((m, i) => {
          const y = i * ROW + 18;
          const gen = m.row.lines_generated;
          const com = m.row.lines_committed;
          const xg = x(gen);
          const xc = x(com);
          const name = label(m);
          const genTip = `${name}\ngenerated ${fmtNum(gen)} lines`;
          const comTip = `${name}\ncommitted ${fmtNum(com)} lines${m.keep == null ? "" : ` · ${fmtPct(m.keep)} kept`}`;
          const bothTip = `${name}\ngenerated ${fmtNum(gen)} lines\ncommitted ${fmtNum(com)} lines${m.keep == null ? "" : ` · ${fmtPct(m.keep)} kept`}`;
          return (
            <g key={`${m.row.agent}/${m.row.model}`}>
              <text x={0} y={y + 4} fill="var(--color-fg-2)" className="font-mono">
                {name}
              </text>
              <line
                x1={Math.min(xg, xc).toFixed(1)}
                x2={Math.max(xg, xc).toFixed(1)}
                y1={y}
                y2={y}
                stroke="var(--color-axis)"
                strokeWidth={2}
                data-tip={bothTip}
              />
              {/* Invisible wide stroke so the connector is hoverable along its length. */}
              <line
                x1={Math.min(xg, xc).toFixed(1)}
                x2={Math.max(xg, xc).toFixed(1)}
                y1={y}
                y2={y}
                stroke="transparent"
                strokeWidth={12}
                data-tip={bothTip}
              />
              <circle cx={xg.toFixed(1)} cy={y} r={5} fill={MUTED_MARK} stroke={SURFACE} strokeWidth={2} data-tip={genTip} />
              <circle cx={xc.toFixed(1)} cy={y} r={5} fill={ACCENT} stroke={SURFACE} strokeWidth={2} data-tip={comTip} />
              <text x={(Math.max(xg, xc) + 10).toFixed(1)} y={y + 4} fill={MUTED}>
                {fmtCompact(gen)}
              </text>
            </g>
          );
        })}
        {ticks.map((t) => (
          <text key={t} x={x(t).toFixed(1)} y={axisY + 12} textAnchor="middle" fill={MUTED}>
            {fmtCompact(t)}
          </text>
        ))}
      </svg>
      <div className="mt-2 flex flex-wrap gap-x-3.5 gap-y-1.5 text-xs text-[var(--color-fg-2)]">
        <span>
          <i className="mr-1.5 inline-block h-2.5 w-2.5 rounded-full align-[-1px]" style={{ background: MUTED_MARK }} />
          generated
        </span>
        <span>
          <i className="mr-1.5 inline-block h-2.5 w-2.5 rounded-full align-[-1px]" style={{ background: ACCENT }} />
          committed
        </span>
      </div>
    </div>
  );
}
