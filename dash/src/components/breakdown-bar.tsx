"use client";

import {
  Bar,
  BarChart,
  CartesianGrid,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
  Legend,
} from "recharts";
import type { BreakdownRow } from "@/types/posthook";

interface Props {
  title: string;
  rows: BreakdownRow[];
}

export function BreakdownBar({ title, rows }: Props) {
  if (rows.length === 0) {
    return (
      <div className="rounded-lg border border-[var(--color-border)] bg-[var(--color-bg-elevated)] p-5">
        <div className="text-xs uppercase tracking-wider text-[var(--color-fg-muted)] mb-3">
          {title}
        </div>
        <div className="text-sm text-[var(--color-fg-muted)]">No data.</div>
      </div>
    );
  }

  const data = rows.map((r) => ({
    name: r.display_name ?? r.key,
    "Lines committed": r.lines_committed,
    "Lines not committed": Math.max(0, r.lines_generated - r.lines_committed),
  }));

  const height = Math.max(160, 50 + data.length * 36);

  return (
    <div className="rounded-lg border border-[var(--color-border)] bg-[var(--color-bg-elevated)] p-5">
      <div className="text-xs uppercase tracking-wider text-[var(--color-fg-muted)] mb-3">
        {title}
      </div>
      <ResponsiveContainer width="100%" height={height}>
        <BarChart data={data} layout="vertical" margin={{ top: 5, right: 16, left: 8, bottom: 5 }}>
          <CartesianGrid strokeDasharray="3 3" stroke="#1f1f23" />
          <XAxis type="number" stroke="#8e8e93" fontSize={11} />
          <YAxis
            type="category"
            dataKey="name"
            stroke="#8e8e93"
            fontSize={11}
            width={140}
          />
          <Tooltip
            contentStyle={{
              background: "#0a0a0b",
              border: "1px solid #1f1f23",
              borderRadius: 6,
              fontSize: 12,
            }}
            formatter={(value: number, name: string) => [value.toLocaleString(), name]}
          />
          <Legend wrapperStyle={{ fontSize: 11 }} />
          <Bar dataKey="Lines committed" stackId="generated" fill="#14b8a6" />
          <Bar dataKey="Lines not committed" stackId="generated" fill="#6366f1" />
        </BarChart>
      </ResponsiveContainer>
      <div className="mt-4 overflow-x-auto">
        <table className="w-full text-xs tabular-nums">
          <thead className="text-[var(--color-fg-muted)]">
            <tr>
              <th className="py-1 pr-3 text-left font-medium">Key</th>
              <th className="py-1 px-3 text-right font-medium">Edits</th>
              <th className="py-1 px-3 text-right font-medium">Generated</th>
              <th className="py-1 px-3 text-right font-medium">Committed</th>
              <th className="py-1 pl-3 text-right font-medium">Sessions</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((r) => (
              <tr key={r.key} className="border-t border-[var(--color-border)]">
                <td className="py-1.5 pr-3 text-left">{r.display_name ?? r.key}</td>
                <td className="py-1.5 px-3 text-right">{r.edits.toLocaleString()}</td>
                <td className="py-1.5 px-3 text-right">{r.lines_generated.toLocaleString()}</td>
                <td className="py-1.5 px-3 text-right">{r.lines_committed.toLocaleString()}</td>
                <td className="py-1.5 pl-3 text-right">{r.sessions.toLocaleString()}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}
