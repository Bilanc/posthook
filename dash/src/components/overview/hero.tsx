import { Panel } from "@/components/ui/panel";
import { fmtCompact, fmtNum, fmtPct } from "@/lib/format";
import type { ActivityTotals } from "@/lib/queries/activity";
import type { Filters } from "@/lib/filters";
import { plural, priorLabel, ratio, windowLabel } from "./series";

// The one number the page leads with: AI lines committed in the window, with
// the share of generated lines it represents and the prior window beside it.
export function Hero({
  filters,
  totals,
  prior,
}: {
  filters: Filters;
  totals: ActivityTotals;
  // null when the filters carry no complete range (no prior window exists).
  prior: ActivityTotals | null;
}) {
  const keep = ratio(totals.lines_committed, totals.lines_generated);
  const parts: string[] = [];
  if (keep != null) {
    parts.push(
      `${fmtPct(keep)} of the ${fmtCompact(totals.lines_generated)} lines agents wrote in the window`,
    );
  }
  if (totals.commits > 0) {
    parts.push(`across ${fmtNum(totals.commits)} ${plural(totals.commits, "commit")}`);
  }
  let sentence: string;
  if (parts.length === 2) sentence = `${parts[0]}, ${parts[1]}.`;
  else if (parts.length === 1) sentence = `${parts[0][0].toUpperCase()}${parts[0].slice(1)}.`;
  else sentence = "No AI edits were captured in this window.";

  let priorSentence: string | null = null;
  if (prior) {
    const label = priorLabel(filters);
    const hadData = prior.sessions > 0 || prior.lines_committed > 0 || prior.commits > 0;
    priorSentence = hadData
      ? `${label[0].toUpperCase()}${label.slice(1)}: ${fmtCompact(prior.lines_committed)} lines, ${fmtNum(prior.commits)} ${plural(prior.commits, "commit")}.`
      : `No sessions in the ${label}.`;
  }

  return (
    <Panel title="Shipped" sub={prior ? `vs ${priorLabel(filters)}` : undefined}>
      <div className="grid grid-cols-1 sm:grid-cols-[auto_1fr] gap-x-6 gap-y-3 items-end">
        <div>
          <div className="text-[13px] text-[var(--color-fg-muted)] mb-1.5">
            AI lines committed, {windowLabel(filters)}
          </div>
          <div className="text-[52px] font-semibold leading-none tracking-tight">
            {fmtCompact(totals.lines_committed)}
          </div>
        </div>
        <p className="text-[13px] text-[var(--color-fg-2)] max-w-[60ch]">
          {sentence}
          {priorSentence ? ` ${priorSentence}` : null}
        </p>
      </div>
    </Panel>
  );
}
