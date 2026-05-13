import { existsSync } from "node:fs";
import { readFile } from "node:fs/promises";
import { computeCost, type TokenCounts } from "./pricing.ts";

// Claude Code stores each session as JSONL at ~/.claude/projects/<encoded-cwd>/<session-id>.jsonl
// Each line is a record like { type: "assistant", message: { model, usage: { input_tokens, ... } } }
// We sum usage across all assistant records and return per-session totals.

export interface TranscriptSummary {
  model: string | null;
  tokens: TokenCounts;
  cost_usd: number;
  assistant_message_count: number;
  first_ts: string | null;
  last_ts: string | null;
}

interface AssistantRecord {
  type?: string;
  timestamp?: string;
  message?: {
    model?: string;
    usage?: {
      input_tokens?: number;
      output_tokens?: number;
      cache_creation_input_tokens?: number;
      cache_read_input_tokens?: number;
    };
  };
}

export async function parseClaudeTranscript(path: string): Promise<TranscriptSummary | null> {
  if (!existsSync(path)) return null;
  const raw = await readFile(path, "utf8");
  const lines = raw.split("\n");

  const tokens: TokenCounts = { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 };
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
    const u = rec.message.usage;
    if (u) {
      tokens.input += u.input_tokens ?? 0;
      tokens.output += u.output_tokens ?? 0;
      tokens.cacheRead += u.cache_read_input_tokens ?? 0;
      tokens.cacheWrite += u.cache_creation_input_tokens ?? 0;
    }
    if (rec.message.model) {
      modelCounts.set(rec.message.model, (modelCounts.get(rec.message.model) ?? 0) + 1);
    }
  }

  // Pick the most-used model.
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
    tokens,
    cost_usd: computeCost(tokens, topModel),
    assistant_message_count: assistantCount,
    first_ts: firstTs,
    last_ts: lastTs,
  };
}
