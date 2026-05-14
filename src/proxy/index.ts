import { spawn } from "node:child_process";
import { runIngest } from "../commands/ingest.ts";
import { canonicalize } from "../util/git.ts";
import { debug, warn } from "../util/log.ts";
import { resolveRealGitPath } from "./realGit.ts";

// The git proxy. When the posthook binary is invoked as `git` (via a symlink at
// ~/.local/bin/git or similar), this function takes over. It runs the real git
// binary as a child, forwards stdio and signals faithfully so the user sees
// identical behavior, and after success runs our capture logic for the commands
// we care about.
//
// Critical invariants:
//   - We MUST exit with the child's exit code (or signal-equivalent) so scripts
//     and IDE integrations see the same outcome they would running git directly.
//   - Our capture logic MUST run after git succeeds, never before. Pre-hooks
//     could block legitimate work if we have a bug.
//   - Any failure in our capture logic MUST NOT affect the user-visible exit code.
//   - If POSTHOOK_BYPASS=1 is set, we skip all capture logic and act as a pure
//     forwarder. Set by our own internal git calls to prevent recursion.
export async function runProxy(args: string[]): Promise<never> {
  const realGit = resolveRealGitPath();
  if (!realGit) {
    process.stderr.write(
      "posthook: cannot find real git binary. Run `posthook install-shadow` to re-detect, or `posthook uninstall-shadow` to remove the proxy.\n",
    );
    process.exit(127);
  }

  const bypass = process.env.POSTHOOK_BYPASS === "1";
  const subcommand = args[0];
  const interceptable =
    !bypass && subcommand !== undefined && SUBCOMMAND_HANDLERS[subcommand] !== undefined;

  const code = await spawnPassthrough(realGit, args);

  if (interceptable && code === 0) {
    const handler = SUBCOMMAND_HANDLERS[subcommand]!;
    try {
      await handler(args.slice(1));
    } catch (err) {
      // Never let capture failure affect git's exit code. Surface to stderr only
      // if debug is on; otherwise stay silent so we don't pollute scripted git
      // output that consumers might parse.
      warn(
        `proxy capture failed for ${subcommand}: ${err instanceof Error ? err.message : String(err)}`,
      );
    }
  }
  process.exit(code);
}

function spawnPassthrough(cmd: string, args: string[]): Promise<number> {
  return new Promise((resolve) => {
    const child = spawn(cmd, args, {
      stdio: "inherit",
      // Inherit env as-is. Children that themselves invoke git (e.g. submodule
      // updates during clone) re-enter our shadow, which is what we want — they
      // become independently captured. We control recursion via POSTHOOK_BYPASS,
      // set explicitly inside our own ingest path.
    });

    // Signal forwarding. The shell sends signals to the foreground process group;
    // when our proxy is foreground, we receive them and must relay to the child.
    const signals: NodeJS.Signals[] = ["SIGINT", "SIGTERM", "SIGQUIT", "SIGHUP"];
    const listeners = new Map<NodeJS.Signals, () => void>();
    for (const sig of signals) {
      const handler = () => {
        if (child.pid) {
          try {
            child.kill(sig);
          } catch {
            // child may have already exited; ignore
          }
        }
      };
      listeners.set(sig, handler);
      process.on(sig, handler);
    }

    child.on("exit", (code, signal) => {
      // Remove our signal handlers so they don't fire during our own exit path.
      for (const [sig, handler] of listeners) process.removeListener(sig, handler);
      if (signal) {
        // Convention: a process terminated by signal N exits with 128+N.
        const num = SIGNAL_NUMBERS[signal] ?? 0;
        resolve(128 + num);
      } else {
        resolve(code ?? 0);
      }
    });
    child.on("error", (err) => {
      for (const [sig, handler] of listeners) process.removeListener(sig, handler);
      process.stderr.write(`posthook: failed to spawn git: ${err.message}\n`);
      resolve(127);
    });
  });
}

// Standard POSIX signal numbers for the signals we forward. Used to compute the
// 128+N exit convention when a child terminates due to signal.
const SIGNAL_NUMBERS: Record<string, number> = {
  SIGHUP: 1,
  SIGINT: 2,
  SIGQUIT: 3,
  SIGTERM: 15,
};

// Subcommand → post-success capture handler. Keep the set tight: anything we
// add here adds latency to every successful invocation of that subcommand.
const SUBCOMMAND_HANDLERS: Record<string, (args: string[]) => Promise<void>> = {
  commit: handleCommit,
  clone: handleClone,
};

async function handleCommit(_args: string[]): Promise<void> {
  // After `git commit` succeeds, capture the new HEAD commit. We're already in
  // the user's cwd so `git rev-parse` works without --cwd plumbing.
  const repoRoot = await runRealGit(["rev-parse", "--show-toplevel"]);
  if (!repoRoot) return;
  const sha = await runRealGit(["rev-parse", "HEAD"]);
  if (!sha) return;
  await runIngest({
    kind: "git-commit",
    repoRoot: canonicalize(repoRoot),
    sha,
  });
  debug(`proxy: captured commit ${sha.slice(0, 7)}`);
}

async function handleClone(args: string[]): Promise<void> {
  // For `git clone <url> [dir]`, the destination directory is either explicit
  // or derived from the URL's basename. After clone succeeds, walk into the
  // new repo and register it so future commits already know their repo_id.
  // We don't try to backfill historical commits — only forward attribution
  // starts after clone time.
  const dest = inferCloneDest(args);
  if (!dest) return;
  const root = canonicalize(dest);
  const sha = await runRealGit(["rev-parse", "HEAD"], root);
  if (!sha) return;
  await runIngest({ kind: "git-commit", repoRoot: root, sha });
  debug(`proxy: registered clone of ${root}`);
}

// Best-effort parse of `git clone` args. Skips known flag/value pairs and treats
// the first remaining positional as the URL, the second (if present) as the
// destination. Falls back to URL-derived basename.
function inferCloneDest(args: string[]): string | null {
  // Skip the first arg if it's "clone" (caller passes args after the subcommand)
  const positional: string[] = [];
  const flagsWithValues = new Set([
    "--template",
    "-o",
    "--origin",
    "-b",
    "--branch",
    "-u",
    "--upload-pack",
    "--reference",
    "--reference-if-able",
    "--depth",
    "--shallow-since",
    "--shallow-exclude",
    "--recurse-submodules",
    "--jobs",
    "-j",
    "--server-option",
    "--separate-git-dir",
    "--filter",
    "--sparse-checkout-set",
  ]);
  for (let i = 0; i < args.length; i++) {
    const a = args[i]!;
    if (a.startsWith("--") && a.includes("=")) continue; // --key=value, ignore
    if (flagsWithValues.has(a)) {
      i++; // skip the value
      continue;
    }
    if (a.startsWith("-")) continue; // bare flag
    positional.push(a);
  }
  const url = positional[0];
  const explicit = positional[1];
  if (explicit) {
    const path = explicit.startsWith("/") ? explicit : `${process.cwd()}/${explicit}`;
    return path;
  }
  if (!url) return null;
  // Derive directory from URL: foo.git → foo; foo → foo; trailing slash trimmed.
  const tail = url.replace(/[\/:]+$/, "").split(/[\/:]+/).pop() ?? "";
  const name = tail.replace(/\.git$/, "");
  if (!name) return null;
  return `${process.cwd()}/${name}`;
}

// Run a git command using the saved real-git path, returning stdout trimmed.
// Returns null on non-zero exit. Sets POSTHOOK_BYPASS=1 so we never recurse if
// the call somehow goes back through PATH.
async function runRealGit(args: string[], cwd?: string): Promise<string | null> {
  const realGit = resolveRealGitPath();
  if (!realGit) return null;
  return await new Promise((resolve) => {
    const child = spawn(realGit, args, {
      cwd: cwd ?? process.cwd(),
      env: { ...process.env, POSTHOOK_BYPASS: "1" },
      stdio: ["ignore", "pipe", "pipe"],
    });
    let out = "";
    child.stdout?.on("data", (chunk) => (out += chunk.toString()));
    child.on("error", () => resolve(null));
    child.on("exit", (code) => resolve(code === 0 ? out.trim() : null));
  });
}
