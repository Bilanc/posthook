import { existsSync, realpathSync } from "node:fs";
import { dirname, join, relative, sep } from "node:path";

// Walk up from a starting directory looking for a .git entry.
// Returns the repo root or null. Handles both .git directories and .git files (for worktrees/submodules).
// Result is always canonicalized via realpathSync so that symlinked paths (e.g. macOS /var → /private/var,
// or tmp shadowing) resolve to a stable identity. Without this, ingest and blame can register the same
// physical repo under two different keys and fail to join.
export function findRepoRoot(start: string): string | null {
  let cur = start;
  for (let i = 0; i < 64; i++) {
    if (existsSync(join(cur, ".git"))) {
      try {
        return realpathSync(cur);
      } catch {
        return cur;
      }
    }
    const parent = dirname(cur);
    if (parent === cur) return null;
    cur = parent;
  }
  return null;
}

// Canonicalize a path that's already known to be a repo root. Used at lookup time when we have
// a path from a command argument (e.g. `--repo-root`) and want to match what's stored in the DB.
export function canonicalize(path: string): string {
  try {
    return realpathSync(path);
  } catch {
    return path;
  }
}

// Env vars for any execSync/spawn call that invokes `git`. POSTHOOK_BYPASS=1 tells our
// git shadow proxy (if installed) to pass through without running its capture hooks —
// otherwise our own ingest path would recurse through the proxy every time it ran a
// git query and call ingest again, eventually blowing the stack.
export function gitBypassEnv(): NodeJS.ProcessEnv {
  return { ...process.env, POSTHOOK_BYPASS: "1" };
}

// Compute a path's location relative to a repo root.
// Returns null if the path is outside the repo.
export function relPathInRepo(repoRoot: string, absPath: string): string | null {
  const r = relative(repoRoot, absPath);
  if (r.startsWith("..") || r.startsWith(`..${sep}`)) return null;
  return r;
}
