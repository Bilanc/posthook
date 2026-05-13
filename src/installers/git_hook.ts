import { execSync } from "node:child_process";
import { existsSync } from "node:fs";
import { chmod, readFile } from "node:fs/promises";
import { join } from "node:path";
import { GIT_TEMPLATE_DIR } from "../config.ts";
import { writeAtomic } from "../util/atomic.ts";
import type { InstallResult } from "./base.ts";

const HOOK_MARKER = "# posthook v1";

function hookScript(binaryPath: string): string {
  return `#!/bin/sh
${HOOK_MARKER}
# Captures commit metadata after every successful commit. Safe to fail silently.
"${binaryPath}" ingest --kind git-commit --repo-root "$(git rev-parse --show-toplevel)" --sha "$(git rev-parse HEAD)" >/dev/null 2>&1 || true
`;
}

async function writeHookFile(path: string, binaryPath: string): Promise<boolean> {
  const desired = hookScript(binaryPath);
  if (existsSync(path)) {
    const existing = await readFile(path, "utf8");
    if (existing === desired) return false;
    if (!existing.includes(HOOK_MARKER) && existing.trim() !== "") {
      // Don't clobber a user-authored hook. Append a marker noting we skipped.
      throw new Error(
        `Refusing to overwrite existing post-commit hook at ${path}. Move it aside and rerun.`,
      );
    }
  }
  await writeAtomic(path, desired);
  await chmod(path, 0o755);
  return true;
}

export async function installGlobalGitTemplate(binaryPath: string): Promise<InstallResult> {
  const hooksDir = join(GIT_TEMPLATE_DIR, "hooks");
  const hookPath = join(hooksDir, "post-commit");
  const changed = await writeHookFile(hookPath, binaryPath);

  // Wire git to use this template for `git init` going forward.
  let configChanged = false;
  try {
    const current = execSync("git config --global --get init.templateDir", {
      encoding: "utf8",
    }).trim();
    if (current !== GIT_TEMPLATE_DIR) {
      execSync(`git config --global init.templateDir "${GIT_TEMPLATE_DIR}"`);
      configChanged = true;
    }
  } catch {
    execSync(`git config --global init.templateDir "${GIT_TEMPLATE_DIR}"`);
    configChanged = true;
  }

  const any = changed || configChanged;
  return {
    changed: any,
    path: hookPath,
    message: any
      ? `Git template: hook installed at ${hookPath}; init.templateDir set`
      : `Git template: already up to date`,
  };
}

export async function installRepoHook(repoPath: string, binaryPath: string): Promise<InstallResult> {
  const gitDir = execSync("git rev-parse --git-dir", {
    cwd: repoPath,
    encoding: "utf8",
  }).trim();
  const absGitDir = gitDir.startsWith("/") ? gitDir : join(repoPath, gitDir);
  const hookPath = join(absGitDir, "hooks", "post-commit");
  const changed = await writeHookFile(hookPath, binaryPath);
  return {
    changed,
    path: hookPath,
    message: changed
      ? `Repo hook: installed at ${hookPath}`
      : `Repo hook: already up to date at ${hookPath}`,
  };
}
