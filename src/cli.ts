import { parseArgs } from "node:util";
import { runBlame } from "./commands/blame.ts";
import { runDash } from "./commands/dash.ts";
import { runIngest } from "./commands/ingest.ts";
import { runInit } from "./commands/init.ts";
import { runInspect } from "./commands/inspect.ts";
import { runInstallShadow, runUninstallShadow } from "./commands/installShadow.ts";
import { runMetrics } from "./commands/metrics.ts";
import { runStatus } from "./commands/status.ts";
import { runTrack } from "./commands/track.ts";

const HELP = `posthook — local hook installer and event store

Usage:
  posthook init                          Install agent hooks + git shadow (full coverage)
  posthook install-shadow                Install the git shadow only (idempotent)
  posthook uninstall-shadow              Remove the git shadow, restore plain git
  posthook track <repo-path>             Install post-commit hook in a single repo (fallback)
  posthook ingest --agent <slug>         Read agent hook payload from stdin and store it
  posthook ingest --kind git-commit \\
                  --repo-root <path> \\
                  --sha <sha>            Record a git commit
  posthook status                        Show counts and recent activity
  posthook metrics                       Show AI metrics with breakdowns by agent/model/repo
  posthook blame <file>                  Show per-line attribution: AI agent/session/timestamp
  posthook inspect [--agent X] \\
                   [--type Y] \\
                   [--session Z] \\
                   [--since ISO] \\
                   [--limit N]           Print recent raw event payloads (default 10)
  posthook dash                          Open the web dashboard (coming soon)
  posthook help                          Show this message

Environment:
  POSTHOOK_BIN     Override the binary path written into hook configs
  POSTHOOK_DEBUG   Set to 1 for verbose stderr logging
  POSTHOOK_BYPASS  Internal: set to 1 to bypass the git shadow proxy (prevents recursion)
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
    case "install-shadow":
      await runInstallShadow();
      return;
    case "uninstall-shadow":
      await runUninstallShadow();
      return;
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
    case "blame": {
      const file = rest[0];
      if (!file) throw new Error("blame requires a file path");
      await runBlame({ file });
      return;
    }
    case "inspect": {
      const { values } = parseArgs({
        args: rest,
        options: {
          agent: { type: "string" },
          type: { type: "string" },
          session: { type: "string" },
          since: { type: "string" },
          limit: { type: "string" },
        },
        strict: false,
      });
      const limitStr = values.limit as string | undefined;
      await runInspect({
        agent: values.agent as string | undefined,
        type: values.type as string | undefined,
        session: values.session as string | undefined,
        since: values.since as string | undefined,
        limit: limitStr ? parseInt(limitStr, 10) : undefined,
      });
      return;
    }
    case "dash":
      await runDash();
      return;
    default:
      process.stderr.write(`Unknown command: ${cmd}\n\n${HELP}`);
      process.exit(2);
  }
}
