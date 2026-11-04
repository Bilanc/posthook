import type { ReactNode } from "react";

// The one card style every page uses. Border + elevated fill mark a panel as
// a separate object; inside it, headings are small caps and content is flush.
export function Panel({
  title,
  sub,
  children,
  className = "",
}: {
  title?: string;
  sub?: ReactNode;
  children: ReactNode;
  className?: string;
}) {
  return (
    <div
      className={`rounded-lg border border-[var(--color-border)] bg-[var(--color-bg-elevated)] p-4 min-w-0 ${className}`}
    >
      {title ? (
        <div className="mb-3 flex items-baseline gap-3">
          <div className="text-[11px] uppercase tracking-wider text-[var(--color-fg-muted)] font-medium">
            {title}
          </div>
          {sub ? (
            <div className="ml-auto text-xs text-[var(--color-fg-muted)]">{sub}</div>
          ) : null}
        </div>
      ) : null}
      {children}
    </div>
  );
}

// Small labelled token: agent, model, repo, mode. `dot` draws a colour swatch.
export function Chip({
  children,
  dot,
  mono = false,
  title,
}: {
  children: ReactNode;
  dot?: string;
  mono?: boolean;
  title?: string;
}) {
  return (
    <span
      title={title}
      className={`inline-flex items-center gap-1.5 rounded border border-[var(--color-border-2)] px-1.5 py-px text-[11px] text-[var(--color-fg-2)] whitespace-nowrap ${
        mono ? "font-mono" : ""
      }`}
    >
      {dot ? (
        <i className="inline-block h-[7px] w-[7px] rounded-full" style={{ background: dot }} />
      ) : null}
      {children}
    </span>
  );
}

export function Empty({ children }: { children: ReactNode }) {
  return <div className="text-sm text-[var(--color-fg-muted)]">{children}</div>;
}
