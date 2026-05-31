import { existsSync, readFileSync } from "node:fs";

// Ported from posthook/src/transcript.ts. Keeps posthook-dash independent of
// posthook source. If posthook's parser gains features, port them here.

interface ClaudeRecord {
  type?: string;
  timestamp?: string;
  message?: {
    role?: string;
    content?: unknown;
  };
}

export interface PromptEntry {
  ts: string;
  text: string;
}

// Pull all user prompts (in timestamp order) from a Claude Code transcript JSONL.
// Returns empty array on missing file / parse failure.
export function readPrompts(transcriptPath: string): PromptEntry[] {
  if (!existsSync(transcriptPath)) return [];
  let raw: string;
  try {
    raw = readFileSync(transcriptPath, "utf8");
  } catch {
    return [];
  }
  const prompts: PromptEntry[] = [];
  for (const line of raw.split("\n")) {
    if (!line.trim()) continue;
    let rec: ClaudeRecord;
    try {
      rec = JSON.parse(line);
    } catch {
      continue;
    }
    if (rec.type !== "user" || !rec.message || rec.message.role !== "user") continue;
    if (!rec.timestamp) continue;
    const text = stringifyContent(rec.message.content);
    if (!text) continue;
    prompts.push({ ts: rec.timestamp, text });
  }
  prompts.sort((a, b) => a.ts.localeCompare(b.ts));
  return prompts;
}

function stringifyContent(content: unknown): string | null {
  if (typeof content === "string") return content.trim() || null;
  if (Array.isArray(content)) {
    const parts: string[] = [];
    for (const block of content) {
      if (block && typeof block === "object" && !Array.isArray(block)) {
        const b = block as { type?: string; text?: string };
        if (b.type === "text" && typeof b.text === "string") {
          parts.push(b.text);
        }
      }
    }
    const joined = parts.join("\n").trim();
    return joined.length > 0 ? joined : null;
  }
  return null;
}
