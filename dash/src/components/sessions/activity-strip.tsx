import { ACCENT, COMMIT_MARK } from "@/lib/palette";

// The session's shape on a linear time axis: one block per turn (previous
// Stop → this Stop), filled when the turn edited a file and hollow when it only
// read or ran commands; a diamond per attributed commit. Server-rendered SVG;
// the <title> is the hover text.
const W = 180;
const H = 16;
const MIN_BLOCK = 3;

interface Props {
  startedAt: string;
  endedAt: string | null;
  stopTs: string[];
  editTs: string[];
  commitTs: string[];
}

function ms(iso: string | null | undefined): number | null {
  if (!iso) return null;
  const t = Date.parse(iso);
  return Number.isFinite(t) ? t : null;
}

function toMs(list: string[]): number[] {
  return list.map(ms).filter((t): t is number => t != null).sort((a, b) => a - b);
}

function plural(n: number, word: string): string {
  return `${n} ${word}${n === 1 ? "" : "s"}`;
}

export function ActivityStrip({ startedAt, endedAt, stopTs, editTs, commitTs }: Props) {
  const stops = toMs(stopTs);
  const edits = toMs(editTs);
  const commits = toMs(commitTs);
  const label = `${plural(stops.length, "turn")} · ${plural(edits.length, "edit")} · ${plural(commits.length, "commit")}`;

  const t0 = ms(startedAt);
  const track = <rect x={0} y={6} width={W} height={4} rx={2} fill="var(--color-bg-2)" />;
  if (t0 == null) {
    return (
      <svg viewBox={`0 0 ${W} ${H}`} width={W} height={H} role="img" aria-label={label}>
        <title>{label}</title>
        {track}
      </svg>
    );
  }

  // Span: ended_at, at least a minute, and never shorter than the last turn
  // (sessions still running have no ended_at yet).
  const t1 = Math.max(ms(endedAt) ?? 0, t0 + 60_000, stops[stops.length - 1] ?? 0);
  const x = (t: number) => Math.min(W, Math.max(0, ((t - t0) / (t1 - t0)) * W));

  const blocks: Array<{ left: number; width: number; edited: boolean }> = [];
  let prev = t0;
  for (const st of stops) {
    const x0 = x(prev);
    const width = Math.max(MIN_BLOCK, x(st) - x0);
    const left = Math.min(x0, W - width);
    const edited = edits.some((e) => e > prev && e <= st);
    blocks.push({ left, width, edited });
    prev = st;
  }

  return (
    <svg viewBox={`0 0 ${W} ${H}`} width={W} height={H} role="img" aria-label={label}>
      <title>{label}</title>
      {track}
      {blocks.map((b, i) => (
        <rect
          key={i}
          x={b.left.toFixed(1)}
          y={3}
          width={b.width.toFixed(1)}
          height={10}
          rx={2}
          fill={b.edited ? ACCENT : "transparent"}
          stroke={b.edited ? "none" : "var(--color-fg-muted)"}
          strokeWidth={1}
        />
      ))}
      {commits.map((c, i) => {
        const cx = Math.min(W - 4, Math.max(4, x(c)));
        return (
          <rect
            key={`c${i}`}
            x={(cx - 4).toFixed(1)}
            y={4}
            width={8}
            height={8}
            fill={COMMIT_MARK}
            transform={`rotate(45 ${cx.toFixed(1)} 8)`}
          />
        );
      })}
    </svg>
  );
}
