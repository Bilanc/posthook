import { existsSync, readFileSync } from "node:fs";
import { readFile } from "node:fs/promises";

// Claude Code stores each session as JSONL at ~/.claude/projects/<encoded-cwd>/<session-id>.jsonl
// Each line is a record like { type: "assistant", message: { model, usage: { input_tokens, ... } } }
// We walk assistant records to find the top model and the timestamp span.
// Token counts are intentionally not extracted — they're only available for Claude Code
// and presenting them per-session in metrics is misleading when Cursor/Codex are zero.
// The raw transcript file still lives on disk; anything that wants tokens can re-parse it.

export interface TranscriptSummary {
  model: string | null;
  assistant_message_count: number;
  first_ts: string | null;
  last_ts: string | null;
}

interface AssistantRecord {
  type?: string;
  timestamp?: string;
  message?: {
    model?: string;
    role?: string;
    content?: unknown;
  };
}

// Pull out the most recent user-role message that arrived strictly before `beforeTs` in a
// Claude Code transcript JSONL. Used by `posthook blame` to show the prompt that triggered
// a given tool call. Returns null on missing file / parse failure / no user messages.
export function findPromptBefore(transcriptPath: string, beforeTs: string): string | null {
  if (!existsSync(transcriptPath)) return null;
  let raw: string;
  try {
    raw = readFileSync(transcriptPath, "utf8");
  } catch {
    return null;
  }
  let candidate: { ts: string; text: string } | null = null;
  for (const line of raw.split("\n")) {
    if (!line.trim()) continue;
    let rec: AssistantRecord;
    try {
      rec = JSON.parse(line);
    } catch {
      continue;
    }
    if (rec.type !== "user" || !rec.message || rec.message.role !== "user") continue;
    if (!rec.timestamp || rec.timestamp >= beforeTs) continue;
    const text = stringifyContent(rec.message.content);
    if (!text) continue;
    if (!candidate || rec.timestamp > candidate.ts) {
      candidate = { ts: rec.timestamp, text };
    }
  }
  return candidate?.text ?? null;
}

function stringifyContent(content: unknown): string | null {
  if (typeof content === "string") return content.trim() || null;
  if (Array.isArray(content)) {
    // Claude messages can be content blocks: [{type: "text", text: "..."}, {type: "tool_result", ...}]
    // For blame, we want the user's actual prompt — tool_result blocks are auto-generated noise.
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

export async function parseClaudeTranscript(path: string): Promise<TranscriptSummary | null> {
  if (!existsSync(path)) return null;
  const raw = await readFile(path, "utf8");
  return parseTranscriptString(raw);
}

export function parseClaudeTranscriptSync(path: string): TranscriptSummary | null {
  if (!existsSync(path)) return null;
  try {
    const raw = readFileSync(path, "utf8");
    return parseTranscriptString(raw);
  } catch {
    return null;
  }
}

function parseTranscriptString(raw: string): TranscriptSummary {
  const lines = raw.split("\n");

  const modelCounts = new Map<string, number>();
  let assistantCount = 0;
  let firstTs: string | null = null;
  let lastTs: string | null = null;

  for (const line of lines) {
    if (!line.trim()) continue;
    let rec: AssistantRecord;
    try {
      rec = JSON.parse(line);
    } catch {
      continue;
    }
    if (rec.timestamp) {
      if (!firstTs || rec.timestamp < firstTs) firstTs = rec.timestamp;
      if (!lastTs || rec.timestamp > lastTs) lastTs = rec.timestamp;
    }
    if (rec.type !== "assistant" || !rec.message) continue;
    assistantCount++;
    if (rec.message.model) {
      modelCounts.set(rec.message.model, (modelCounts.get(rec.message.model) ?? 0) + 1);
    }
  }

  let topModel: string | null = null;
  let topCount = 0;
  for (const [m, c] of modelCounts) {
    if (c > topCount) {
      topCount = c;
      topModel = m;
    }
  }

  return {
    model: topModel,
    assistant_message_count: assistantCount,
    first_ts: firstTs,
    last_ts: lastTs,
  };
}

