"use client";

import { useRouter, usePathname, useSearchParams } from "next/navigation";
import { useState, useTransition } from "react";
import { formatISO, subDays } from "date-fns";
import type { FilterOptions } from "@/lib/queries/breakdowns";
import type { Filters } from "@/lib/filters";

const QUICK_RANGES: Array<{ key: string; label: string; days: number }> = [
  { key: "today", label: "Today", days: 0 },
  { key: "7d", label: "7d", days: 6 },
  { key: "30d", label: "30d", days: 29 },
  { key: "90d", label: "90d", days: 89 },
];

function isoDate(d: Date): string {
  return formatISO(d, { representation: "date" });
}

function rangeFor(days: number): { from: string; to: string } {
  const now = new Date();
  return { from: isoDate(subDays(now, days)), to: isoDate(now) };
}

interface Props {
  filters: Filters;
  options: FilterOptions;
}

export function FilterBar({ filters, options }: Props) {
  const router = useRouter();
  const pathname = usePathname();
  const searchParams = useSearchParams();
  const [, startTransition] = useTransition();

  const [open, setOpen] = useState<string | null>(null);

  function apply(updates: Record<string, string | null | undefined>) {
    const next = new URLSearchParams(searchParams.toString());
    for (const [k, v] of Object.entries(updates)) {
      if (v == null || v === "") next.delete(k);
      else next.set(k, v);
    }
    startTransition(() => {
      router.push(`${pathname}?${next.toString()}`);
    });
  }

  function toggleMulti(key: "agents" | "models" | "repos" | "engineers", val: string) {
    const current = filters[key];
    const next = current.includes(val)
      ? current.filter((x) => x !== val)
      : [...current, val];
    apply({ [key]: next.length ? next.join(",") : null });
  }

  const activeRange = QUICK_RANGES.find((r) => {
    const { from, to } = rangeFor(r.days);
    return filters.from === from && filters.to === to;
  })?.key;

  return (
    <div className="rounded-lg border border-[var(--color-border)] bg-[var(--color-bg-elevated)] p-4 mb-6">
      <div className="flex flex-wrap items-center gap-3">
        <div className="flex items-center gap-1 rounded-md border border-[var(--color-border)] bg-[var(--color-bg)] p-0.5">
          {QUICK_RANGES.map((r) => {
            const isActive = activeRange === r.key;
            return (
              <button
                key={r.key}
                type="button"
                onClick={() => {
                  const { from, to } = rangeFor(r.days);
                  apply({ from, to });
                }}
                className={`text-xs rounded px-2.5 py-1 transition-colors ${
                  isActive
                    ? "bg-[var(--color-accent)] text-white"
                    : "text-[var(--color-fg-muted)] hover:text-[var(--color-fg)]"
                }`}
              >
                {r.label}
              </button>
            );
          })}
        </div>

        <DateInput
          label="From"
          value={filters.from ?? ""}
          onChange={(v) => apply({ from: v || null })}
        />
        <DateInput
          label="To"
          value={filters.to ?? ""}
          onChange={(v) => apply({ to: v || null })}
        />

        <Pill
          label="Agent"
          count={filters.agents.length}
          open={open === "agents"}
          onClick={() => setOpen(open === "agents" ? null : "agents")}
        />
        <Pill
          label="Model"
          count={filters.models.length}
          open={open === "models"}
          onClick={() => setOpen(open === "models" ? null : "models")}
        />
        <Pill
          label="Repo"
          count={filters.repos.length}
          open={open === "repos"}
          onClick={() => setOpen(open === "repos" ? null : "repos")}
        />
        <Pill
          label="Engineer"
          count={filters.engineers.length}
          open={open === "engineers"}
          onClick={() => setOpen(open === "engineers" ? null : "engineers")}
        />

        {(filters.agents.length ||
          filters.models.length ||
          filters.repos.length ||
          filters.engineers.length ||
          filters.from ||
          filters.to) ? (
          <button
            type="button"
            onClick={() =>
              apply({ from: null, to: null, agents: null, models: null, repos: null, engineers: null })
            }
            className="ml-auto text-xs text-[var(--color-fg-muted)] hover:text-[var(--color-accent)]"
          >
            Reset
          </button>
        ) : null}
      </div>

      {open === "agents" ? (
        <MultiList
          values={options.agents.map((a) => ({ key: a, label: a }))}
          selected={filters.agents}
          onToggle={(v) => toggleMulti("agents", v)}
        />
      ) : null}
      {open === "models" ? (
        <MultiList
          values={options.models.map((m) => ({ key: m, label: m }))}
          selected={filters.models}
          onToggle={(v) => toggleMulti("models", v)}
        />
      ) : null}
      {open === "repos" ? (
        <MultiList
          values={options.repos.map((r) => ({ key: r.id, label: r.name }))}
          selected={filters.repos}
          onToggle={(v) => toggleMulti("repos", v)}
        />
      ) : null}
      {open === "engineers" ? (
        <MultiList
          values={options.engineers.map((e) => ({
            key: e.email,
            label: e.name ? `${e.name} <${e.email}>` : e.email,
          }))}
          selected={filters.engineers}
          onToggle={(v) => toggleMulti("engineers", v)}
        />
      ) : null}
    </div>
  );
}

function DateInput({
  label,
  value,
  onChange,
}: {
  label: string;
  value: string;
  onChange: (v: string) => void;
}) {
  return (
    <label className="flex items-center gap-2 text-xs text-[var(--color-fg-muted)]">
      <span>{label}</span>
      <span className="relative flex items-center bg-[var(--color-bg)] border border-[var(--color-border)] rounded hover:border-[var(--color-fg-muted)] focus-within:border-[var(--color-accent)] transition-colors">
        <svg
          aria-hidden
          viewBox="0 0 24 24"
          className="pointer-events-none absolute left-2 h-3.5 w-3.5 text-[var(--color-fg)]"
          fill="none"
          stroke="currentColor"
          strokeWidth="2"
          strokeLinecap="round"
          strokeLinejoin="round"
        >
          <rect x="3" y="4" width="18" height="18" rx="2" />
          <path d="M16 2v4M8 2v4M3 10h18" />
        </svg>
        <input
          type="date"
          value={value}
          onChange={(e) => onChange(e.target.value)}
          className="bg-transparent rounded pl-7 pr-2 py-1 text-sm text-[var(--color-fg)] cursor-pointer outline-none w-[8.5rem]"
        />
      </span>
    </label>
  );
}

function Pill({
  label,
  count,
  open,
  onClick,
}: {
  label: string;
  count: number;
  open: boolean;
  onClick: () => void;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={`text-xs rounded-full border px-3 py-1.5 transition-colors ${
        count > 0
          ? "border-[var(--color-accent)] text-[var(--color-accent)]"
          : "border-[var(--color-border)] text-[var(--color-fg-muted)] hover:text-[var(--color-fg)]"
      } ${open ? "ring-1 ring-[var(--color-accent)]" : ""}`}
    >
      {label}
      {count > 0 ? <span className="ml-1 opacity-70">({count})</span> : null}
    </button>
  );
}

function MultiList({
  values,
  selected,
  onToggle,
}: {
  values: Array<{ key: string; label: string }>;
  selected: string[];
  onToggle: (v: string) => void;
}) {
  if (values.length === 0) {
    return <div className="mt-3 text-xs text-[var(--color-fg-muted)]">No options.</div>;
  }
  return (
    <div className="mt-3 flex flex-wrap gap-2">
      {values.map((v) => {
        const isSelected = selected.includes(v.key);
        return (
          <button
            key={v.key}
            type="button"
            onClick={() => onToggle(v.key)}
            className={`text-xs rounded border px-2 py-1 ${
              isSelected
                ? "border-[var(--color-accent)] text-[var(--color-accent)] bg-[var(--color-accent)]/10"
                : "border-[var(--color-border)] text-[var(--color-fg)] hover:border-[var(--color-fg-muted)]"
            }`}
          >
            {v.label}
          </button>
        );
      })}
    </div>
  );
}
