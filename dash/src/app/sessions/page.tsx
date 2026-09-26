import Link from "next/link";
import { FilterBar } from "@/components/filter-bar";
import { SessionsTable } from "@/components/sessions-table";
import {
  parseFilters,
  resolveFilters,
  filtersToQueryString,
  type SearchParams,
} from "@/lib/filters";
import { listSessions } from "@/lib/queries/sessions";
import { filterOptions } from "@/lib/queries/breakdowns";

export const dynamic = "force-dynamic";

const DEFAULT_PER_PAGE = 50;

export default async function SessionsPage({
  searchParams,
}: {
  searchParams: Promise<SearchParams>;
}) {
  const sp = await searchParams;
  const filters = resolveFilters(parseFilters(sp));
  const options = filterOptions();
  const page = Math.max(1, parseInt(String(sp.page ?? "1"), 10) || 1);
  const perPage = Math.min(200, parseInt(String(sp.per_page ?? DEFAULT_PER_PAGE), 10) || DEFAULT_PER_PAGE);

  const { rows, total } = listSessions(filters, page, perPage);
  const totalPages = Math.max(1, Math.ceil(total / perPage));
  const qs = filtersToQueryString(filters);
  const qsJoin = qs ? `${qs}&` : "?";
  const hasNarrowingFilters =
    !!filters.q ||
    filters.agents.length > 0 ||
    filters.models.length > 0 ||
    filters.repos.length > 0 ||
    filters.engineers.length > 0;
  const emptyMessage = hasNarrowingFilters
    ? "No sessions match the current filters."
    : "No sessions yet.";

  return (
    <div>
      <h1 className="text-2xl font-semibold mb-1">Sessions</h1>
      <p className="text-sm text-[var(--color-fg-muted)] mb-6">
        {total === 0
          ? emptyMessage
          : `${total} session${total === 1 ? "" : "s"} — showing page ${page} of ${totalPages}.`}
      </p>

      <FilterBar filters={filters} options={options} searchable />

      <SessionsTable rows={rows} />

      {totalPages > 1 ? (
        <div className="mt-6 flex items-center justify-between text-sm">
          {page > 1 ? (
            <Link
              href={`/sessions${qsJoin}page=${page - 1}`}
              className="text-[var(--color-accent)] hover:underline"
            >
              ← Previous
            </Link>
          ) : (
            <span />
          )}
          <span className="text-[var(--color-fg-muted)]">
            Page {page} / {totalPages}
          </span>
          {page < totalPages ? (
            <Link
              href={`/sessions${qsJoin}page=${page + 1}`}
              className="text-[var(--color-accent)] hover:underline"
            >
              Next →
            </Link>
          ) : (
            <span />
          )}
        </div>
      ) : null}
    </div>
  );
}
