#!/usr/bin/env node
import { spawn } from "node:child_process";
import { existsSync } from "node:fs";
import { homedir } from "node:os";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const __dirname = dirname(fileURLToPath(import.meta.url));
const root = resolve(__dirname, "..");
const standaloneServer = resolve(root, ".next/standalone/server.js");

const dbPath = process.env.POSTHOOK_DB ?? resolve(homedir(), ".posthook", "posthook.db");
const port = process.env.POSTHOOK_DASH_PORT ?? "3847";
const hostname = process.env.POSTHOOK_DASH_HOSTNAME ?? "127.0.0.1";

if (!existsSync(dbPath)) {
  console.error(`posthook-dash: database not found at ${dbPath}`);
  console.error("  Run posthook first to capture some events, then try again.");
  console.error("  Or set POSTHOOK_DB to point at an existing posthook.db.");
  process.exit(1);
}

if (!existsSync(standaloneServer)) {
  console.error(`posthook-dash: standalone build not found at ${standaloneServer}`);
  console.error("  Did you forget to run `npm run build`?");
  process.exit(1);
}

const url = `http://${hostname}:${port}`;
console.log(`posthook-dash`);
console.log(`  db:   ${dbPath}`);
console.log(`  url:  ${url}`);
console.log();

// --disable-warning silences node:sqlite's ExperimentalWarning (the DB driver);
// it's a stable enough API for our read-only use and the notice only confuses.
const child = spawn(process.execPath, ["--disable-warning=ExperimentalWarning", standaloneServer], {
  cwd: root,
  env: { ...process.env, POSTHOOK_DB: dbPath, PORT: port, HOSTNAME: hostname },
  stdio: "inherit",
});

// Forward common termination signals.
for (const sig of ["SIGINT", "SIGTERM", "SIGHUP"]) {
  process.on(sig, () => {
    if (!child.killed) child.kill(sig);
  });
}

child.on("exit", (code, signal) => {
  if (signal) process.kill(process.pid, signal);
  else process.exit(code ?? 0);
});
