import { Empty } from "@/components/ui/panel";
import { fmtNum, fmtPct } from "@/lib/format";
import { ACCENT, MUTED_MARK } from "@/lib/palette";
import type { FileSummary } from "./model";

// Per file: how much of what the agent wrote reached a commit. Bars share one
// scale (the largest file) so widths compare across rows. Each segment carries
// a data-tip for the shared hover layer; the path keeps a native title since
// that is overflow text, not a chart value.
export function FilesPanel({ files }: { files: FileSummary }) {
  if (files.rows.length === 0) return <Empty>No file edits captured.</Empty>;
  const fmax = Math.max(1, ...files.rows.map((r) => Math.max(r.generated, r.committed)));
  const pct = (v: number) => `${((v / fmax) * 100).toFixed(1)}%`;
  return (
    <div className="grid content-start gap-2.5">
      {files.rows.map((r) => {
        const lost = Math.max(0, r.generated - r.committed);
        return (
          <div key={r.key} className="grid gap-1 text-xs">
            <div className="truncate font-mono text-[11px] text-[var(--color-fg)]" title={r.path}>
              {r.path}
            </div>
            <div className="segbar flex h-1.5 w-full gap-[2px] overflow-hidden">
              {r.committed > 0 ? (
                <div
                  className="rounded-sm shrink-0"
                  style={{ width: pct(r.committed), background: ACCENT }}
                  data-tip={`${r.path}\n${fmtNum(r.committed)} lines committed`}
                />
              ) : null}
              {lost > 0 ? (
                <div
                  className="rounded-sm shrink-0"
                  style={{ width: pct(lost), background: MUTED_MARK }}
                  data-tip={`${r.path}\n${fmtNum(lost)} lines not committed`}
                />
              ) : null}
            </div>
            <div className="flex justify-between gap-2 text-[var(--color-fg-muted)] tabular-nums">
              <span>
                {r.committed ? `${fmtNum(r.committed)} committed` : "not committed"}
                {r.committed && lost ? ` · ${fmtNum(lost)} dropped` : ""}
              </span>
              <span>
                {r.committed
                  ? fmtPct(Math.min(1, r.committed / Math.max(1, r.generated)))
                  : `${fmtNum(r.generated)} generated`}
              </span>
            </div>
          </div>
        );
      })}
      <div className="mt-1 flex flex-wrap gap-x-3.5 gap-y-1.5 text-xs text-[var(--color-fg-2)]">
        <span className="inline-flex items-center gap-1.5">
          <i className="inline-block h-2.5 w-2.5 rounded-sm" style={{ background: ACCENT }} />
          committed
        </span>
        <span className="inline-flex items-center gap-1.5">
          <i className="inline-block h-2.5 w-2.5 rounded-sm" style={{ background: MUTED_MARK }} />
          not committed
        </span>
      </div>
    </div>
  );
}
