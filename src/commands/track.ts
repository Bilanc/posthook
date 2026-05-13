import { resolve } from "node:path";
import { detectBinaryPath } from "../installers/base.ts";
import { installRepoHook } from "../installers/git_hook.ts";
import { info } from "../util/log.ts";

export async function runTrack(repoPathArg: string, binaryPath?: string): Promise<void> {
  const repoPath = resolve(repoPathArg);
  const bin = binaryPath ?? detectBinaryPath();
  const result = await installRepoHook(repoPath, bin);
  info(result.message);
}
