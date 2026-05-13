import { existsSync } from "node:fs";
import { CURSOR_HOOKS_PATH } from "../config.ts";
import {
  type InstallResult,
  isPosthookCommand,
  posthookCommandFor,
  readJsonOrEmpty,
  writeJsonAtomic,
} from "./base.ts";

const AGENT_SLUG = "cursor";
const HOOK_TYPES = ["preToolUse", "postToolUse", "beforeSubmitPrompt", "afterFileEdit"] as const;

interface HookEntry {
  command?: string;
}

export async function detectCursor(): Promise<boolean> {
  const cursorDir = CURSOR_HOOKS_PATH.replace(/\/hooks\.json$/, "");
  return existsSync(cursorDir);
}

export async function installCursorHooks(binaryPath: string): Promise<InstallResult> {
  const path = CURSOR_HOOKS_PATH;
  const before = await readJsonOrEmpty(path);
  const after = JSON.parse(JSON.stringify(before)) as Record<string, unknown>;
  const desiredCmd = posthookCommandFor(binaryPath, AGENT_SLUG);

  if (after.version === undefined) after.version = 1;

  const hooksObj = (after.hooks && typeof after.hooks === "object" && !Array.isArray(after.hooks)
    ? (after.hooks as Record<string, unknown>)
    : {}) as Record<string, HookEntry[]>;
  after.hooks = hooksObj;

  for (const hookType of HOOK_TYPES) {
    const arr: HookEntry[] = Array.isArray(hooksObj[hookType])
      ? (hooksObj[hookType] as HookEntry[])
      : [];

    const idx = arr.findIndex(
      (h) => typeof h.command === "string" && isPosthookCommand(h.command, AGENT_SLUG),
    );
    if (idx === -1) {
      arr.push({ command: desiredCmd });
    } else if (arr[idx]!.command !== desiredCmd) {
      arr[idx] = { command: desiredCmd };
    }
    // Deduplicate.
    const seen = new Set<number>();
    const deduped = arr.filter((h, i) => {
      if (typeof h.command === "string" && isPosthookCommand(h.command, AGENT_SLUG)) {
        if (seen.size > 0) return false;
        seen.add(i);
      }
      return true;
    });

    hooksObj[hookType] = deduped;
  }

  const changed = JSON.stringify(before) !== JSON.stringify(after);
  if (changed) await writeJsonAtomic(path, after);
  return {
    changed,
    path,
    message: changed ? `Cursor: hooks installed in ${path}` : `Cursor: already up to date`,
  };
}
