import { existsSync } from "node:fs";
import { CLAUDE_SETTINGS_PATH } from "../config.ts";
import {
  type InstallResult,
  isPosthookCommand,
  posthookCommandFor,
  readJsonOrEmpty,
  writeJsonAtomic,
} from "./base.ts";

const AGENT_SLUG = "claude-code";
const CATCH_ALL = "*";
const HOOK_TYPES = ["PreToolUse", "PostToolUse", "SessionStart", "Stop"] as const;

interface HookEntry {
  type?: string;
  command?: string;
}
interface MatcherBlock {
  matcher?: string;
  hooks?: HookEntry[];
}

export async function detectClaudeCode(): Promise<boolean> {
  const claudeDir = CLAUDE_SETTINGS_PATH.replace(/\/settings\.json$/, "");
  return existsSync(claudeDir);
}

export async function installClaudeCodeHooks(binaryPath: string): Promise<InstallResult> {
  const path = CLAUDE_SETTINGS_PATH;
  const before = await readJsonOrEmpty(path);
  const after = JSON.parse(JSON.stringify(before)) as Record<string, unknown>;
  const desiredCmd = posthookCommandFor(binaryPath, AGENT_SLUG);

  const hooksObj = (after.hooks && typeof after.hooks === "object" && !Array.isArray(after.hooks)
    ? (after.hooks as Record<string, unknown>)
    : {}) as Record<string, MatcherBlock[]>;
  after.hooks = hooksObj;

  for (const hookType of HOOK_TYPES) {
    const blocks: MatcherBlock[] = Array.isArray(hooksObj[hookType])
      ? (hooksObj[hookType] as MatcherBlock[])
      : [];

    // Strip our command from every non-catch-all block (handles upgrades).
    for (const block of blocks) {
      const isCatchAll = block.matcher === CATCH_ALL;
      if (!isCatchAll && Array.isArray(block.hooks)) {
        block.hooks = block.hooks.filter(
          (h) => !(typeof h.command === "string" && isPosthookCommand(h.command, AGENT_SLUG)),
        );
      }
    }
    // Drop blocks left empty by our migration (but leave pre-existing empties alone).
    // Heuristic: a block with matcher !== "*" and hooks: [] that we just emptied.
    // Simpler approach: just leave them; they're harmless.

    // Find or create the catch-all block.
    let catchAll = blocks.find((b) => b.matcher === CATCH_ALL);
    if (!catchAll) {
      catchAll = { matcher: CATCH_ALL, hooks: [] };
      blocks.push(catchAll);
    }
    if (!Array.isArray(catchAll.hooks)) catchAll.hooks = [];

    // Ensure exactly one posthook command in the catch-all block.
    const existingIdx = catchAll.hooks.findIndex(
      (h) => typeof h.command === "string" && isPosthookCommand(h.command, AGENT_SLUG),
    );
    if (existingIdx === -1) {
      catchAll.hooks.push({ type: "command", command: desiredCmd });
    } else {
      const existing = catchAll.hooks[existingIdx]!;
      if (existing.command !== desiredCmd) {
        catchAll.hooks[existingIdx] = { type: "command", command: desiredCmd };
      }
      // Deduplicate any further copies.
      catchAll.hooks = catchAll.hooks.filter(
        (h, i) =>
          i === existingIdx ||
          !(typeof h.command === "string" && isPosthookCommand(h.command, AGENT_SLUG)),
      );
    }

    hooksObj[hookType] = blocks;
  }

  const changed = JSON.stringify(before) !== JSON.stringify(after);
  if (changed) await writeJsonAtomic(path, after);
  return {
    changed,
    path,
    message: changed ? `Claude Code: hooks installed in ${path}` : `Claude Code: already up to date`,
  };
}
