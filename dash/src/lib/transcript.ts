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

// One API message's usage, from a Claude Code transcript (one record per
// assistant message, deduped by message id) or a Codex rollout (token_count
// events are cumulative; each entry here is the delta from the previous one).
export interface UsageEntry {
  ts: string;
  model: string | null;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_creation_tokens: number;
  thinking_tokens: number | null;
  thinking_ms: number | null;
}

interface ClaudeUsageRecord {
  type?: string;
  timestamp?: string;
  thinkingDurationMs?: number;
  message?: {
    id?: string;
    model?: string;
    usage?: {
      input_tokens?: number;
      output_tokens?: number;
      cache_read_input_tokens?: number;
      cache_creation_input_tokens?: number;
      output_tokens_details?: { thinking_tokens?: number };
    };
  };
  payload?: {
    type?: string;
    info?: {
      total_token_usage?: {
        input_tokens?: number;
        cached_input_tokens?: number;
        output_tokens?: number;
        reasoning_output_tokens?: number;
      };
    };
  };
}

// Per-message usage in timestamp order. Returns null when the file is missing
// or carries no usage at all (Cursor), so callers can tell "unknown" from zero.
export function readUsage(transcriptPath: string): UsageEntry[] | null {
  if (!existsSync(transcriptPath)) return null;
  let raw: string;
  try {
    raw = readFileSync(transcriptPath, "utf8");
  } catch {
    return null;
  }
  const out: UsageEntry[] = [];
  const seen = new Set<string>();
  let codexPrev: { input: number; cached: number; output: number; reasoning: number } | null = null;
  for (const line of raw.split("\n")) {
    if (!line.trim()) continue;
    let rec: ClaudeUsageRecord;
    try {
      rec = JSON.parse(line);
    } catch {
      continue;
    }
    if (!rec.timestamp) continue;
    // Claude Code: one API message is repeated across several lines (one per
    // content block), each carrying the same usage — count it once.
    if (rec.type === "assistant" && rec.message?.usage) {
      const id = rec.message.id ?? `${rec.timestamp}-${out.length}`;
      if (seen.has(id)) continue;
      seen.add(id);
      const u = rec.message.usage;
      out.push({
        ts: rec.timestamp,
        model: rec.message.model ?? null,
        input_tokens: u.input_tokens ?? 0,
        output_tokens: u.output_tokens ?? 0,
        cache_read_tokens: u.cache_read_input_tokens ?? 0,
        cache_creation_tokens: u.cache_creation_input_tokens ?? 0,
        thinking_tokens: u.output_tokens_details?.thinking_tokens ?? null,
        thinking_ms: rec.thinkingDurationMs ?? null,
      });
      continue;
    }
    // Codex: cumulative totals per token_count event; emit the delta.
    if (rec.type === "event_msg" && rec.payload?.type === "token_count" && rec.payload.info?.total_token_usage) {
      const tu = rec.payload.info.total_token_usage;
      const cur = {
        input: tu.input_tokens ?? 0,
        cached: tu.cached_input_tokens ?? 0,
        output: tu.output_tokens ?? 0,
        reasoning: tu.reasoning_output_tokens ?? 0,
      };
      const prev = codexPrev ?? { input: 0, cached: 0, output: 0, reasoning: 0 };
      const dOut = cur.output - prev.output;
      const dIn = cur.input - prev.input;
      codexPrev = cur;
      if (dOut <= 0 && dIn <= 0) continue;
      out.push({
        ts: rec.timestamp,
        model: null,
        input_tokens: Math.max(0, dIn - (cur.cached - prev.cached)),
        output_tokens: Math.max(0, dOut),
        cache_read_tokens: Math.max(0, cur.cached - prev.cached),
        cache_creation_tokens: 0,
        thinking_tokens: Math.max(0, cur.reasoning - prev.reasoning),
        thinking_ms: null,
      });
    }
  }
  if (out.length === 0) return null;
  out.sort((a, b) => a.ts.localeCompare(b.ts));
  return out;
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
