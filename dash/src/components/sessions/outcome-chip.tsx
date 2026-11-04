import type { Outcome } from "@/lib/filters";
import { fmtNum } from "@/lib/format";

// Shipped (n commits) / Edited / Explored. Same footprint as ui/panel's Chip,
// with the dot and border carrying the outcome. The data-tip spells the
// outcome out for the shared hover layer (ui/chart-tooltip).
const BASE =
  "inline-flex items-center gap-1.5 rounded border px-1.5 py-px text-[11px] whitespace-nowrap";
const DOT = "inline-block h-[7px] w-[7px] rounded-full";

export function OutcomeChip({ outcome, commits }: { outcome: Outcome; commits: number }) {
  if (outcome === "shipped") {
    const tip =
      commits === 1
        ? "Shipped: 1 commit carries AI lines from this session"
        : `Shipped: ${fmtNum(commits)} commits carry AI lines from this session`;
    return (
      <span className={`${BASE} border-[var(--color-accent)]/60 text-[#c7c8fb]`} data-tip={tip}>
        <i className={`${DOT} bg-[var(--color-accent)]`} />
        Shipped {fmtNum(commits)}
      </span>
    );
  }
  if (outcome === "edited") {
    return (
      <span
        className={`${BASE} border-[var(--color-border-2)] text-[var(--color-fg-2)]`}
        data-tip="Edited: files changed, nothing committed yet"
      >
        <i className={`${DOT} bg-[var(--color-fg-2)]`} />
        Edited
      </span>
    );
  }
  return (
    <span
      className={`${BASE} border-[var(--color-border-2)] text-[var(--color-fg-muted)]`}
      data-tip="Explored: no file edits"
    >
      <i className={`${DOT} border border-[var(--color-fg-muted)] bg-transparent`} />
      Explored
    </span>
  );
}
