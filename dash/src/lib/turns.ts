import { toolFamily, type ToolFamily } from "./tool-families.ts";
import type {
  PromptEntry,
  SessionCommit,
  StopEvent,
  ToolCall,
} from "./queries/turns.ts";
import type { UsageEntry } from "./transcript.ts";

// A turn is one prompt → one Stop: everything the agent did in between. Turns
// are cut at Stop events; prompts, tool calls, usage records and commits are
// bucketed into the turn whose window contains their timestamp. prompt_id on
// events does not join to session_prompts, so timestamps are the join key.

export interface TurnPrompt extends PromptEntry {
  kind: "user" | "compaction" | "resend";
}

export interface TurnToolCall extends ToolCall {
  family: ToolFamily;
}

export interface Turn {
  n: number;
  start: string; // ISO; the first real prompt in the turn, else the window start
  end: string; // ISO; the Stop timestamp (or session end for an open turn)
  open: boolean; // true when the turn has no Stop yet
  prompts: TurnPrompt[];
  tools: TurnToolCall[];
  edits: TurnToolCall[];
  families: Partial<Record<ToolFamily, number>>;
  tool_ms: number; // summed tool durations where reported
  commits: SessionCommit[];
  reply: string | null;
  effort: string | null;
  permission_mode: string | null;
  output_tokens: number | null; // null when the transcript carries no usage
  thinking_tokens: number | null;
  model_messages: number;
}

const COMPACTION_PREFIX = /^This session is being continued from a previous conversation/;
const RESEND_WINDOW_MS = 3 * 60 * 1000;
// A commit made within this long after a Stop still belongs to that turn (the
// engineer commits what the agent just wrote before prompting again).
const COMMIT_SLACK_MS = 2 * 60 * 1000;

function t(iso: string | null | undefined): number {
  return iso ? new Date(iso).getTime() : NaN;
}

export function classifyPrompts(prompts: PromptEntry[]): TurnPrompt[] {
  return prompts.map((p, i, arr) => {
    if (COMPACTION_PREFIX.test(p.text)) return { ...p, kind: "compaction" };
    const prev = arr[i - 1];
    if (
      prev &&
      prev.text === p.text &&
      p.ts &&
      prev.ts &&
      t(p.ts) - t(prev.ts) >= 0 &&
      t(p.ts) - t(prev.ts) < RESEND_WINDOW_MS
    ) {
      return { ...p, kind: "resend" };
    }
    return { ...p, kind: "user" };
  });
}

export interface BuildTurnsInput {
  started_at: string;
  ended_at: string | null;
  stops: StopEvent[];
  prompts: PromptEntry[];
  tools: ToolCall[];
  commits: SessionCommit[];
  usage: UsageEntry[] | null;
}

export function buildTurns(input: BuildTurnsInput): Turn[] {
  const prompts = classifyPrompts(input.prompts);
  const stops = [...input.stops].sort((a, b) => t(a.ts) - t(b.ts));
  const turns: Turn[] = [];
  let windowStart = input.started_at;

  const bounds: Array<{ start: string; end: string; stop: StopEvent | null }> = stops.map((s) => {
    const b = { start: windowStart, end: s.ts, stop: s };
    windowStart = s.ts;
    return b;
  });
  // Anything after the last Stop is an open turn (the session is still
  // running, or ended without a Stop hook).
  const tail = [
    ...prompts.filter((p) => p.ts && t(p.ts) > t(windowStart)),
    ...input.tools.filter((x) => t(x.ts) > t(windowStart)),
  ];
  if (tail.length > 0 || bounds.length === 0) {
    bounds.push({ start: windowStart, end: input.ended_at ?? new Date().toISOString(), stop: null });
  }

  bounds.forEach((b, i) => {
    const s = t(b.start), e = t(b.end);
    const inWin = (ts: string | null) => ts != null && t(ts) > s && t(ts) <= e;
    const firstWindow = i === 0;
    const tp = prompts.filter((p) => inWin(p.ts) || (firstWindow && p.ts != null && t(p.ts) <= s));
    const tools: TurnToolCall[] = input.tools
      .filter((x) => inWin(x.ts) || (firstWindow && t(x.ts) <= s))
      .map((x) => ({ ...x, family: x.ranged ? "edit" : toolFamily(x.tool_name) }));
    const nextStart = bounds[i + 1] ? t(bounds[i + 1].start) : Infinity;
    const commits = input.commits.filter((c) => {
      const ct = t(c.committed_at);
      return ct > s && (ct <= e + COMMIT_SLACK_MS || (ct <= nextStart && i === bounds.length - 1));
    });
    const usage = input.usage ? input.usage.filter((u) => inWin(u.ts) || (firstWindow && t(u.ts) <= s)) : null;
    const families: Partial<Record<ToolFamily, number>> = {};
    for (const x of tools) families[x.family] = (families[x.family] ?? 0) + 1;
    const realPrompt = tp.find((p) => p.kind === "user") ?? tp[0];
    turns.push({
      n: i + 1,
      start: realPrompt?.ts ?? b.start,
      end: b.end,
      open: b.stop === null,
      prompts: tp,
      tools,
      edits: tools.filter((x) => x.family === "edit"),
      families,
      tool_ms: tools.reduce((a, x) => a + (x.duration_ms ?? 0), 0),
      commits,
      reply: b.stop?.last_assistant_message ?? null,
      effort: b.stop?.effort ?? null,
      permission_mode: b.stop?.permission_mode ?? null,
      output_tokens: usage ? usage.reduce((a, u) => a + u.output_tokens, 0) : null,
      thinking_tokens: usage ? usage.reduce((a, u) => a + (u.thinking_tokens ?? 0), 0) : null,
      model_messages: usage ? usage.length : 0,
    });
  });

  return turns;
}

export function turnDurationMs(turn: Turn): number {
  return Math.max(0, t(turn.end) - t(turn.start));
}
