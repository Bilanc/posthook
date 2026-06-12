import { KpiCard } from "@/components/kpi-card";
import { FilterBar } from "@/components/filter-bar";
import { SankeyFunnel } from "@/components/sankey-funnel";
import { BreakdownBar } from "@/components/breakdown-bar";
import { DailyUsageChart } from "@/components/daily-usage-chart";
import { parseFilters, resolveFilters, type SearchParams } from "@/lib/filters";
import { overviewSummary, funnel } from "@/lib/queries/overview";
import { dailyUsage } from "@/lib/queries/daily";
import {
  breakdownByAgent,
  breakdownByModel,
  breakdownByRepo,
  breakdownByEngineer,
  filterOptions,
} from "@/lib/queries/breakdowns";

export const dynamic = "force-dynamic";

function fmtNum(n: number): string {
  return new Intl.NumberFormat("en-US").format(Math.round(n));
}

function fmtPct(n: number): string {
  return `${n.toFixed(1)}%`;
}

function fmtHours(n: number): string {
  if (n < 1) return `${(n * 60).toFixed(0)}m`;
  return `${n.toFixed(1)}h`;
}

export default async function OverviewPage({
  searchParams,
}: {
  searchParams: Promise<SearchParams>;
}) {
  const sp = await searchParams;
  const filters = resolveFilters(parseFilters(sp));
  const options = filterOptions();
  const summary = overviewSummary(filters);
  const fn = funnel(filters);
  const daily = dailyUsage(filters);
  const byAgent = breakdownByAgent(filters);
  const byModel = breakdownByModel(filters);
  const byRepo = breakdownByRepo(filters);
  const byEngineer = breakdownByEngineer(filters);

  return (
    <div>
      <h1 className="text-2xl font-semibold mb-1">Overview</h1>
      <p className="text-sm text-[var(--color-fg-muted)] mb-6">
        AI usage across all tracked agents, models, repos, and engineers.
      </p>

      <FilterBar filters={filters} options={options} />

      <section className="grid grid-cols-2 md:grid-cols-3 lg:grid-cols-6 gap-4 mb-8">
        <KpiCard
          label="AI code %"
          value={fmtPct(summary.ai_code_pct)}
          sublabel={`of ${fmtNum(summary.commit_lines_added)} committed lines`}
          accent
        />
        <KpiCard
          label="AI lines generated"
          value={fmtNum(summary.lines_generated)}
          sublabel={`${fmtNum(summary.edits)} edit${summary.edits === 1 ? "" : "s"} captured`}
        />
        <KpiCard
          label="AI lines committed"
          value={fmtNum(summary.lines_committed)}
          sublabel="attributed to captured commits"
        />
        <KpiCard
          label="Top model"
          value={summary.top_model ?? "—"}
          sublabel={summary.top_model ? `${fmtNum(summary.top_model_edits)} edits` : null}
        />
        <KpiCard
          label="Working hours"
          value={fmtHours(summary.working_hours)}
          sublabel={`${summary.sessions} session${summary.sessions === 1 ? "" : "s"}`}
        />
        <KpiCard
          label="Max parallel agents"
          value={String(summary.max_concurrent)}
          sublabel="peak concurrent sessions"
        />
      </section>

      <section className="mb-8">
        <DailyUsageChart from={filters.from} to={filters.to} data={daily} />
      </section>

      <section className="mb-8">
        <SankeyFunnel generated={fn.generated} committed={fn.committed} />
      </section>

      <section className="grid grid-cols-1 lg:grid-cols-2 gap-4">
        <BreakdownBar title="By agent" rows={byAgent} />
        <BreakdownBar title="By model" rows={byModel} />
        <BreakdownBar title="By repo" rows={byRepo} />
        <BreakdownBar title="By engineer" rows={byEngineer} />
      </section>
    </div>
  );
}
