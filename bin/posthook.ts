#!/usr/bin/env bun
import { run } from "../src/cli.ts";

run(process.argv.slice(2)).catch((err) => {
  console.error(err instanceof Error ? err.message : String(err));
  process.exit(1);
});
