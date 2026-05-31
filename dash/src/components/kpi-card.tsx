export interface KpiCardProps {
  label: string;
  value: string;
  sublabel?: string | null;
  accent?: boolean;
}

export function KpiCard({ label, value, sublabel, accent }: KpiCardProps) {
  return (
    <div className="rounded-lg border border-[var(--color-border)] bg-[var(--color-bg-elevated)] p-5">
      <div className="text-xs uppercase tracking-wider text-[var(--color-fg-muted)]">
        {label}
      </div>
      <div
        className={`mt-2 text-3xl font-semibold tabular-nums ${
          accent ? "text-[var(--color-accent)]" : ""
        }`}
      >
        {value}
      </div>
      {sublabel ? (
        <div className="mt-1 text-xs text-[var(--color-fg-muted)]">{sublabel}</div>
      ) : null}
    </div>
  );
}
