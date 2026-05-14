import { homedir } from "node:os";
import { join } from "node:path";

export const HOME = homedir();
export const POSTHOOK_DIR = join(HOME, ".posthook");
export const DB_PATH = join(POSTHOOK_DIR, "posthook.db");
export const GIT_TEMPLATE_DIR = join(POSTHOOK_DIR, "git-template");
export const LOG_DIR = join(POSTHOOK_DIR, "logs");

export const CLAUDE_SETTINGS_PATH = join(HOME, ".claude", "settings.json");
export const CURSOR_HOOKS_PATH = join(HOME, ".cursor", "hooks.json");
export const CODEX_CONFIG_PATH = join(HOME, ".codex", "config.toml");

export const POSTHOOK_MARKER = "posthook ingest";

// Git Notes ref used to carry line attribution data with the repo. Notes are not pushed
// by default; the post-commit hook self-configures push/fetch refspecs for this ref on
// remote.origin so attribution travels with the code.
export const NOTES_REF = "refs/notes/posthook";
