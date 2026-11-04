import { FAMILY_COLOR } from "@/lib/palette";
import { FAMILY_LABEL, FAMILY_ORDER, type ToolFamily } from "@/lib/tool-families";
import { fmtNum, fmtPct } from "@/lib/format";
import { Empty } from "@/components/ui/panel";
import type { ToolMixRow } from "@/lib/queries/toolmix";

interface Props {
  mix: ToolMixRow[];
}

interface AgentMix {
  agent: string;
  total: number;
  byFamily: Partial<Record<ToolFamily, number>>;
}

// 100% stacked bar per agent across the fixed tool families. Shows how
// differently agents work: Codex runs everything through shell, Claude Code
// reads before it edits.
export function ToolMix({ mix }: Props) {
  const byAgent = new Map<string, AgentMix>();
  for (const r of mix) {
    if (!r.agent || !(r.calls > 0)) continue;
    const a = byAgent.get(r.agent) ?? { agent: r.agent, total: 0, byFamily: {} };
    a.byFamily[r.family] = (a.byFamily[r.family] ?? 0) + r.calls;
    a.total += r.calls;
    byAgent.set(r.agent, a);
  }
  const agents = [...byAgent.values()].sort((a, b) => b.total - a.total || a.agent.localeCompare(b.agent));
  if (agents.length === 0) return <Empty>No tool calls in this range.</Empty>;

  const present = FAMILY_ORDER.filter((f) => agents.some((a) => (a.byFamily[f] ?? 0) > 0));

  return (
    <div>
      {agents.map((a) => (
        <div key={a.agent} className="my-2 grid grid-cols-[84px_1fr] items-center gap-2.5">
          <div className="text-xs text-[var(--color-fg-2)]">
            {a.agent}
            <br />
            <span className="text-[11px] text-[var(--color-fg-muted)]">{fmtNum(a.total)} calls</span>
          </div>
          <div
            className="flex h-[22px] gap-[2px] overflow-hidden rounded"
            role="img"
            aria-label={`${a.agent}: ${present
              .filter((f) => (a.byFamily[f] ?? 0) > 0)
              .map((f) => `${FAMILY_LABEL[f]} ${fmtPct((a.byFamily[f] ?? 0) / a.total)}`)
              .join(", ")}`}
          >
            {present.map((f) => {
              const calls = a.byFamily[f] ?? 0;
              if (calls <= 0) return null;
              const share = calls / a.total;
              return (
                <div
                  key={f}
                  title={`${a.agent} · ${FAMILY_LABEL[f]}: ${fmtNum(calls)} calls (${fmtPct(share)})`}
                  className="flex min-w-0 items-center overflow-hidden whitespace-nowrap rounded px-2 text-[11px] font-medium text-white"
                  style={{ flex: `${calls} 0 0`, background: FAMILY_COLOR[f] }}
                >
                  {share >= 0.1 ? fmtPct(share) : ""}
                </div>
              );
            })}
          </div>
        </div>
      ))}
      <div className="mt-2 flex flex-wrap gap-x-3.5 gap-y-1.5 text-xs text-[var(--color-fg-2)]">
        {present.map((f) => (
          <span key={f}>
            <i className="mr-1.5 inline-block h-2.5 w-2.5 rounded-sm align-[-1px]" style={{ background: FAMILY_COLOR[f] }} />
            {FAMILY_LABEL[f]}
          </span>
        ))}
      </div>
    </div>
  );
}
