// Package service manages the background posthook sync daemon — the long-lived
// process that runs `posthook sync --loop` so a connected machine keeps flushing
// local AI-usage data to the cloud without anyone remembering to.
//
// It installs an OS-native supervised unit so the daemon starts at login,
// restarts on failure, and survives reboots:
//
//   - macOS: a launchd user agent at ~/Library/LaunchAgents/<label>.plist
//   - Linux: a systemd --user unit at ~/.config/systemd/user/<unit>
//
// install.sh calls Install when a team install key is present. Everything is
// idempotent — re-running Install reloads the unit in place.
package service

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/bilanc/posthook/internal/paths"
)

const (
	// launchdLabel is the launchd job label (reverse-DNS, per convention) and
	// systemdUnit is the systemd --user unit name. Both target `posthook sync
	// --loop` against the installed binary.
	launchdLabel = "co.bilanc.posthook-sync"
	systemdUnit  = "posthook-sync.service"
)

// syncLogPath is where the daemon's stdout/stderr land (~/.posthook/sync.log),
// next to the dashboard's dash.log.
func syncLogPath() string { return filepath.Join(paths.PosthookDir(), "sync.log") }

// binaryPath is the absolute path to the running posthook binary, hard-coded
// into the unit file as the ExecStart / ProgramArguments target.
func binaryPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locating posthook binary: %w", err)
	}
	return exe, nil
}

// Install writes and loads the platform unit, starting the daemon immediately.
func Install() error {
	if err := os.MkdirAll(paths.PosthookDir(), 0o755); err != nil {
		return err
	}
	switch runtime.GOOS {
	case "darwin":
		return installLaunchd()
	case "linux":
		return installSystemd()
	default:
		return fmt.Errorf(
			"background sync isn't auto-managed on %s yet — run `posthook sync --loop` yourself (e.g. a cron @reboot job or your init system)",
			runtime.GOOS)
	}
}

// Uninstall stops the daemon and removes the unit. Safe to call when nothing is
// installed.
func Uninstall() error {
	switch runtime.GOOS {
	case "darwin":
		return uninstallLaunchd()
	case "linux":
		return uninstallSystemd()
	default:
		return nil
	}
}

// Status returns a one-line human-readable state for `posthook service status`.
func Status() string {
	switch runtime.GOOS {
	case "darwin":
		if !fileExists(launchdPlistPath()) {
			return "not installed (no launchd agent)"
		}
		if exec.Command("launchctl", "list", launchdLabel).Run() == nil {
			return "installed and loaded (launchd: " + launchdLabel + ")"
		}
		return "installed but not loaded — run `posthook service install` to (re)load it"
	case "linux":
		if !fileExists(systemdUnitPath()) {
			return "not installed (no systemd --user unit)"
		}
		out, _ := exec.Command("systemctl", "--user", "is-active", systemdUnit).Output()
		return "installed (systemd --user " + systemdUnit + "): " + strings.TrimSpace(string(out))
	default:
		return "unsupported on " + runtime.GOOS
	}
}

// --- macOS / launchd ---------------------------------------------------------

func launchdPlistPath() string {
	return filepath.Join(paths.Home(), "Library", "LaunchAgents", launchdLabel+".plist")
}

func installLaunchd() error {
	exe, err := binaryPath()
	if err != nil {
		return err
	}
	log := syncLogPath()
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>sync</string>
		<string>--loop</string>
	</array>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><true/>
	<key>ProcessType</key><string>Background</string>
	<key>StandardOutPath</key><string>%s</string>
	<key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, launchdLabel, exe, log, log)

	path := launchdPlistPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
		return err
	}

	domain := "gui/" + strconv.Itoa(os.Getuid())
	// Reload cleanly: bootout any existing instance (ignore "not found"), then
	// bootstrap the freshly written plist.
	_ = exec.Command("launchctl", "bootout", domain+"/"+launchdLabel).Run()
	if out, err := exec.Command("launchctl", "bootstrap", domain, path).CombinedOutput(); err != nil {
		// Older macOS / non-GUI sessions: fall back to the legacy load -w.
		_ = exec.Command("launchctl", "unload", path).Run()
		if out2, err2 := exec.Command("launchctl", "load", "-w", path).CombinedOutput(); err2 != nil {
			return fmt.Errorf("launchctl could not load the agent (bootstrap: %s; load: %s)",
				strings.TrimSpace(string(out)), strings.TrimSpace(string(out2)))
		}
	}
	// RunAtLoad starts it, but kickstart guarantees a running instance even on a
	// reload where it was already loaded.
	_ = exec.Command("launchctl", "kickstart", domain+"/"+launchdLabel).Run()
	return nil
}

func uninstallLaunchd() error {
	domain := "gui/" + strconv.Itoa(os.Getuid())
	_ = exec.Command("launchctl", "bootout", domain+"/"+launchdLabel).Run()
	_ = exec.Command("launchctl", "unload", launchdPlistPath()).Run()
	if err := os.Remove(launchdPlistPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// --- Linux / systemd --user --------------------------------------------------

func systemdUnitPath() string {
	return filepath.Join(paths.Home(), ".config", "systemd", "user", systemdUnit)
}

func installSystemd() error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return fmt.Errorf("systemd (systemctl) not found — run `posthook sync --loop` yourself (e.g. a cron @reboot job)")
	}
	exe, err := binaryPath()
	if err != nil {
		return err
	}
	unit := fmt.Sprintf(`[Unit]
Description=posthook cloud sync (flushes local AI-usage data upstream)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s sync --loop
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`, exe)

	path := systemdUnitPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
		return err
	}

	userctl := func(args ...string) error {
		out, err := exec.Command("systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("systemctl --user %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
		}
		return nil
	}
	if err := userctl("daemon-reload"); err != nil {
		return err
	}
	if err := userctl("enable", "--now", systemdUnit); err != nil {
		return err
	}
	// Best-effort: keep the --user instance running across logout/reboot without
	// an active login session. Often needs polkit/root, so never fail on it.
	if u, err := user.Current(); err == nil {
		_ = exec.Command("loginctl", "enable-linger", u.Username).Run()
	}
	return nil
}

func uninstallSystemd() error {
	if _, err := exec.LookPath("systemctl"); err == nil {
		_ = exec.Command("systemctl", "--user", "disable", "--now", systemdUnit).Run()
	}
	if err := os.Remove(systemdUnitPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	return nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
