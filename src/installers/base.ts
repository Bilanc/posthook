import { existsSync } from "node:fs";
import { readFile } from "node:fs/promises";
import { POSTHOOK_MARKER } from "../config.ts";
import { writeAtomic } from "../util/atomic.ts";

export interface InstallResult {
  changed: boolean;
  path: string;
  message: string;
}

export async function readJsonOrEmpty(path: string): Promise<Record<string, unknown>> {
  if (!existsSync(path)) return {};
  const raw = await readFile(path, "utf8");
  if (raw.trim() === "") return {};
  const parsed = JSON.parse(raw);
  if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
    throw new Error(`Expected ${path} to contain a JSON object`);
  }
  return parsed as Record<string, unknown>;
}

export async function writeJsonAtomic(path: string, value: unknown): Promise<void> {
  await writeAtomic(path, JSON.stringify(value, null, 2) + "\n");
}

export function isPosthookCommand(cmd: string, agentSlug: string): boolean {
  return cmd.includes(POSTHOOK_MARKER) && cmd.includes(agentSlug);
}

export function posthookCommandFor(binaryPath: string, agentSlug: string): string {
  return `${binaryPath} ingest --agent ${agentSlug}`;
}

export function detectBinaryPath(): string {
  const fromEnv = process.env.POSTHOOK_BIN;
  if (fromEnv) return fromEnv;
  // process.execPath when running via `bun bin/posthook.ts` is bun itself,
  // not useful as a hook command. When running a compiled binary, argv[0] /
  // process.execPath points at the binary.
  // For dev install we rely on POSTHOOK_BIN; for production we use argv[0].
  return process.execPath;
}
