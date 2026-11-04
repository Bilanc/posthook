import { fmtCompact, fmtMinutes, fmtMs, fmtNum, fmtTime, oneLine } from "@/lib/format";
import { ACCENT, COMMIT_MARK, FAMILY_COLOR, MUTED_MARK } from "@/lib/palette";
import type { SessionCommit, StopEvent } from "@/lib/queries/turns";
import { FAMILY_LABEL, FAMILY_ORDER } from "@/lib/tool-families";
import type { Turn } from "@/lib/turns";
import { layoutTimeline, ms } from "./model";

// One SVG: prompts, tool calls, Stops and commits on a shared x axis where any
// idle stretch over GAP_MS collapses to a fixed break. Turn bands link to the
// conversation below. Hover values are data-tip attributes read by the shared
// hover layer (ui/chart-tooltip); the markup stays server-rendered.

const GAP_MS = 8 * 60_000;
const GAP_W = 34;
const PAD = 6;
const H = 122;
const BAND_Y = 34;
const BAND_H = 26;
const AXIS_Y = BAND_Y + BAND_H + 12;
const COMPACTION = "#c98500";
const PROMPT_MARK = "var(--color-fg-2)";
// Ticks are 2 units wide; an invisible rect this wide around each one makes
// them hoverable. Where ticks crowd closer than this, the later one wins the
// overlap and each keeps its own spacing, no worse than the bare tick.
const HIT_W = 6;

export function SessionTimeline({
  startedAt,
  endedAt,
  turns,
  stops,
  commits,
}: {
  startedAt: string;
  endedAt: string | null;
  turns: Turn[];
  stops: StopEvent[];
  commits: SessionCommit[];
}) {
  const prompts = turns.flatMap((t) => t.prompts).filter((p) => p.ts != null);
  const tools = turns.flatMap((t) => t.tools);
  const eventTs = [
    ms(startedAt),
    ...tools.map((x) => ms(x.ts)),
    ...prompts.map((p) => ms(p.ts)),
    ...stops.map((s) => ms(s.ts)),
    ...commits.map((c) => ms(c.committed_at)),
  ].filter((t) => Number.isFinite(t));
  // A running session ends at its last event rather than at "now", so the
  // axis is not dominated by one idle break to the present.
  const endTs = endedAt ? ms(endedAt) : Math.max(...eventTs);
  const L = layoutTimeline([...eventTs, endTs], {
    width: 900,
    padL: PAD,
    padR: PAD,
    gapMs: GAP_MS,
    gapWidth: GAP_W,
  });
  const W = L.width;
  const X = (iso: string) => L.x(ms(iso));
  const running = endedAt == null;

  const famsPresent = FAMILY_ORDER.filter((f) => tools.some((x) => x.family === f));
  const hasCompaction = prompts.some((p) => p.kind === "compaction");
  const hasResend = prompts.some((p) => p.kind === "resend");
  const hasUser = prompts.some((p) => p.kind === "user");

  return (
    <div>
      <div className="overflow-x-auto">
        <svg
          viewBox={`0 0 ${W} ${H}`}
          className="block h-auto w-full"
          style={{ minWidth: Math.max(640, W > 900 ? W : 0) }}
          role="img"
          aria-label="Session timeline"
        >
          <line x1={PAD} x2={W - PAD} y1={AXIS_Y} y2={AXIS_Y} stroke="var(--color-axis)" strokeWidth={1} />

          {L.gaps.map((g, i) => {
            const cx = g.x + g.w / 2;
            const ly = AXIS_Y + 18 + (i % 2) * 13;
            const label = fmtMinutes(g.ms / 60_000);
            return (
              <g key={`gap-${i}`} data-tip={`idle ${label}`}>
                <rect x={g.x} y={BAND_Y} width={g.w} height={BAND_H} fill="var(--color-bg-2)" />
                <text x={cx} y={BAND_Y + BAND_H / 2 + 4} textAnchor="middle" fontSize={11} fill="var(--color-fg-faint)">
                  ⋯
                </text>
                <line x1={cx} x2={cx} y1={AXIS_Y + 2} y2={ly - 9} stroke="var(--color-grid)" strokeWidth={1} />
                <text x={cx} y={ly} textAnchor="middle" fontSize={11} fill="var(--color-fg-muted)">
                  {label}
                </text>
              </g>
            );
          })}

          {turns.map((t) => {
            const x0 = X(t.start);
            const x1 = Math.max(X(t.end), x0 + 4);
            const end = t.open && running ? "…" : fmtTime(t.end);
            const tokens = t.output_tokens == null ? "" : ` · ${fmtCompact(t.output_tokens)} output tokens`;
            const tip = `Turn ${t.n} · ${fmtTime(t.start)}–${end}\n${plural(t.tools.length, "tool call")} · ${plural(t.edits.length, "edit")}${tokens}`;
            return (
              // The link keeps its pointer cursor; [data-tip] resets it to the arrow.
              <a key={`turn-${t.n}`} href={`#turn-${t.n}`} data-tip={tip} style={{ cursor: "pointer" }}>
                <rect x={x0} y={BAND_Y} width={x1 - x0} height={BAND_H} rx={3} fill={ACCENT} opacity={0.16} />
                <text x={x0 + 3} y={12} fontSize={11} fill="var(--color-fg-2)">
                  {t.n}
                </text>
              </a>
            );
          })}

          {tools.map((x) => {
            const px = X(x.ts);
            const dur = x.duration_ms != null ? ` · ${fmtMs(x.duration_ms)}` : "";
            const detail = oneLine(x.file ?? x.summary ?? "", 80);
            const tip = `${fmtTime(x.ts)} ${x.tool_name ?? "tool"}${dur}${detail ? `\n${detail}` : ""}`;
            return (
              <g key={x.id} data-tip={tip}>
                <rect x={px - HIT_W / 2} y={BAND_Y + 3} width={HIT_W} height={BAND_H - 6} fill="transparent" />
                <rect x={px - 1} y={BAND_Y + 3} width={2} height={BAND_H - 6} fill={FAMILY_COLOR[x.family]} />
              </g>
            );
          })}

          {commits.map((c) => {
            const px = X(c.committed_at);
            const msg = oneLine((c.message ?? "").split("\n")[0] ?? "", 80);
            const tip = `commit ${c.sha.slice(0, 7)} · ${fmtTime(c.committed_at)}${msg ? `\n${msg}` : ""}\n${fmtNum(c.lines_attributed)} lines from this session`;
            return (
              <rect
                key={c.id}
                x={px - 5}
                y={AXIS_Y - 5}
                width={10}
                height={10}
                fill={COMMIT_MARK}
                transform={`rotate(45 ${px} ${AXIS_Y})`}
                data-tip={tip}
              />
            );
          })}

          {prompts.map((p) => {
            const px = X(p.ts as string);
            if (p.kind === "compaction") {
              return (
                <g key={`p-${p.seq}`} data-tip={`context compacted · ${fmtTime(p.ts)}`}>
                  <circle cx={px} cy={BAND_Y - 11} r={4} fill="var(--color-bg-elevated)" stroke={COMPACTION} strokeWidth={1.5} />
                  <text x={px + 8} y={BAND_Y - 7} fontSize={11} fill="var(--color-fg-muted)">
                    compacted
                  </text>
                </g>
              );
            }
            if (p.kind === "resend") {
              return (
                <circle
                  key={`p-${p.seq}`}
                  cx={px}
                  cy={BAND_Y - 11}
                  r={3}
                  fill={MUTED_MARK}
                  data-tip={`prompt re-sent · ${fmtTime(p.ts)}`}
                />
              );
            }
            return (
              <g key={`p-${p.seq}`} data-tip={`prompt · ${fmtTime(p.ts)}\n${oneLine(p.text, 90)}`}>
                <rect x={px - HIT_W / 2} y={BAND_Y - 8} width={HIT_W} height={8} fill="transparent" />
                <rect x={px - 1} y={BAND_Y - 6} width={2} height={6} fill={PROMPT_MARK} />
              </g>
            );
          })}

          <text x={PAD} y={H - 3} fontSize={11} fill="var(--color-fg-muted)">
            {fmtTime(startedAt)}
          </text>
          <text x={W - PAD} y={H - 3} textAnchor="end" fontSize={11} fill="var(--color-fg-muted)">
            {running ? "running" : fmtTime(endedAt)}
          </text>
        </svg>
      </div>

      <div className="mt-2 flex flex-wrap gap-x-3.5 gap-y-1.5 text-xs text-[var(--color-fg-2)]">
        {famsPresent.map((f) => (
          <LegendItem key={f} label={FAMILY_LABEL[f]}>
            <i className="inline-block h-2.5 w-2.5 rounded-sm" style={{ background: FAMILY_COLOR[f] }} />
          </LegendItem>
        ))}
        {commits.length > 0 ? (
          <LegendItem label="commit">
            <i className="mx-px inline-block h-2 w-2 rotate-45" style={{ background: COMMIT_MARK }} />
          </LegendItem>
        ) : null}
        {hasUser ? (
          <LegendItem label="prompt">
            <i className="mx-1 inline-block h-2.5 w-0.5" style={{ background: PROMPT_MARK }} />
          </LegendItem>
        ) : null}
        {hasCompaction ? (
          <LegendItem label="compaction">
            <i
              className="inline-block h-2.5 w-2.5 rounded-full bg-[var(--color-bg-elevated)]"
              style={{ border: `1.5px solid ${COMPACTION}` }}
            />
          </LegendItem>
        ) : null}
        {hasResend ? (
          <LegendItem label="re-sent prompt">
            <i className="mx-0.5 inline-block h-1.5 w-1.5 rounded-full" style={{ background: MUTED_MARK }} />
          </LegendItem>
        ) : null}
      </div>
    </div>
  );
}

function plural(n: number, word: string): string {
  return `${n} ${word}${n === 1 ? "" : "s"}`;
}

function LegendItem({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <span className="inline-flex items-center gap-1.5">
      {children}
      {label}
    </span>
  );
}
