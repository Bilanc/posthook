package commands

import (
	"github.com/bilanc/posthook/internal/config"
	"github.com/bilanc/posthook/internal/logx"
	"github.com/bilanc/posthook/internal/service"

	"github.com/spf13/cobra"
)

func newServiceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Manage the background sync daemon (launchd/systemd)",
		Long: `Install, remove, or inspect the OS service that runs ` + "`posthook sync --loop`" + ` in
the background so a connected machine keeps flushing to the cloud across reboots.

  posthook service install     install + start the daemon (launchd on macOS, systemd --user on Linux)
  posthook service uninstall   stop + remove it
  posthook service status      show whether it's installed and running

Cloud sync must be configured (endpoint + token + enabled) for the daemon to do
anything — the install.sh team installer wires that up before calling this.`,
	}

	install := &cobra.Command{
		Use:   "install",
		Short: "Install and start the background sync daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := service.Install(); err != nil {
				return err
			}
			logx.Info("Background sync daemon installed and started.")
			logx.Infof("  status: %s", service.Status())

			// The daemon re-reads config every tick, so it's fine to install
			// before cloud is configured — but warn so it isn't a silent no-op.
			if cfg, err := config.Load(); err == nil {
				if !cfg.Cloud.Enabled || cfg.Cloud.Endpoint == "" || cfg.Cloud.Token == "" {
					logx.Warn("Cloud sync isn't fully configured yet (endpoint/token/enabled), so the daemon will idle until it is.")
					logx.Warn("Configure it with: posthook sync --set-endpoint URL --set-token TOKEN --set-enabled true")
				}
			}
			return nil
		},
	}

	uninstall := &cobra.Command{
		Use:   "uninstall",
		Short: "Stop and remove the background sync daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := service.Uninstall(); err != nil {
				return err
			}
			logx.Info("Background sync daemon stopped and removed.")
			return nil
		},
	}

	status := &cobra.Command{
		Use:   "status",
		Short: "Show whether the background sync daemon is installed and running",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			logx.Infof("posthook sync service: %s", service.Status())
			return nil
		},
	}

	cmd.AddCommand(install, uninstall, status)
	return cmd
}
