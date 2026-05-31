package commands

import (
	"errors"
	"path/filepath"

	"github.com/bilanc/posthook/internal/installers"
	"github.com/bilanc/posthook/internal/logx"

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
			logx.Info(res.Message)
			return nil
		},
	}
	cmd.Flags().StringVar(&binFlag, "bin", "", "Override the posthook binary path written into the hook")
	return cmd
}
