package commands

import (
	"errors"
	"path/filepath"

	"github.com/bilanc/posthook/internal/gitx"
	"github.com/bilanc/posthook/internal/installers"
	"github.com/bilanc/posthook/internal/logx"
	"github.com/bilanc/posthook/internal/store"

	"github.com/spf13/cobra"
)

func newTrackCmd() *cobra.Command {
	var binFlag string
	cmd := &cobra.Command{
		Use:   "track <repo-path>",
		Short: "Install post-commit hook in a single repo (fallback)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			abs, err := filepath.Abs(args[0])
			if err != nil {
				return err
			}
			bin := binFlag
			if bin == "" {
				bin = installers.DetectBinaryPath()
			}
			if bin == "" {
				return errors.New("could not determine posthook binary path")
			}
			res, err := installers.InstallRepoHook(abs, bin)
			if err != nil {
				return err
			}
			// Register the repo so upgrade-time sweeps (see repairKnownRepos)
			// reach it even if it never produces an event. Best-effort.
			if db, err := store.Open(); err == nil {
				if _, err := db.EnsureRepository(gitx.Canonicalize(abs)); err != nil {
					logx.Debugf("track: register %s: %v", abs, err)
				}
			}
			logx.Info(res.Message)
			return nil
		},
	}
	cmd.Flags().StringVar(&binFlag, "bin", "", "Override the posthook binary path written into the hook")
	return cmd
}
