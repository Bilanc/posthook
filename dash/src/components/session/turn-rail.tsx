import { fmtTime, oneLine } from "@/lib/format";
import { turnDurationMs, type Turn } from "@/lib/turns";
import { spanLabel } from "./model";

// Numbered jump list for the conversation. Pure anchors: the browser scrolls
// to <article id="turn-n">. The preview is clipped to one line, so each entry
// tips the prompt's opening (shared hover layer, ui/chart-tooltip).
export function TurnRail({ turns, running }: { turns: Turn[]; running: boolean }) {
  return (
    <nav className="grid content-start gap-1" aria-label="Turns">
      {turns.map((t) => {
        const prompt = t.prompts.find((p) => p.kind === "user") ?? t.prompts[0];
        const preview = prompt
          ? oneLine(prompt.text, 200)
          : t.prompts.length
            ? "(continues)"
            : "(no prompt captured)";
        const span = t.open && running ? "in progress" : spanLabel(turnDurationMs(t));
        return (
          <a
            key={t.n}
            href={`#turn-${t.n}`}
            className="grid grid-cols-[22px_minmax(0,1fr)] gap-2 overflow-hidden rounded-md border border-transparent px-2 py-1.5 text-xs text-[var(--color-fg-2)] hover:bg-[var(--color-bg-2)]"
            data-tip={prompt ? preview : undefined}
            // The link keeps its pointer cursor; [data-tip] resets it to the arrow.
            style={prompt ? { cursor: "pointer" } : undefined}
          >
            <span className="pt-px font-mono text-[11px] text-[var(--color-fg-muted)]">{t.n}</span>
            <span className="min-w-0">
              <span className="block truncate text-[var(--color-fg)]">{preview}</span>
              <span className="mt-0.5 block text-[11px] text-[var(--color-fg-muted)] tabular-nums">
                {fmtTime(t.start)} · {t.tools.length} tools · {t.edits.length} edits · {span}
              </span>
            </span>
          </a>
        );
      })}
    </nav>
  );
}
