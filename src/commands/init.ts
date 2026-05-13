import { detectBinaryPath, type InstallResult } from "../installers/base.ts";
import { detectClaudeCode, installClaudeCodeHooks } from "../installers/claude_code.ts";
import { detectCodex, installCodexHooks } from "../installers/codex.ts";
import { detectCursor, installCursorHooks } from "../installers/cursor.ts";
import { installGlobalGitTemplate } from "../installers/git_hook.ts";
import { openDb } from "../store.ts";
import { info, warn } from "../util/log.ts";

interface InitOptions {
  binaryPath?: string;
}

export async function runInit(opts: InitOptions = {}): Promise<void> {
  openDb(); // ensure ~/.posthook/ exists and DB is initialized
  const binaryPath = opts.binaryPath ?? detectBinaryPath();
  info(`Using binary path: ${binaryPath}`);

  const results: InstallResult[] = [];

  const installs: Array<[string, () => Promise<boolean>, () => Promise<InstallResult>]> = [
    ["Claude Code", detectClaudeCode, () => installClaudeCodeHooks(binaryPath)],
    ["Cursor", detectCursor, () => installCursorHooks(binaryPath)],
    ["Codex CLI", detectCodex, () => installCodexHooks(binaryPath)],
  ];

  for (const [name, detect, install] of installs) {
    try {
      const present = await detect();
      if (!present) {
        info(`${name}: not detected, skipping`);
        continue;
      }
      results.push(await install());
    } catch (err) {
      warn(`${name}: ${err instanceof Error ? err.message : String(err)}`);
    }
  }

  try {
    results.push(await installGlobalGitTemplate(binaryPath));
  } catch (err) {
    warn(`Git template: ${err instanceof Error ? err.message : String(err)}`);
  }

  for (const r of results) info(`  ${r.message}`);
  info("");
  info("Done. Track an existing repo with: posthook track <path>");
}
