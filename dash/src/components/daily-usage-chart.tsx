"use client";

import { useState } from "react";
import { eachDayOfInterval, format, parseISO } from "date-fns";
import {
  Bar,
  CartesianGrid,
  ComposedChart,
  Legend,
  Line,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";
import type { DailyKeyRow, DailyUsage } from "@/lib/queries/daily";

type Mode = "overall" | "agent" | "model" | "repo" | "engineer";

const MODES: Array<{ key: Mode; label: string }> = [
  { key: "overall", label: "Generated vs committed" },
  { key: "agent", label: "Agent" },
  { key: "model", label: "Model" },
  { key: "repo", label: "Repo" },
  { key: "engineer", label: "Engineer" },
];

// Stacked series beyond this collapse into "Other" so the legend stays legible.
const MAX_KEYS = 8;

const GENERATED_COLOR = "#6366f1";
const COMMITTED_COLOR = "#14b8a6";
const OTHER_COLOR = "#3f3f46";
const PALETTE = [
  "#6366f1",
  "#14b8a6",
  "#f59e0b",
  "#f43f5e",
  "#0ea5e9",
  "#a78bfa",
  "#84cc16",
  "#fb923c",
];

interface Props {
  from: string | null;
  to: string | null;
  data: DailyUsage;
}

function dayRange(from: string | null, to: string | null, fallbackDays: string[]): string[] {
  if (from && to && from <= to) {
    return eachDayOfInterval({ start: parseISO(from), end: parseISO(to) }).map((d) =>
      format(d, "yyyy-MM-dd"),
    );
  }
  return [...new Set(fallbackDays)].sort();
}

function pivotKeys(
  rows: DailyKeyRow[],
  days: string[],
): { data: Array<Record<string, number | string>>; keys: string[] } {
  const totals = new Map<string, number>();
  for (const r of rows) totals.set(r.key, (totals.get(r.key) ?? 0) + r.lines);
  const sorted = [...totals.entries()].sort((a, b) => b[1] - a[1]).map(([k]) => k);
  const top = sorted.slice(0, MAX_KEYS);
  const hasOther = sorted.length > MAX_KEYS;

  const byDay = new Map<string, Record<string, number | string>>();
  for (const d of days) {
    const row: Record<string, number | string> = { day: d };
    for (const k of top) row[k] = 0;
    if (hasOther) row["Other"] = 0;
    byDay.set(d, row);
  }
  for (const r of rows) {
    const row = byDay.get(r.day);
    if (!row) continue; // commit/edit days outside the displayed range
    const k = top.includes(r.key) ? r.key : "Other";
    row[k] = (row[k] as number) + r.lines;
  }
  return {
    data: days.map((d) => byDay.get(d)!),
    keys: hasOther ? [...top, "Other"] : top,
  };
}

function fmtDay(day: string): string {
  return format(parseISO(day), "MMM d");
}

export function DailyUsageChart({ from, to, data }: Props) {
  const [mode, setMode] = useState<Mode>("overall");

  const allDays = [
    ...data.overall.map((r) => r.day),
    ...data.byAgent.map((r) => r.day),
  ];
  const days = dayRange(from, to, allDays);

  let chartData: Array<Record<string, number | string>>;
  let series: Array<{ key: string; color: string; type: "bar" | "line" }>;

  if (mode === "overall") {
    const byDay = new Map(data.overall.map((r) => [r.day, r]));
    chartData = days.map((d) => ({
      day: d,
      "Lines generated": byDay.get(d)?.generated ?? 0,
      "Lines committed": byDay.get(d)?.committed ?? 0,
    }));
    series = [
      { key: "Lines generated", color: GENERATED_COLOR, type: "bar" },
      { key: "Lines committed", color: COMMITTED_COLOR, type: "line" },
    ];
  } else {
    const rows = {
      agent: data.byAgent,
      model: data.byModel,
      repo: data.byRepo,
      engineer: data.byEngineer,
    }[mode];
    const pivoted = pivotKeys(rows, days);
    chartData = pivoted.data;
    series = pivoted.keys.map((k, i) => ({
      key: k,
      color: k === "Other" ? OTHER_COLOR : PALETTE[i % PALETTE.length],
      type: "bar" as const,
    }));
  }

  const hasData = chartData.some((row) =>
    series.some((s) => (row[s.key] as number) > 0),
  );

  return (
    <div className="rounded-lg border border-[var(--color-border)] bg-[var(--color-bg-elevated)] p-5">
      <div className="flex flex-wrap items-center justify-between gap-3 mb-3">
        <div className="text-xs uppercase tracking-wider text-[var(--color-fg-muted)]">
          Daily usage — AI lines per day
        </div>
        <div className="flex items-center gap-1 rounded-md border border-[var(--color-border)] bg-[var(--color-bg)] p-0.5">
          {MODES.map((m) => (
            <button
              key={m.key}
              type="button"
              onClick={() => setMode(m.key)}
              className={`text-xs rounded px-2.5 py-1 transition-colors ${
                mode === m.key
                  ? "bg-[var(--color-accent)] text-white"
                  : "text-[var(--color-fg-muted)] hover:text-[var(--color-fg)]"
              }`}
            >
              {m.label}
            </button>
          ))}
        </div>
      </div>
      {hasData ? (
        <ResponsiveContainer width="100%" height={280}>
          <ComposedChart data={chartData} margin={{ top: 5, right: 16, left: 8, bottom: 5 }}>
            <CartesianGrid strokeDasharray="3 3" stroke="#1f1f23" vertical={false} />
            <XAxis
              dataKey="day"
              stroke="#8e8e93"
              fontSize={11}
              tickFormatter={fmtDay}
              minTickGap={24}
            />
            <YAxis stroke="#8e8e93" fontSize={11} width={56} />
            <Tooltip
              cursor={{ fill: "#1f1f23", opacity: 0.4 }}
              contentStyle={{
                background: "#0a0a0b",
                border: "1px solid #1f1f23",
                borderRadius: 6,
                fontSize: 12,
              }}
              labelFormatter={(day) => fmtDay(String(day))}
              formatter={(value: number, name: string) => [value.toLocaleString(), name]}
            />
            <Legend wrapperStyle={{ fontSize: 11 }} />
            {series.map((s) =>
              s.type === "line" ? (
                <Line
                  key={s.key}
                  dataKey={s.key}
                  stroke={s.color}
                  strokeWidth={2}
                  dot={{ r: 3, fill: s.color, strokeWidth: 0 }}
                  activeDot={{ r: 4 }}
                />
              ) : (
                <Bar
                  key={s.key}
                  dataKey={s.key}
                  stackId={mode === "overall" ? undefined : "daily"}
                  fill={s.color}
                  maxBarSize={48}
                />
              ),
            )}
          </ComposedChart>
        </ResponsiveContainer>
      ) : (
        <div className="text-sm text-[var(--color-fg-muted)]">No data.</div>
      )}
    </div>
  );
}
