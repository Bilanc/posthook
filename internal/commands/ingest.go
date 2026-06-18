package commands

import (
	"errors"
	"os"
	"time"

	"github.com/bilanc/posthook/internal/ingest"

	"github.com/spf13/cobra"
)

// ingestWatchdog is a hard upper bound on how long an `ingest` invocation may
// live. The agent-event path only reads stdin and writes a small spool file —
// microseconds — but a parent that holds the stdin pipe open without EOF could
// otherwise block io.ReadAll forever. The watchdog guarantees the hook process
// always exits promptly so it can never accumulate, which was the original
// CPU-pileup failure mode.
const ingestWatchdog = 3 * time.Second

func newIngestCmd() *cobra.Command {
	var (
		agent    string
		kind     string
		repoRoot string
		sha      string
	)
	cmd := &cobra.Command{
		Use:   "ingest",
		Short: "Read agent hook payload from stdin and store it (called by hooks)",
		RunE: func(cmd *cobra.Command, args []string) error {
			switch {
			case kind == "git-commit":
				if repoRoot == "" || sha == "" {
					return errors.New("--kind git-commit requires --repo-root and --sha")
				}
				// Git commits are low-frequency (once per commit), so they
				// stay synchronous — no pile-up risk, and the note write wants
				// to run in the repo.
				return ingest.GitCommit(repoRoot, sha)
			case agent != "":
				// Watchdog: never let a hook outlive a few seconds.
				time.AfterFunc(ingestWatchdog, func() { os.Exit(0) })
				if err := ingest.SpoolAgentEvent(agent); err != nil {
					return err
				}
				// Kick the background worker so the spooled event gets drained
				// into the store. Best-effort and near-free when one already
				// runs; never blocks the hook.
				ensureWorker()
				return nil
			default:
				return errors.New("ingest requires either --agent <slug> or --kind git-commit")
			}
		},
	}
	cmd.Flags().StringVar(&agent, "agent", "", "Agent slug (claude-code, cursor, codex)")
	cmd.Flags().StringVar(&kind, "kind", "", "Ingest kind (git-commit)")
	cmd.Flags().StringVar(&repoRoot, "repo-root", "", "Repo root for git-commit ingest")
	cmd.Flags().StringVar(&sha, "sha", "", "Commit SHA for git-commit ingest")
	return cmd
}
