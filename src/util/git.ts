import { existsSync } from "node:fs";
import { dirname, join, relative, sep } from "node:path";

// Walk up from a starting directory looking for a .git entry.
// Returns the repo root or null. Handles both .git directories and .git files (for worktrees/submodules).
export function findRepoRoot(start: string): string | null {
  let cur = start;
  for (let i = 0; i < 64; i++) {
    if (existsSync(join(cur, ".git"))) return cur;
    const parent = dirname(cur);
    if (parent === cur) return null;
    cur = parent;
  }
  return null;
}

// Compute a path's location relative to a repo root.
// Returns null if the path is outside the repo.
export function relPathInRepo(repoRoot: string, absPath: string): string | null {
  const r = relative(repoRoot, absPath);
  if (r.startsWith("..") || r.startsWith(`..${sep}`)) return null;
  return r;
}
