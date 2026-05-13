import { existsSync } from "node:fs";
import { readFile } from "node:fs/promises";
import { parse as parseToml, stringify as stringifyToml } from "smol-toml";
import { CODEX_CONFIG_PATH } from "../config.ts";
import { writeAtomic } from "../util/atomic.ts";
import { type InstallResult, isPosthookCommand, posthookCommandFor } from "./base.ts";

const AGENT_SLUG = "codex";
const HOOK_EVENTS = ["PreToolUse", "PostToolUse", "Stop"] as const;

interface HookEntry {
  type?: string;
  command?: string;
  async?: boolean;
  timeout?: number;
}
interface MatcherBlock {
  matcher?: string;
  hooks?: HookEntry[];
}

export async function detectCodex(): Promise<boolean> {
  const codexDir = CODEX_CONFIG_PATH.replace(/\/config\.toml$/, "");
  return existsSync(codexDir);
}

export async function installCodexHooks(binaryPath: string): Promise<InstallResult> {
  const path = CODEX_CONFIG_PATH;
  const raw = existsSync(path) ? await readFile(path, "utf8") : "";
  const config = (raw.trim() === "" ? {} : parseToml(raw)) as Record<string, unknown>;
  const before = JSON.stringify(config);
  const desiredCmd = posthookCommandFor(binaryPath, AGENT_SLUG);

  // Enable hooks feature flag.
  const features = (config.features as Record<string, unknown> | undefined) ?? {};
  features.hooks = true;
  delete features.codex_hooks; // migrate legacy flag
  config.features = features;

  // Ensure [hooks] table with our command in the catch-all block of each event.
  const hooksTable = (config.hooks as Record<string, MatcherBlock[]> | undefined) ?? {};
  for (const event of HOOK_EVENTS) {
    const blocks: MatcherBlock[] = Array.isArray(hooksTable[event]) ? hooksTable[event]! : [];

    // Strip posthook command from non-catch-all blocks.
    for (const block of blocks) {
      const isCatchAll = block.matcher === undefined || block.matcher === "*";
      if (!isCatchAll && Array.isArray(block.hooks)) {
        block.hooks = block.hooks.filter(
          (h) => !(typeof h.command === "string" && isPosthookCommand(h.command, AGENT_SLUG)),
        );
      }
    }

    // Find or create the catch-all block (matcher absent or "*").
    let catchAll = blocks.find((b) => b.matcher === undefined || b.matcher === "*");
    if (!catchAll) {
      catchAll = { hooks: [] };
      blocks.push(catchAll);
    }
    if (!Array.isArray(catchAll.hooks)) catchAll.hooks = [];

    // Ensure exactly one posthook command in catch-all.
    const idx = catchAll.hooks.findIndex(
      (h) => typeof h.command === "string" && isPosthookCommand(h.command, AGENT_SLUG),
    );
    if (idx === -1) {
      catchAll.hooks.push({ type: "command", command: desiredCmd });
    } else if (catchAll.hooks[idx]!.command !== desiredCmd) {
      catchAll.hooks[idx] = { type: "command", command: desiredCmd };
    }
    // Deduplicate.
    let kept = false;
    catchAll.hooks = catchAll.hooks.filter((h) => {
      if (typeof h.command === "string" && isPosthookCommand(h.command, AGENT_SLUG)) {
        if (kept) return false;
        kept = true;
      }
      return true;
    });

    hooksTable[event] = blocks;
  }
  config.hooks = hooksTable;

  const after = JSON.stringify(config);
  const changed = before !== after;
  if (changed) {
    await writeAtomic(path, stringifyToml(config));
  }
  return {
    changed,
    path,
    message: changed ? `Codex CLI: hooks installed in ${path}` : `Codex CLI: already up to date`,
  };
}
