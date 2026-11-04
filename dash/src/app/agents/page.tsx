import { FilterBar } from "@/components/filter-bar";
import { Panel } from "@/components/ui/panel";
import { ModelsTable } from "@/components/agents/models-table";
import { Dumbbell } from "@/components/agents/dumbbell";
import { ToolMix } from "@/components/agents/tool-mix";
import { deriveMetrics, parseSort, sortMetrics } from "@/components/agents/model-metrics";
import { parseFilters, resolveFilters, type SearchParams } from "@/lib/filters";
import { modelTable } from "@/lib/queries/models";
import { toolMix } from "@/lib/queries/toolmix";
import { filterOptions } from "@/lib/queries/breakdowns";

export const dynamic = "force-dynamic";

// Everything in the URL except the sort, so a header link keeps the window
// and filters the reader has set.
function preservedQuery(sp: SearchParams): string {
  const p = new URLSearchParams();
  for (const [k, v] of Object.entries(sp)) {
    if (k === "sort" || k === "dir" || v == null) continue;
    for (const one of Array.isArray(v) ? v : [v]) p.append(k, one);
  }
  return p.toString();
}

export default async function AgentsPage({
  searchParams,
}: {
  searchParams: Promise<SearchParams>;
}) {
  const sp = await searchParams;
  const filters = resolveFilters(parseFilters(sp));
  const options = filterOptions();
  const rows = modelTable(filters).filter((r) => r.sessions > 0);
  const mix = toolMix(filters);
  const { key, dir } = parseSort(sp.sort, sp.dir);

  const metrics = rows.map(deriveMetrics);
  const sorted = sortMetrics(metrics, key, dir);
  // The dumbbell shows the busiest models whatever the table is sorted by.
  const busiest = sortMetrics(metrics, "sessions", "desc").slice(0, 8);

  return (
    <div>
      <h1 className="text-2xl font-semibold mb-1">Agents</h1>
      <p className="text-sm text-[var(--color-fg-muted)] mb-6">
        Which agent and model are worth it for you: what each one keeps, what it costs in tokens,
        and how it works.
      </p>

      <FilterBar filters={filters} options={options} />

      {rows.length === 0 ? (
        <Panel>
          <div className="text-sm text-[var(--color-fg)]">No sessions in this range.</div>
          <div className="mt-1 text-xs text-[var(--color-fg-muted)]">
            Widen the date range or clear the filters.
          </div>
        </Panel>
      ) : (
        <>
          <Panel title="Models" sub="sessions started in window" className="mb-4">
            <ModelsTable rows={sorted} sort={key} dir={dir} preserved={preservedQuery(sp)} />
          </Panel>
          <div className="grid grid-cols-1 gap-4 min-[760px]:grid-cols-2">
            <Panel title="Generated → committed" sub="lines per model">
              <Dumbbell rows={busiest} />
            </Panel>
            <Panel title="Tool mix" sub="share of tool calls by agent">
              <ToolMix mix={mix} />
            </Panel>
          </div>
        </>
      )}
    </div>
  );
}
