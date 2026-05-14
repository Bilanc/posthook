import { execSync } from "node:child_process";
import { existsSync, readFileSync, writeFileSync, mkdirSync, statSync, realpathSync } from "node:fs";
import { basename, join } from "node:path";
import { POSTHOOK_DIR } from "../config.ts";

const GIT_PATH_FILE = join(POSTHOOK_DIR, "git-path");

// Hardcoded fallbacks in order of preference. Hit if the saved path is missing
// and PATH lookup also fails. /usr/bin/git is the macOS default; /opt/homebrew
// is brew on Apple Silicon; /usr/local is brew on Intel + many Linux setups.
const FALLBACK_PATHS = [
  "/usr/bin/git",
  "/opt/homebrew/bin/git",
  "/usr/local/bin/git",
];

// Read the saved real-git path. Returns null if not yet detected.
export function loadRealGitPath(): string | null {
  if (!existsSync(GIT_PATH_FILE)) return null;
  try {
    const p = readFileSync(GIT_PATH_FILE, "utf8").trim();
    return p.length > 0 && existsSync(p) ? p : null;
  } catch {
    return null;
  }
}

// Persist the detected real-git path so the proxy can find it without re-scanning PATH on every call.
export function saveRealGitPath(path: string): void {
  mkdirSync(POSTHOOK_DIR, { recursive: true });
  writeFileSync(GIT_PATH_FILE, path + "\n");
}

// Locate the real git binary, skipping any that resolve to our own posthook binary.
// `posthookExecPath` is the absolute path to the posthook binary so we can recognize
// symlinks that point at it. Returns null if no suitable git is found.
export function detectRealGitPath(posthookExecPath: string): string | null {
  const ourReal = safeRealpath(posthookExecPath);

  // `which -a` is portable and returns each PATH match on its own line.
  let candidates: string[] = [];
  try {
    const out = execSync("which -a git 2>/dev/null", { encoding: "utf8" });
    candidates = out
      .split("\n")
      .map((s) => s.trim())
      .filter((s) => s.length > 0);
  } catch {
    // which not available or no git on PATH; fall through to FALLBACK_PATHS
  }

  for (const candidate of [...candidates, ...FALLBACK_PATHS]) {
    if (!existsSync(candidate)) continue;
    const real = safeRealpath(candidate);
    if (real === ourReal) continue; // skip our own shadow
    // Skip any other binary-shadow wrapper. A real git binary, after symlink
    // resolution, has basename "git". Wrappers like git-ai resolve to
    // "/.../.git-ai/bin/git-ai" — different basename, so we filter them out
    // and fall through to system paths.
    if (basename(real) !== "git") continue;
    if (!isExecutableFile(real)) continue;
    return real;
  }
  return null;
}

// Resolve a path through symlinks. Returns the input on failure so callers can
// still compare strings instead of branching.
function safeRealpath(path: string): string {
  try {
    return realpathSync(path);
  } catch {
    return path;
  }
}

function isExecutableFile(path: string): boolean {
  try {
    const st = statSync(path);
    if (!st.isFile()) return false;
    // We can't easily check the X bit cross-platform without fs.access, but if it exists
    // and is a file at a git-binary-looking path, that's enough for our purposes.
    return true;
  } catch {
    return false;
  }
}

// Resolve the real-git path for proxy/internal use. Caches in-process.
let cached: string | null | undefined;
export function resolveRealGitPath(): string | null {
  if (cached !== undefined) return cached;
  const saved = loadRealGitPath();
  if (saved) {
    cached = saved;
    return cached;
  }
  // No saved path yet — fall back to a fresh detection using our own exec path
  // (the binary running right now is the posthook binary).
  cached = detectRealGitPath(process.execPath);
  return cached;
}
