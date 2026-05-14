#!/usr/bin/env bun
import { basename } from "node:path";
import { run } from "../src/cli.ts";

// argv[0] dispatch: when the binary is invoked via a symlink named `git`
// (installed by `posthook install-shadow`), we proxy to the real git binary
// and capture relevant subcommands. Otherwise we run the normal posthook CLI.
//
// Important: process.argv0 preserves the original argv[0] string passed to
// exec (the symlink name), while process.argv[0] in Bun-compiled binaries
// resolves to the binary's actual path on disk. We need the original.
const invokedAs = basename(process.argv0 || process.argv[0] || "");

// User args always start at argv[2] for bun: in compiled mode argv is
// ["bun", "/$bunfs/root/posthook", ...userArgs]; in dev mode it's
// [bunPath, scriptPath, ...userArgs]. Both forms slice the same way.
const userArgs = process.argv.slice(2);

if (invokedAs === "git") {
  const { runProxy } = await import("../src/proxy/index.ts");
  await runProxy(userArgs);
  // runProxy exits internally; this is unreachable.
}

run(userArgs).catch((err) => {
  console.error(err instanceof Error ? err.message : String(err));
  process.exit(1);
});
