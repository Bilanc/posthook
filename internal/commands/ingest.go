package commands

import (
	"errors"

	"github.com/bilanc/posthook/internal/ingest"

	"github.com/spf13/cobra"
)

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
				return ingest.GitCommit(repoRoot, sha)
			case agent != "":
				return ingest.AgentEvent(agent)
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
