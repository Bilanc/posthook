// Package commands provides cobra command implementations for the posthook
// CLI. The package is intentionally thin — most logic lives in dedicated
// packages (store, ingest, installers, proxy) and commands wires them up to
// cobra and handles argv/flag plumbing.
package commands

import (
	"github.com/bilanc/posthook/internal/installers"
	"github.com/bilanc/posthook/internal/logx"
	"github.com/bilanc/posthook/internal/store"

	"github.com/spf13/cobra"
)

func newInitCmd() *cobra.Command {
	var binFlag string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Install agent hooks + git shadow",
		Long:  "Install hooks for every detected AI agent, install the git shadow, and set up the global git template as a fallback.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInit(binFlag)
		},
	}
	cmd.Flags().StringVar(&binFlag, "bin", "", "Override the posthook binary path written into hook configs")
	return cmd
}

func runInit(binFlag string) error {
	// Ensure ~/.posthook/ + DB exist before any installer touches them.
	if _, err := store.Open(); err != nil {
		return err
	}

	binaryPath := binFlag
	if binaryPath == "" {
		binaryPath = installers.DetectBinaryPath()
	}
	logx.Infof("Using binary path: %s", binaryPath)
	logx.Info("")

	type installEntry struct {
		name    string
		detect  func() bool
		install func() (installers.Result, error)
	}
	entries := []installEntry{
		{"Claude Code", installers.DetectClaudeCode, func() (installers.Result, error) { return installers.InstallClaudeCodeHooks(binaryPath) }},
		{"Cursor", installers.DetectCursor, func() (installers.Result, error) { return installers.InstallCursorHooks(binaryPath) }},
		{"Codex CLI", installers.DetectCodex, func() (installers.Result, error) { return installers.InstallCodexHooks(binaryPath) }},
	}

	logx.Info("AI agent hooks:")
	var results []installers.Result
	for _, e := range entries {
		if !e.detect() {
			logx.Infof("  %s: not detected, skipping", e.name)
			continue
		}
		res, err := e.install()
		if err != nil {
			logx.Warnf("%s: %v", e.name, err)
			continue
		}
		results = append(results, res)
	}
	for _, r := range results {
		logx.Infof("  %s", r.Message)
	}
	logx.Info("")

	// Git shadow — primary mechanism for commit capture across all repos.
	logx.Info("Git shadow:")
	if err := runInstallShadow(); err != nil {
		logx.Warnf("shadow install failed: %v", err)
		logx.Warn("Falling back to per-repo hooks only. Use `posthook track <path>` for each repo, or fix the shadow and re-run `posthook install-shadow`.")
	}
	logx.Info("")

	// templateDir fallback for new repos.
	logx.Info("Git template (fallback for new repos):")
	res, err := installers.InstallGlobalGitTemplate(binaryPath)
	if err != nil {
		logx.Warnf("Git template: %v", err)
	} else {
		logx.Infof("  %s", res.Message)
	}
	logx.Info("")

	// Bring the web dashboard up in the background (best-effort; never fails init).
	logx.Info("Web dashboard:")
	autostartDashboard()
	logx.Info("")

	logx.Info("Done. Track existing repos with: posthook track <path>")
	logx.Info("Verify with:                     posthook status")
	return nil
}
