import { execSync } from "node:child_process";
import {
  existsSync,
  lstatSync,
  readlinkSync,
  realpathSync,
  symlinkSync,
  unlinkSync,
} from "node:fs";
import { dirname, join } from "node:path";
import { detectRealGitPath, loadRealGitPath, saveRealGitPath } from "../proxy/realGit.ts";
import { gitBypassEnv } from "../util/git.ts";
import { info, warn } from "../util/log.ts";

// Health snapshot of the git shadow installation. Used by `posthook status` to
// surface PATH-ordering problems, since a silently-wrong PATH means we capture
// nothing for git commits and the user has no idea.
export interface ShadowHealth {
  // True if anything indicates the user intended to install the shadow — either
  // the symlink exists or we have a saved real-git path. We only warn when this
  // is true; otherwise the user is using the per-repo fallback intentionally.
  attempted: boolean;
  shadowPath: string | null;       // expected symlink location, if computable
  symlinkExists: boolean;          // is the symlink actually there
  symlinkValid: boolean;           // does it point at our posthook binary
  whichGit: string | null;         // what shell PATH lookup returns for `git`
  winning: boolean;                // does which-git match our shadow
  savedRealGit: string | null;     // contents of ~/.posthook/git-path
}

export function checkShadowHealth(): ShadowHealth {
  const posthookPath = resolvePosthookBinaryPath();
  const shadowPath = posthookPath ? join(dirname(posthookPath), "git") : null;

  let symlinkExists = false;
  let symlinkValid = false;
  if (shadowPath && existsSync(shadowPath)) {
    symlinkExists = true;
    try {
      const st = lstatSync(shadowPath);
      if (st.isSymbolicLink()) {
        const target = readlinkSync(shadowPath);
        symlinkValid = posthookPath !== null && (target === posthookPath || sameFile(target, posthookPath));
      }
    } catch {
      // ignore; symlinkValid stays false
    }
  }

  const whichGit = resolveWhichGit();
  const winning = !!(shadowPath && whichGit && sameFile(whichGit, shadowPath));
  const savedRealGit = loadRealGitPath();

  return {
    attempted: symlinkExists || savedRealGit !== null,
    shadowPath,
    symlinkExists,
    symlinkValid,
    whichGit,
    winning,
    savedRealGit,
  };
}

// Install posthook as a `git` shadow: a symlink alongside the posthook binary
// named `git`, so any `git` command on PATH hits us first. We then proxy to
// the real git and capture relevant subcommands.
//
// Idempotent: safe to re-run. If the symlink already points at our binary,
// only the saved real-git path is refreshed.
export async function runInstallShadow(): Promise<void> {
  const posthookPath = resolvePosthookBinaryPath();
  if (!posthookPath) {
    process.stderr.write(
      "posthook: cannot determine posthook binary path. Set POSTHOOK_BIN to an absolute path and retry.\n",
    );
    process.exit(1);
  }
  const installDir = dirname(posthookPath);
  const shadowPath = join(installDir, "git");

  // Detect real git BEFORE we create the symlink — otherwise `which -a git`
  // would include our shadow and we'd have to filter it out anyway. Doing it
  // first is simpler and avoids edge cases.
  const realGit = detectRealGitPath(posthookPath);
  if (!realGit) {
    process.stderr.write(
      "posthook: no real git binary found on PATH or in fallback locations.\n" +
        "  Install git first (e.g. via Xcode CLT or Homebrew), then re-run.\n",
    );
    process.exit(1);
  }

  // Create or refresh the symlink.
  const created = ensureSymlink(shadowPath, posthookPath);
  saveRealGitPath(realGit);

  // Verify which git PATH lookup now returns — that's how downstream tools
  // (IDEs, shell aliases, scripts) will reach git. If our shadow doesn't win,
  // the install is technically successful but won't actually intercept anything.
  const whichGit = resolveWhichGit();
  const winning = whichGit ? sameFile(whichGit, shadowPath) : false;

  info(created ? `Shadow installed: ${shadowPath} → ${posthookPath}` : `Shadow already in place: ${shadowPath}`);
  info(`Real git saved:    ${realGit}`);
  if (winning) {
    info(`PATH check:        \`which git\` → ${shadowPath} ✓`);
  } else if (whichGit) {
    warn(
      `PATH check: \`which git\` → ${whichGit}, not our shadow at ${shadowPath}.\n` +
        `  Add this to your shell rc so the shadow wins:\n` +
        `    export PATH="${installDir}:$PATH"\n` +
        `  Then open a new shell and re-run \`posthook install-shadow\` to verify.`,
    );
  } else {
    warn(
      `PATH check: \`which git\` returned nothing. Ensure ${installDir} is on PATH.`,
    );
  }
}

// Remove the shadow symlink, leaving posthook and the real git binary alone.
// Idempotent: a missing or non-symlink `git` at the install dir is a no-op
// (and a warning, since we refuse to delete arbitrary files at that path).
export async function runUninstallShadow(): Promise<void> {
  const posthookPath = resolvePosthookBinaryPath();
  if (!posthookPath) {
    process.stderr.write("posthook: cannot determine posthook binary path.\n");
    process.exit(1);
  }
  const installDir = dirname(posthookPath);
  const shadowPath = join(installDir, "git");

  if (!existsSync(shadowPath)) {
    info(`No shadow found at ${shadowPath}.`);
    return;
  }
  const st = lstatSync(shadowPath);
  if (!st.isSymbolicLink()) {
    warn(
      `Refusing to remove ${shadowPath}: not a symlink. Inspect it manually before deleting.`,
    );
    return;
  }
  const target = readlinkSync(shadowPath);
  if (!sameFile(target, posthookPath) && target !== posthookPath) {
    warn(
      `Refusing to remove ${shadowPath}: symlink target is ${target}, not our posthook binary at ${posthookPath}. Inspect manually.`,
    );
    return;
  }
  unlinkSync(shadowPath);
  info(`Shadow removed: ${shadowPath}`);
  if (loadRealGitPath()) {
    info(`Saved real-git path is preserved at ~/.posthook/git-path (safe to delete if no longer wanted).`);
  }
}

// Best effort to locate the posthook binary on disk. Honors POSTHOOK_BIN for
// dev installs where process.execPath points at bun rather than a compiled
// binary.
function resolvePosthookBinaryPath(): string | null {
  const fromEnv = process.env.POSTHOOK_BIN;
  if (fromEnv && existsSync(fromEnv)) return fromEnv;
  // process.execPath is the running binary itself when we're a compiled bun
  // single-file. When running via `bun run`, it points at bun — not useful as
  // a symlink target, so POSTHOOK_BIN is required for dev installs.
  const ours = process.execPath;
  if (ours && !ours.endsWith("/bun") && !ours.endsWith("\\bun.exe")) return ours;
  return null;
}

function ensureSymlink(path: string, target: string): boolean {
  if (existsSync(path)) {
    const st = lstatSync(path);
    if (st.isSymbolicLink()) {
      const current = readlinkSync(path);
      if (current === target || sameFile(current, target)) return false;
      // Different target — replace.
      unlinkSync(path);
    } else {
      throw new Error(
        `${path} exists and is not a symlink. Move or delete it manually before installing the shadow.`,
      );
    }
  }
  symlinkSync(target, path);
  return true;
}

function sameFile(a: string, b: string): boolean {
  try {
    return realpathSync(a) === realpathSync(b);
  } catch {
    return false;
  }
}

function resolveWhichGit(): string | null {
  try {
    // `which` itself isn't subject to recursion — we just want to know what the
    // shell would resolve `git` to right now. No bypass needed; this is fast.
    const out = execSync("which git 2>/dev/null", { encoding: "utf8", env: gitBypassEnv() }).trim();
    return out.length > 0 ? out : null;
  } catch {
    return null;
  }
}
