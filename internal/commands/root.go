package commands

import "github.com/spf13/cobra"

// NewRootCmd assembles the posthook cobra tree.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "posthook",
		Short:         "Local hook installer and event store",
		Long:          longHelp,
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(
		newInitCmd(),
		newInstallShadowCmd(),
		newUninstallShadowCmd(),
		newTrackCmd(),
		newIngestCmd(),
		newStatusCmd(),
		newMetricsCmd(),
		newInspectCmd(),
		newBlameCmd(),
		newDashCmd(),
		newSyncCmd(),
		newIdentityCmd(),
		newServiceCmd(),
		newVersionCmd(),
	)
	return root
}

const longHelp = `posthook — local-first instrumentation for AI-assisted coding.

Installs hooks into Claude Code, Cursor, and Codex CLI; installs a git shadow
that intercepts every git command on every repo; captures everything into a
single SQLite file at ~/.posthook/posthook.db.

Environment:
  POSTHOOK_BIN     Override the binary path written into hook configs
  POSTHOOK_DEBUG   Set to 1 for verbose stderr logging
  POSTHOOK_BYPASS  Internal: set to 1 to bypass the git shadow proxy`
