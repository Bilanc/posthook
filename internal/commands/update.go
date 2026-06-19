package commands

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
)

// installScriptURL is the canonical OSS installer. Re-running it is exactly what
// `curl -fsSL … | sh` does on a fresh machine: it downloads the latest release
// (sha256 verified), installs the binary to ~/.local/bin, refreshes the dash
// bundle, and re-runs the idempotent `posthook init`. Cloud sync config already
// lives in ~/.posthook/config.json, so a plain re-run leaves team sync intact.
const installScriptURL = "https://raw.githubusercontent.com/Bilanc/posthook/main/install.sh"

func newUpdateCmd() *cobra.Command {
	var pinVersion string
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update posthook to the latest release (re-runs the installer)",
		Long: `Update posthook in place by re-running the official installer — the same
thing as installing from curl again.

It downloads the latest release for this OS/arch (sha256 verified), replaces the
binary in ~/.local/bin, refreshes the dashboard bundle, and re-runs the
idempotent ` + "`posthook init`" + `. Your local database and cloud sync config in
~/.posthook are left untouched.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUpdate(pinVersion)
		},
	}
	cmd.Flags().StringVar(&pinVersion, "version", "", "Install a specific release version instead of the latest (e.g. 0.2.0)")
	return cmd
}

func runUpdate(pinVersion string) error {
	// Mirror install.sh's downloader preference: curl, then wget.
	var dl string
	switch {
	case commandExists("curl"):
		dl = "curl -fsSL " + installScriptURL
	case commandExists("wget"):
		dl = "wget -qO- " + installScriptURL
	default:
		return fmt.Errorf("need curl or wget to download the installer")
	}

	fmt.Printf("Updating posthook from %s\n", installScriptURL)

	sh := exec.Command("sh", "-c", dl+" | sh")
	sh.Stdin = os.Stdin
	sh.Stdout = os.Stdout
	sh.Stderr = os.Stderr
	sh.Env = os.Environ()
	if pinVersion != "" {
		// install.sh strips a leading "v", so accept either form.
		sh.Env = append(sh.Env, "POSTHOOK_VERSION="+pinVersion)
	}
	if err := sh.Run(); err != nil {
		return fmt.Errorf("update failed: %w", err)
	}
	return nil
}

func commandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}
