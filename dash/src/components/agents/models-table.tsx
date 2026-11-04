import Link from "next/link";
import { Chip } from "@/components/ui/panel";
import { agentColor, ORDINAL } from "@/lib/palette";
import { fmtCompact, fmtMinutes, fmtNum, fmtPct } from "@/lib/format";
import { defaultDir, type ModelMetrics, type SortDir, type SortKey } from "./model-metrics";

const COLUMNS: Array<{ key: SortKey; label: string }> = [
  { key: "model", label: "Model" },
  { key: "sessions", label: "Sessions" },
  { key: "active", label: "Active" },
  { key: "prompts", label: "Prompts" },
  { key: "generated", label: "Generated" },
  { key: "committed", label: "Committed" },
  { key: "keep", label: "Keep rate" },
  { key: "lpk", label: "Lines / 1K tok" },
  { key: "tpe", label: "Tools / edit" },
  { key: "reedit", label: "Re-edit" },
  { key: "shipped", label: "Shipped" },
];

interface Props {
  rows: ModelMetrics[];
  sort: SortKey;
  dir: SortDir;
  // The other search params (filters), already serialized, so a sort link
  // keeps the window and filters the reader has set.
  preserved: string;
}

function sortHref(preserved: string, key: SortKey, active: SortKey, dir: SortDir): string {
  const next: SortDir = key === active ? (dir === "asc" ? "desc" : "asc") : defaultDir(key);
  const base = preserved ? `${preserved}&` : "";
  return `/agents?${base}sort=${key}&dir=${next}`;
}

function one(n: number | null): string {
  return n == null || !Number.isFinite(n) ? "—" : n.toFixed(1);
}

function Meter({ ratio }: { ratio: number | null }) {
  if (ratio == null) return <>—</>;
  return (
    <span className="inline-flex items-center gap-2">
      <span className="inline-block h-1.5 w-[70px] overflow-hidden rounded-full bg-[#1e2a4a]">
        <span
          className="block h-full rounded-full"
          style={{ width: `${Math.min(100, ratio * 100)}%`, background: ORDINAL[0] }}
        />
      </span>
      {fmtPct(ratio)}
    </span>
  );
}

export function ModelsTable({ rows, sort, dir, preserved }: Props) {
  return (
    <div>
      <div className="overflow-x-auto">
        <table className="w-full border-collapse text-[12.5px] tabular-nums">
          <thead>
            <tr>
              {COLUMNS.map((c) => {
                const active = c.key === sort;
                return (
                  <th
                    key={c.key}
                    scope="col"
                    aria-sort={active ? (dir === "asc" ? "ascending" : "descending") : "none"}
                    className={`whitespace-nowrap border-b border-[var(--color-border-2)] px-2.5 py-1.5 text-[11px] font-medium uppercase tracking-wider ${
                      c.key === "model" ? "text-left" : "text-right"
                    } ${active ? "text-[var(--color-fg-2)]" : "text-[var(--color-fg-muted)]"}`}
                  >
                    <Link
                      href={sortHref(preserved, c.key, sort, dir)}
                      className="inline-flex items-center gap-1 hover:text-[var(--color-fg)]"
                    >
                      {c.label}
                      {active ? (
                        <span aria-hidden className="text-[9px]">{dir === "asc" ? "▲" : "▼"}</span>
                      ) : null}
                    </Link>
                  </th>
                );
              })}
            </tr>
          </thead>
          <tbody>
            {rows.map((m) => {
              const r = m.row;
              const noPrompts = r.prompts === 0 && r.tools > 0;
              return (
                <tr key={`${r.agent}/${r.model}`} className="border-t border-[var(--color-border)] first:border-t-0">
                  <td className="whitespace-nowrap px-2 py-[7px] text-left">
                    <Chip dot={agentColor(r.agent)}>{r.agent}</Chip>
                    <span className="ml-1.5 font-mono">{r.model}</span>
                  </td>
                  <Num>{fmtNum(r.sessions)}</Num>
                  <Num>{fmtMinutes(r.active_minutes)}</Num>
                  <Num title={noPrompts ? "This agent captured no prompts" : undefined}>
                    {noPrompts ? "—" : fmtNum(r.prompts)}
                  </Num>
                  <Num>{fmtCompact(r.lines_generated)}</Num>
                  <Num>{fmtCompact(r.lines_committed)}</Num>
                  <Num>
                    <Meter ratio={m.keep} />
                  </Num>
                  <Num>{one(m.linesPerKTok)}</Num>
                  <Num>{one(m.toolsPerEdit)}</Num>
                  <Num>{fmtPct(m.reedit)}</Num>
                  <Num>
                    {fmtNum(r.sessions_with_commits)}{" "}
                    <span className="text-[var(--color-fg-muted)]">({fmtPct(m.shipped)})</span>
                  </Num>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
      <p className="mt-2 text-[11px] text-[var(--color-fg-muted)]">
        Cursor reports no token usage, so its token columns show &quot;—&quot;. Lines / 1K tok = lines
        generated per thousand output tokens. Re-edit = share of files edited more than once in a
        session.
      </p>
    </div>
  );
}

function Num({ children, title }: { children: React.ReactNode; title?: string }) {
  return (
    <td title={title} className="whitespace-nowrap px-2 py-[7px] text-right">
      {children}
    </td>
  );
}
