"use client";

import { useState } from "react";
import type { TokenBreakdownRow, TokenBucket, TokenSummary } from "@/lib/queries/tokens";

type Dimension = "agent" | "model" | "repo" | "engineer";

const DIMENSIONS: Array<{ key: Dimension; label: string }> = [
  { key: "agent", label: "Agent" },
  { key: "model", label: "Model" },
  { key: "repo", label: "Repo" },
  { key: "engineer", label: "Engineer" },
];

export const TOKEN_BUCKETS: Array<{
  key: keyof TokenBucket;
  label: string;
  color: string;
}> = [
  { key: "input_tokens", label: "Input", color: "#6366f1" },
  { key: "output_tokens", label: "Output", color: "#14b8a6" },
  { key: "cache_read_tokens", label: "Cache read", color: "#0ea5e9" },
  { key: "cache_creation_tokens", label: "Cache write", color: "#f59e0b" },
];

const compact = new Intl.NumberFormat("en-US", {
  notation: "compact",
  maximumFractionDigits: 1,
});

function fmtTok(n: number | null | undefined): string {
  return n == null ? "—" : compact.format(n);
}

function fmtPct(n: number | null): string {
  return n == null ? "—" : `${n.toFixed(1)}%`;
}

function total(b: TokenBucket): number {
  return (
    (b.input_tokens ?? 0) +
    (b.output_tokens ?? 0) +
    (b.cache_read_tokens ?? 0) +
    (b.cache_creation_tokens ?? 0)
  );
}

function cacheHitRate(b: TokenBucket): number | null {
  const prompt =
    (b.input_tokens ?? 0) + (b.cache_read_tokens ?? 0) + (b.cache_creation_tokens ?? 0);
  return prompt === 0 ? null : ((b.cache_read_tokens ?? 0) / prompt) * 100;
}

interface Props {
  summary: TokenSummary;
  breakdowns: Record<Dimension, TokenBreakdownRow[]>;
}

export function TokenUsagePanel({ summary, breakdowns }: Props) {
  const [dim, setDim] = useState<Dimension>("agent");
  const rows = breakdowns[dim];

  const grand = total(summary);
  const hasUsage = summary.sessions_with_usage > 0 && grand > 0;
  const hitRate = cacheHitRate(summary);
  const outPerLine =
    summary.output_tokens != null && summary.lines_generated > 0
      ? summary.output_tokens / summary.lines_generated
      : null;
  const perSession =
    summary.sessions_with_usage > 0 ? grand / summary.sessions_with_usage : null;

  return (
    <div className="rounded-lg border border-[var(--color-border)] bg-[var(--color-bg-elevated)] p-5">
      <div className="flex flex-wrap items-center justify-between gap-3 mb-4">
        <div>
          <div className="text-xs uppercase tracking-wider text-[var(--color-fg-muted)]">
            Token usage
          </div>
          <div className="mt-1 text-xs text-[var(--color-fg-muted)]">
            {summary.sessions_with_usage.toLocaleString()} of{" "}
            {summary.sessions_total.toLocaleString()} session
            {summary.sessions_total === 1 ? "" : "s"} report usage (Claude Code &amp; Codex;
            Cursor exposes none)
          </div>
        </div>
        <div className="text-right">
          <div className="text-xs uppercase tracking-wider text-[var(--color-fg-muted)]">
            Total
          </div>
          <div className="text-2xl font-semibold tabular-nums">
            {hasUsage ? compact.format(grand) : "—"}
          </div>
        </div>
      </div>

      {!hasUsage ? (
        <div className="text-sm text-[var(--color-fg-muted)]">
          No token usage reported for the current filters.
        </div>
      ) : (
        <>
          {/* Composition bar */}
          <div className="flex h-3 w-full overflow-hidden rounded bg-[var(--color-bg)] mb-3">
            {TOKEN_BUCKETS.map((b) => {
              const v = summary[b.key] ?? 0;
              if (v <= 0) return null;
              return (
                <div
                  key={b.key}
                  style={{ width: `${(v / grand) * 100}%`, background: b.color }}
                  title={`${b.label}: ${v.toLocaleString()}`}
                />
              );
            })}
          </div>

          <div className="grid grid-cols-2 md:grid-cols-4 gap-3 mb-5">
            {TOKEN_BUCKETS.map((b) => {
              const v = summary[b.key];
              return (
                <div key={b.key} className="rounded border border-[var(--color-border)] bg-[var(--color-bg)] p-3">
                  <div className="flex items-center gap-1.5 text-[10px] uppercase tracking-wider text-[var(--color-fg-muted)]">
                    <span
                      className="inline-block h-2 w-2 rounded-sm"
                      style={{ background: b.color }}
                    />
                    {b.label}
                  </div>
                  <div className="mt-1 text-lg font-semibold tabular-nums">{fmtTok(v)}</div>
                  <div className="text-[11px] text-[var(--color-fg-muted)] tabular-nums">
                    {v == null ? "not reported" : fmtPct((v / grand) * 100) + " of total"}
                  </div>
                </div>
              );
            })}
          </div>

          <div className="grid grid-cols-1 md:grid-cols-3 gap-3 mb-5 text-sm">
            <Derived
              label="Cache hit rate"
              value={fmtPct(hitRate)}
              hint="cache reads ÷ all prompt tokens"
            />
            <Derived
              label="Output tokens / AI line"
              value={outPerLine == null ? "—" : outPerLine.toFixed(0)}
              hint="from sessions that report usage"
            />
            <Derived
              label="Tokens / session"
              value={perSession == null ? "—" : compact.format(perSession)}
              hint="avg total across reporting sessions"
            />
          </div>

          <div className="flex flex-wrap items-center justify-between gap-3 mb-2">
            <div className="text-xs uppercase tracking-wider text-[var(--color-fg-muted)]">
              By {DIMENSIONS.find((d) => d.key === dim)?.label.toLowerCase()}
            </div>
            <div className="flex items-center gap-1 rounded-md border border-[var(--color-border)] bg-[var(--color-bg)] p-0.5">
              {DIMENSIONS.map((d) => (
                <button
                  key={d.key}
                  type="button"
                  onClick={() => setDim(d.key)}
                  className={`text-xs rounded px-2.5 py-1 transition-colors ${
                    dim === d.key
                      ? "bg-[var(--color-accent)] text-white"
                      : "text-[var(--color-fg-muted)] hover:text-[var(--color-fg)]"
                  }`}
                >
                  {d.label}
                </button>
              ))}
            </div>
          </div>

          {rows.length === 0 ? (
            <div className="text-sm text-[var(--color-fg-muted)]">No data.</div>
          ) : (
            <div className="overflow-x-auto">
              <table className="w-full text-xs tabular-nums">
                <thead className="text-[var(--color-fg-muted)]">
                  <tr>
                    <th className="py-1 pr-3 text-left font-medium">Key</th>
                    <th className="py-1 px-3 text-right font-medium">Sessions</th>
                    <th className="py-1 px-3 text-right font-medium">Input</th>
                    <th className="py-1 px-3 text-right font-medium">Output</th>
                    <th className="py-1 px-3 text-right font-medium">Cache read</th>
                    <th className="py-1 px-3 text-right font-medium">Cache write</th>
                    <th className="py-1 px-3 text-right font-medium">Total</th>
                    <th className="py-1 px-3 text-right font-medium">Cache hit</th>
                    <th className="py-1 pl-3 text-left font-medium w-32">Share</th>
                  </tr>
                </thead>
                <tbody>
                  {rows.map((r) => {
                    const t = total(r);
                    return (
                      <tr key={r.key} className="border-t border-[var(--color-border)]">
                        <td className="py-1.5 pr-3 text-left">{r.display_name ?? r.key}</td>
                        <td className="py-1.5 px-3 text-right">{r.sessions.toLocaleString()}</td>
                        <td className="py-1.5 px-3 text-right">{fmtTok(r.input_tokens)}</td>
                        <td className="py-1.5 px-3 text-right">{fmtTok(r.output_tokens)}</td>
                        <td className="py-1.5 px-3 text-right">{fmtTok(r.cache_read_tokens)}</td>
                        <td className="py-1.5 px-3 text-right">{fmtTok(r.cache_creation_tokens)}</td>
                        <td className="py-1.5 px-3 text-right font-medium">{compact.format(t)}</td>
                        <td className="py-1.5 px-3 text-right">{fmtPct(cacheHitRate(r))}</td>
                        <td className="py-1.5 pl-3">
                          <div className="h-1.5 w-full rounded bg-[var(--color-bg)]">
                            <div
                              className="h-1.5 rounded bg-[var(--color-accent)]"
                              style={{ width: `${grand > 0 ? (t / grand) * 100 : 0}%` }}
                            />
                          </div>
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          )}
        </>
      )}
    </div>
  );
}

function Derived({ label, value, hint }: { label: string; value: string; hint: string }) {
  return (
    <div className="flex items-baseline justify-between gap-3 rounded border border-[var(--color-border)] bg-[var(--color-bg)] px-3 py-2">
      <div>
        <div className="text-[10px] uppercase tracking-wider text-[var(--color-fg-muted)]">
          {label}
        </div>
        <div className="text-[11px] text-[var(--color-fg-muted)]">{hint}</div>
      </div>
      <div className="text-lg font-semibold tabular-nums">{value}</div>
    </div>
  );
}
