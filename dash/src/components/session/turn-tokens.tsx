import { Empty } from "@/components/ui/panel";
import { fmtCompact } from "@/lib/format";
import { ORDINAL } from "@/lib/palette";
import type { Turn } from "@/lib/turns";
import { niceCeil } from "./model";

const OUTPUT = ORDINAL[0]; // #3987e5
const THINKING = ORDINAL[2]; // #184f95
const W = 240;
const H = 90;
const ML = 30;
const MB = 16;
const TOP = 8;

// Column per turn: output tokens, with the thinking share drawn as the lower
// segment. When usage is unknown (Cursor, missing transcript) the caller
// passes the sentence to show instead.
export function TurnTokens({ turns, noUsage }: { turns: Turn[]; noUsage: string | null }) {
  if (noUsage) return <Empty>{noUsage}</Empty>;
  const n = Math.max(1, turns.length);
  const max = niceCeil(Math.max(1, ...turns.map((t) => t.output_tokens ?? 0)));
  const plotH = H - MB - TOP;
  const slot = (W - ML) / n;
  const bw = Math.max(1, Math.min(24, slot - 6));
  const baseline = H - MB;
  const px = (v: number) => (v / max) * plotH;
  const labelEvery = Math.max(1, Math.ceil((n * 14) / (W - ML)));

  return (
    <div>
      <svg viewBox={`0 0 ${W} ${H}`} className="block h-auto w-full" role="img" aria-label="Output tokens per turn">
        {[max / 2, max].map((g) => {
          const y = baseline - px(g);
          return (
            <g key={g}>
              <line x1={ML} x2={W} y1={y} y2={y} stroke="var(--color-grid)" strokeWidth={1} />
              <text x={ML - 4} y={y + 4} textAnchor="end" fontSize={11} fill="var(--color-fg-muted)">
                {fmtCompact(g)}
              </text>
            </g>
          );
        })}
        {turns.map((t, i) => {
          const out = t.output_tokens ?? 0;
          const think = Math.min(out, t.thinking_tokens ?? 0);
          const x = ML + i * slot + (slot - bw) / 2;
          const h = px(out);
          const th = px(think);
          const split = th >= 3 && h - th - 2 > 0;
          const tip = `Turn ${t.n}: ${fmtCompact(out)} output tokens${think ? `, ${fmtCompact(think)} thinking` : ""}\n${t.edits.length} edits`;
          return (
            <g key={t.n}>
              <title>{tip}</title>
              {h > 0 ? (
                split ? (
                  <>
                    <rect x={x} y={baseline - h} width={bw} height={h - th - 2} rx={2} fill={OUTPUT} />
                    <rect x={x} y={baseline - th} width={bw} height={th} fill={THINKING} />
                  </>
                ) : (
                  <rect x={x} y={baseline - h} width={bw} height={h} rx={2} fill={OUTPUT} />
                )
              ) : null}
              {i % labelEvery === 0 ? (
                <text x={x + bw / 2} y={H - 4} textAnchor="middle" fontSize={11} fill="var(--color-fg-muted)">
                  {t.n}
                </text>
              ) : null}
            </g>
          );
        })}
        <line x1={ML} x2={W} y1={baseline} y2={baseline} stroke="var(--color-axis)" strokeWidth={1} />
      </svg>
      <div className="mt-2 flex flex-wrap gap-x-3.5 gap-y-1.5 text-xs text-[var(--color-fg-2)]">
        <span className="inline-flex items-center gap-1.5">
          <i className="inline-block h-2.5 w-2.5 rounded-sm" style={{ background: OUTPUT }} />
          output
        </span>
        <span className="inline-flex items-center gap-1.5">
          <i className="inline-block h-2.5 w-2.5 rounded-sm" style={{ background: THINKING }} />
          of which thinking
        </span>
      </div>
    </div>
  );
}
