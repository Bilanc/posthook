"use client";

import { useState } from "react";
import { format, parseISO } from "date-fns";
import type { PromptEntry } from "@/lib/transcript";

interface Props {
  prompts: PromptEntry[];
  transcriptAvailable: boolean;
}

function fmtTs(iso: string): string {
  try {
    return format(parseISO(iso), "HH:mm:ss");
  } catch {
    return iso;
  }
}

function truncate(s: string, n = 200): string {
  if (s.length <= n) return s;
  return `${s.slice(0, n)}…`;
}

export function PromptsList({ prompts, transcriptAvailable }: Props) {
  const [expanded, setExpanded] = useState<Set<number>>(new Set());

  function toggle(i: number) {
    const next = new Set(expanded);
    if (next.has(i)) next.delete(i);
    else next.add(i);
    setExpanded(next);
  }

  if (!transcriptAvailable) {
    return (
      <div className="text-sm text-[var(--color-fg-muted)]">
        No transcript captured for this session.
        <div className="mt-1 text-xs">
          Prompts panel only renders for Claude Code sessions with a `transcript_path` event.
        </div>
      </div>
    );
  }

  if (prompts.length === 0) {
    return (
      <div className="text-sm text-[var(--color-fg-muted)]">
        Transcript found, but no user prompts in it.
      </div>
    );
  }

  return (
    <ul className="space-y-3">
      {prompts.map((p, i) => {
        const isOpen = expanded.has(i);
        const display = isOpen ? p.text : truncate(p.text);
        const isLong = p.text.length > 200;
        return (
          <li
            key={`${p.ts}-${i}`}
            className="rounded border border-[var(--color-border)] bg-[var(--color-bg)] p-3"
          >
            <div className="text-xs text-[var(--color-fg-muted)] font-mono mb-1.5">
              {fmtTs(p.ts)}
            </div>
            <div className="text-sm whitespace-pre-wrap break-words">{display}</div>
            {isLong ? (
              <button
                type="button"
                onClick={() => toggle(i)}
                className="mt-2 text-xs text-[var(--color-accent)] hover:underline"
              >
                {isOpen ? "Show less" : "Show more"}
              </button>
            ) : null}
          </li>
        );
      })}
    </ul>
  );
}
