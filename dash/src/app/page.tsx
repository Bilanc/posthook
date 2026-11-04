import { FilterBar } from "@/components/filter-bar";
import { Panel } from "@/components/ui/panel";
import { Hero } from "@/components/overview/hero";
import { KpiTiles } from "@/components/overview/kpi-tiles";
import { ActivityChart } from "@/components/overview/activity-chart";
import { RhythmHeatmap } from "@/components/overview/rhythm-heatmap";
import { Outcomes } from "@/components/overview/outcomes";
import { TimeSplitPanel } from "@/components/overview/time-split";
import { parseMeasure, windowDayKeys } from "@/components/overview/series";
import { parseFilters, priorWindow, resolveFilters, type SearchParams } from "@/lib/filters";
import { activityTotals, dailyActivity } from "@/lib/queries/activity";
import { funnel, leadTime } from "@/lib/queries/outcomes";
import { editHeatmap } from "@/lib/queries/heat";
import { timeSplit } from "@/lib/queries/timesplit";
import { filterOptions } from "@/lib/queries/breakdowns";

export const dynamic = "force-dynamic";

export default async function OverviewPage({
  searchParams,
}: {
  searchParams: Promise<SearchParams>;
}) {
  const sp = await searchParams;
  const filters = resolveFilters(parseFilters(sp));
  const measure = parseMeasure(sp.measure);
  const options = filterOptions();

  const rows = dailyActivity(filters);
  const prior = priorWindow(filters);
  const priorRows = prior ? dailyActivity(prior) : null;
  const totals = activityTotals(rows);
  const priorTotals = priorRows ? activityTotals(priorRows) : null;
  const fn = funnel(filters);
  const priorFn = prior ? funnel(prior) : null;
  const lt = leadTime(filters);
  const heat = editHeatmap(filters);
  const ts = timeSplit(filters);
  const days = windowDayKeys(filters, rows);

  const empty = totals.sessions === 0 && fn.sessions === 0;

  return (
    <div>
      <h1 className="text-2xl font-semibold mb-1">Overview</h1>
      <p className="text-sm text-[var(--color-fg-muted)] mb-6">
        What your agents shipped, when they worked, and where their time went.
      </p>

      <FilterBar filters={filters} options={options} />

      {empty ? (
        <Panel>
          <div className="text-sm">No sessions in this range.</div>
          <div className="mt-1 text-xs text-[var(--color-fg-muted)]">
            Widen the date range or clear the filters.
          </div>
        </Panel>
      ) : (
        <div className="grid gap-3.5">
          {/* The daily rows count a commit once per agent it is attributed to; the
              funnel counts each commit once, which is what the hero should say. */}
          <Hero
            filters={filters}
            totals={{ ...totals, commits: fn.commits }}
            prior={priorTotals && priorFn ? { ...priorTotals, commits: priorFn.commits } : priorTotals}
          />

          <KpiTiles rows={rows} days={days} totals={totals} prior={priorTotals} funnel={fn} />

          <div className="grid gap-3.5 lg:grid-cols-3">
            <div className="lg:col-span-2 min-w-0">
              <ActivityChart rows={rows} days={days} measure={measure} sp={sp} />
            </div>
            <RhythmHeatmap cells={heat} />
          </div>

          <div className="grid gap-3.5 md:grid-cols-2">
            <Outcomes funnel={fn} leadTime={lt} />
            <TimeSplitPanel ts={ts} />
          </div>
        </div>
      )}
    </div>
  );
}
