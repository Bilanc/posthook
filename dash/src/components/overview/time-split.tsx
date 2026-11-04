import Link from "next/link";
import { Empty, Panel } from "@/components/ui/panel";
import { fmtMinutes, fmtMs, fmtPct, oneLine } from "@/lib/format";
import { SERIES } from "@/lib/palette";
import type { TimeSplit } from "@/lib/queries/timesplit";
import { plural, ratio } from "./series";

interface Part {
  label: string;
  minutes: number;
  color: string;
}

// Active time split into what the model was doing versus what it waited on,
// then the slowest tool calls of the period. Waits are capped per call at the
// idle threshold, the same cap active time applies to the gap around the call,
// so the model remainder is active minus waits; the max(0, …) only guards the
// rare session where overlapping subagent calls still push waits past active.
export function TimeSplitPanel({ ts }: { ts: TimeSplit }) {
  const wait = (fams: string[]) =>
    ts.waits.filter((w) => fams.includes(w.family)).reduce((a, w) => a + w.minutes, 0);
  const shell = wait(["shell"]);
  const mcpWeb = wait(["mcp", "web"]);
  const agent = wait(["agent"]);
  const model = Math.max(0, ts.active_minutes - shell - mcpWeb - agent);
  const parts: Part[] = [
    { label: "Model generating & reading", minutes: model, color: SERIES[0] },
    { label: "Shell", minutes: shell, color: SERIES[1] },
    { label: "MCP & web", minutes: mcpWeb, color: SERIES[3] },
    { label: "Subagents", minutes: agent, color: SERIES[2] },
  ];
  const total = parts.reduce((a, p) => a + p.minutes, 0);
  const missing = ts.agents_without_durations;

  return (
    <Panel title="Where agent time goes" sub={`${fmtMinutes(ts.active_minutes)} active`}>
      {total <= 0 ? (
        <Empty>No active time in this range.</Empty>
      ) : (
        <>
          <div className="segbar flex gap-0.5 h-7" role="img" aria-label="Active time by what the agent was doing">
            {parts.map((p) => {
              const share = ratio(p.minutes, total);
              if (share == null || p.minutes <= 0) return null;
              const labelled = share >= 0.12;
              return (
                <div
                  key={p.label}
                  className={`flex items-center rounded text-xs font-medium text-white whitespace-nowrap overflow-hidden min-w-0 ${
                    labelled ? "px-2" : ""
                  }`}
                  style={{ flex: `${p.minutes} 0 0`, background: p.color }}
                  data-tip={`${p.label}\n${fmtMinutes(p.minutes)} · ${share < 0.005 ? "<1%" : fmtPct(share)}`}
                >
                  {labelled ? fmtPct(share) : ""}
                </div>
              );
            })}
          </div>
          <div className="mt-2 flex flex-wrap gap-x-3.5 gap-y-1.5 text-xs text-[var(--color-fg-2)]">
            {parts.map((p) => (
              <span key={p.label} className="inline-flex items-center gap-1.5">
                <i className="inline-block h-2.5 w-2.5 rounded-sm" style={{ background: p.color }} />
                {p.label} · {fmtMinutes(p.minutes)}
              </span>
            ))}
          </div>
        </>
      )}
      {missing.length ? (
        <p className="mt-2 text-[11px] text-[var(--color-fg-faint)]">
          {missing.join(", ")} {plural(missing.length, "reports", "report")} no tool durations, so{" "}
          {plural(missing.length, "its", "their")} time in tools is unknown.
        </p>
      ) : null}

      <div className="mt-4 text-xs text-[var(--color-fg-muted)]">Slowest calls this period</div>
      {ts.slowest.length === 0 ? (
        <div className="mt-1 text-xs text-[var(--color-fg-faint)]">No tool durations recorded.</div>
      ) : (
        <ul className="mt-1 text-xs">
          {ts.slowest.slice(0, 5).map((c) => {
            const full = c.summary ?? c.tool_name ?? "";
            const shown = full ? oneLine(full, 120) : "—";
            return (
              <li key={`${c.session_id}-${c.ts}`} className="border-t border-[var(--color-border)]">
                <Link
                  href={`/sessions/${encodeURIComponent(c.session_id)}`}
                  className="grid grid-cols-[minmax(0,1fr)_auto_auto] items-baseline gap-3 py-1.5 -mx-1.5 px-1.5 rounded hover:bg-[var(--color-bg-2)]"
                >
                  <span className="font-mono truncate text-[var(--color-fg)]" title={full || undefined}>
                    {shown}
                  </span>
                  <span className="text-[var(--color-fg-muted)] truncate max-w-[12rem]">{c.repo_name ?? "—"}</span>
                  <span className="text-[var(--color-fg-2)] tabular-nums text-right">{fmtMs(c.duration_ms) || "—"}</span>
                </Link>
              </li>
            );
          })}
        </ul>
      )}
    </Panel>
  );
}
