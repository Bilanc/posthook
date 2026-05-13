const DEBUG = process.env.POSTHOOK_DEBUG === "1";

export function info(msg: string): void {
  process.stderr.write(`${msg}\n`);
}

export function debug(msg: string): void {
  if (DEBUG) process.stderr.write(`[posthook] ${msg}\n`);
}

export function warn(msg: string): void {
  process.stderr.write(`[posthook] warning: ${msg}\n`);
}
