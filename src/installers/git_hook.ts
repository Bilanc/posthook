import { execSync } from "node:child_process";
import { existsSync } from "node:fs";
import { chmod, readFile } from "node:fs/promises";
import { join } from "node:path";
import { GIT_TEMPLATE_DIR, NOTES_REF } from "../config.ts";
import { writeAtomic } from "../util/atomic.ts";
import { gitBypassEnv } from "../util/git.ts";
import type { InstallResult } from "./base.ts";

const HOOK_MARKER = "# posthook v2";

function hookScript(binaryPath: string): string {
  // Self-healing notes transport: ensures fetch/push refspecs exist for refs/notes/posthook
  // on `origin` if they're missing. Means repos created via the global git template
  // also start syncing notes without needing `posthook track`. Safe to fail silently:
  // if `origin` doesn't exist yet, the config lines still get added and take effect once
  // `origin` is configured. No-op when the refspecs are already present.
  return `#!/bin/sh
${HOOK_MARKER}
# Captures commit metadata after every successful commit. Safe to fail silently.
"${binaryPath}" ingest --kind git-commit --repo-root "$(git rev-parse --show-toplevel)" --sha "$(git rev-parse HEAD)" >/dev/null 2>&1 || true
{
  spec='${NOTES_REF}:${NOTES_REF}'
  if ! git config --get-all remote.origin.fetch 2>/dev/null | grep -Fxq "$spec"; then
    git config --add remote.origin.fetch "$spec" 2>/dev/null || true
  fi
  if ! git config --get-all remote.origin.push 2>/dev/null | grep -Fxq "$spec"; then
    git config --add remote.origin.push "$spec" 2>/dev/null || true
  fi
} >/dev/null 2>&1 || true
`;
}

export function configureNotesTransport(repoPath: string): boolean {
  const spec = `${NOTES_REF}:${NOTES_REF}`;
  let changed = false;
  for (const key of ["remote.origin.fetch", "remote.origin.push"]) {
    let existing = "";
    try {
      existing = execSync(`git config --get-all ${key}`, {
        cwd: repoPath,
        encoding: "utf8",
        env: gitBypassEnv(),
      });
    } catch {
      // section missing — config --add will create it
    }
    if (!existing.split("\n").includes(spec)) {
      execSync(`git config --add ${key} ${JSON.stringify(spec)}`, {
        cwd: repoPath,
        env: gitBypassEnv(),
      });
      changed = true;
    }
  }
  return changed;
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
      env: gitBypassEnv(),
    }).trim();
    if (current !== GIT_TEMPLATE_DIR) {
      execSync(`git config --global init.templateDir "${GIT_TEMPLATE_DIR}"`, {
        env: gitBypassEnv(),
      });
      configChanged = true;
    }
  } catch {
    execSync(`git config --global init.templateDir "${GIT_TEMPLATE_DIR}"`, {
      env: gitBypassEnv(),
    });
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
    env: gitBypassEnv(),
  }).trim();
  const absGitDir = gitDir.startsWith("/") ? gitDir : join(repoPath, gitDir);
  const hookPath = join(absGitDir, "hooks", "post-commit");
  const hookChanged = await writeHookFile(hookPath, binaryPath);
  let notesChanged = false;
  try {
    notesChanged = configureNotesTransport(repoPath);
  } catch {
    // not fatal — the hook itself will retry the config on first commit
  }
  const changed = hookChanged || notesChanged;
  const parts: string[] = [];
  if (hookChanged) parts.push("installed");
  else parts.push("hook up to date");
  if (notesChanged) parts.push("notes transport configured");
  return {
    changed,
    path: hookPath,
    message: `Repo hook (${hookPath}): ${parts.join(", ")}`,
  };
}
