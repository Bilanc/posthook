import { parseArgs } from "node:util";
import { runIngest } from "./commands/ingest.ts";
import { runInit } from "./commands/init.ts";
import { runMetrics } from "./commands/metrics.ts";
import { runStatus } from "./commands/status.ts";
import { runTrack } from "./commands/track.ts";

const HELP = `posthook — local hook installer and event store

Usage:
  posthook init                          Install hooks for detected agents + global git template
  posthook track <repo-path>             Install post-commit hook in an existing repo
  posthook ingest --agent <slug>         Read agent hook payload from stdin and store it
  posthook ingest --kind git-commit \\
                  --repo-root <path> \\
                  --sha <sha>            Record a git commit
  posthook status                        Show counts and recent activity
  posthook metrics                       Show AI metrics with breakdowns by agent/model/repo
  posthook help                          Show this message

Environment:
  POSTHOOK_BIN    Override the binary path written into hook configs
  POSTHOOK_DEBUG  Set to 1 for verbose stderr logging
`;

export async function run(argv: string[]): Promise<void> {
  const [cmd, ...rest] = argv;
  if (!cmd || cmd === "help" || cmd === "--help" || cmd === "-h") {
    process.stdout.write(HELP);
    return;
  }

  switch (cmd) {
    case "init": {
      const { values } = parseArgs({
        args: rest,
        options: { bin: { type: "string" } },
        strict: false,
      });
      await runInit({ binaryPath: values.bin as string | undefined });
      return;
    }
    case "track": {
      const repoPath = rest[0];
      if (!repoPath) throw new Error("track requires a repo path");
      const { values } = parseArgs({
        args: rest.slice(1),
        options: { bin: { type: "string" } },
        strict: false,
      });
      await runTrack(repoPath, values.bin as string | undefined);
      return;
    }
    case "ingest": {
      const { values } = parseArgs({
        args: rest,
        options: {
          agent: { type: "string" },
          kind: { type: "string" },
          "repo-root": { type: "string" },
          sha: { type: "string" },
        },
        strict: false,
      });
      await runIngest({
        agent: values.agent as string | undefined,
        kind: values.kind as string | undefined,
        repoRoot: values["repo-root"] as string | undefined,
        sha: values.sha as string | undefined,
      });
      return;
    }
    case "status":
      await runStatus();
      return;
    case "metrics":
      await runMetrics();
      return;
    default:
      process.stderr.write(`Unknown command: ${cmd}\n\n${HELP}`);
      process.exit(2);
  }
}
