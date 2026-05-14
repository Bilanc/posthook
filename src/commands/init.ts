import { detectBinaryPath, type InstallResult } from "../installers/base.ts";
import { detectClaudeCode, installClaudeCodeHooks } from "../installers/claude_code.ts";
import { detectCodex, installCodexHooks } from "../installers/codex.ts";
import { detectCursor, installCursorHooks } from "../installers/cursor.ts";
import { installGlobalGitTemplate } from "../installers/git_hook.ts";
import { openDb } from "../store.ts";
import { info, warn } from "../util/log.ts";
import { runInstallShadow } from "./installShadow.ts";

interface InitOptions {
  binaryPath?: string;
}

export async function runInit(opts: InitOptions = {}): Promise<void> {
  openDb(); // ensure ~/.posthook/ exists and DB is initialized
  const binaryPath = opts.binaryPath ?? detectBinaryPath();
  info(`Using binary path: ${binaryPath}`);
  info("");

  const results: InstallResult[] = [];

  const installs: Array<[string, () => Promise<boolean>, () => Promise<InstallResult>]> = [
    ["Claude Code", detectClaudeCode, () => installClaudeCodeHooks(binaryPath)],
    ["Cursor", detectCursor, () => installCursorHooks(binaryPath)],
    ["Codex CLI", detectCodex, () => installCodexHooks(binaryPath)],
  ];

  info("AI agent hooks:");
  for (const [name, detect, install] of installs) {
    try {
      const present = await detect();
      if (!present) {
        info(`  ${name}: not detected, skipping`);
        continue;
      }
      results.push(await install());
    } catch (err) {
      warn(`${name}: ${err instanceof Error ? err.message : String(err)}`);
    }
  }
  for (const r of results) info(`  ${r.message}`);
  info("");

  // Git shadow: the primary mechanism for capturing commits across all repos
  // without per-repo setup. Install before the templateDir fallback so users
  // see the preferred path first.
  info("Git shadow:");
  try {
    await runInstallShadow();
  } catch (err) {
    warn(`shadow install failed: ${err instanceof Error ? err.message : String(err)}`);
    warn(
      `Falling back to per-repo hooks only. Use \`posthook track <path>\` for each repo, or fix the shadow and re-run \`posthook install-shadow\`.`,
    );
  }
  info("");

  // templateDir fallback: covers `git init` in new repos even if the shadow is
  // disabled or removed. Coexists safely with the shadow because commit ingest
  // is idempotent via UNIQUE(repo_id, sha).
  info("Git template (fallback for new repos):");
  try {
    const r = await installGlobalGitTemplate(binaryPath);
    info(`  ${r.message}`);
  } catch (err) {
    warn(`Git template: ${err instanceof Error ? err.message : String(err)}`);
  }
  info("");
  info("Done. Track existing repos with: posthook track <path>");
  info("Verify with:                     posthook status");
}
