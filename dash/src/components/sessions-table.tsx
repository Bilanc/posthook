import Link from "next/link";
import { format, formatDistanceStrict, parseISO } from "date-fns";
import type { SessionListRow } from "@/lib/queries/sessions";

interface Props {
  rows: SessionListRow[];
}

function fmtNum(n: number): string {
  return new Intl.NumberFormat("en-US").format(Math.round(n));
}

function fmtDuration(hours: number | null): string {
  if (hours == null || hours === 0) return "—";
  if (hours < 1) return `${Math.round(hours * 60)}m`;
  return `${hours.toFixed(1)}h`;
}

function fmtStarted(iso: string): string {
  try {
    return format(parseISO(iso), "MMM d, HH:mm");
  } catch {
    return iso.slice(0, 16);
  }
}

function fmtRelative(iso: string): string {
  try {
    return formatDistanceStrict(parseISO(iso), new Date(), { addSuffix: true });
  } catch {
    return "";
  }
}

export function SessionsTable({ rows }: Props) {
  if (rows.length === 0) {
    return (
      <div className="rounded-lg border border-[var(--color-border)] bg-[var(--color-bg-elevated)] p-8 text-center text-sm text-[var(--color-fg-muted)]">
        No sessions match the current filters.
      </div>
    );
  }

  return (
    <div className="rounded-lg border border-[var(--color-border)] bg-[var(--color-bg-elevated)] overflow-hidden">
      <table className="w-full text-sm">
        <thead className="bg-[var(--color-bg)] text-xs uppercase tracking-wider text-[var(--color-fg-muted)]">
          <tr>
            <th className="text-left px-4 py-2.5 font-medium">Started</th>
            <th className="text-left px-4 py-2.5 font-medium">Duration</th>
            <th className="text-left px-4 py-2.5 font-medium">Agent</th>
            <th className="text-left px-4 py-2.5 font-medium">Model</th>
            <th className="text-left px-4 py-2.5 font-medium">Engineer</th>
            <th className="text-left px-4 py-2.5 font-medium">Repo</th>
            <th className="text-right px-4 py-2.5 font-medium">Lines gen</th>
            <th className="text-right px-4 py-2.5 font-medium">Edits</th>
            <th className="text-right px-4 py-2.5 font-medium">Files</th>
            <th className="text-right px-4 py-2.5 font-medium">Commits</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((r) => (
            <tr
              key={r.id}
              className="border-t border-[var(--color-border)] hover:bg-[var(--color-bg)]"
            >
              <td className="px-4 py-2.5">
                <Link
                  href={`/sessions/${r.id}`}
                  className="text-[var(--color-accent)] hover:underline tabular-nums"
                  title={fmtRelative(r.started_at)}
                >
                  {fmtStarted(r.started_at)}
                </Link>
              </td>
              <td className="px-4 py-2.5 tabular-nums text-[var(--color-fg-muted)]">
                {fmtDuration(r.duration_hours)}
              </td>
              <td className="px-4 py-2.5">{r.agent_slug}</td>
              <td className="px-4 py-2.5 text-[var(--color-fg-muted)]">
                {r.model_slug ?? "—"}
              </td>
              <td className="px-4 py-2.5 text-[var(--color-fg-muted)]">
                {r.engineer_name || r.engineer_email || "—"}
              </td>
              <td className="px-4 py-2.5 text-[var(--color-fg-muted)]">
                {r.repo_name ?? "—"}
              </td>
              <td className="px-4 py-2.5 text-right tabular-nums">
                {fmtNum(r.lines_generated)}
              </td>
              <td className="px-4 py-2.5 text-right tabular-nums">{r.edits}</td>
              <td className="px-4 py-2.5 text-right tabular-nums">{r.files_touched}</td>
              <td className="px-4 py-2.5 text-right tabular-nums">
                {r.commits_attributed}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
