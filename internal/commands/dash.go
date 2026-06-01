package commands

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bilanc/posthook/internal/logx"
	"github.com/bilanc/posthook/internal/paths"

	"github.com/spf13/cobra"
)

const (
	defaultDashPort = "3847"
	defaultDashHost = "127.0.0.1"
)

// dashConfig resolves the host/port the dashboard binds to, honouring the same
// env vars the standalone Next.js server reads (so a manual `node server.js`
// and `posthook dash` agree on where the dashboard lives).
type dashConfig struct {
	host string
	port string
}

func resolveDashConfig() dashConfig {
	host := os.Getenv("POSTHOOK_DASH_HOSTNAME")
	if host == "" {
		host = defaultDashHost
	}
	port := os.Getenv("POSTHOOK_DASH_PORT")
	if port == "" {
		port = defaultDashPort
	}
	return dashConfig{host: host, port: port}
}

func (c dashConfig) addr() string { return net.JoinHostPort(c.host, c.port) }
func (c dashConfig) url() string  { return fmt.Sprintf("http://%s", c.addr()) }

func newDashCmd() *cobra.Command {
	var stop bool
	var noOpen bool
	cmd := &cobra.Command{
		Use:   "dash",
		Short: "Open the local web dashboard",
		Long: "Start the posthook web dashboard (if it isn't already running) and open it in your browser.\n" +
			"The dashboard is a local Next.js server reading ~/.posthook/posthook.db; it binds to\n" +
			"127.0.0.1:3847 by default (override with POSTHOOK_DASH_HOSTNAME / POSTHOOK_DASH_PORT).",
		RunE: func(cmd *cobra.Command, args []string) error {
			if stop {
				return stopDash()
			}
			return runDash(noOpen)
		},
	}
	cmd.Flags().BoolVar(&stop, "stop", false, "Stop the background dashboard server")
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "Start the server but don't open a browser")
	return cmd
}

// dashStatus reports whether ensureDashRunning found the server already up or
// had to start it.
type dashStatus int

const (
	dashAlreadyRunning dashStatus = iota
	dashStarted
)

// ensureDashRunning makes sure the dashboard server is accepting connections,
// starting the background daemon if it isn't. It does NOT open a browser — that
// is the caller's concern. Returns an error if the dashboard can't be brought
// up (not built, no Node, no DB, or it failed to bind).
func ensureDashRunning(cfg dashConfig) (dashStatus, error) {
	// Already serving (started earlier, by init, or manually)? Nothing to do.
	if portOpen(cfg.addr()) {
		return dashAlreadyRunning, nil
	}

	// Need the staged standalone build and a database to read.
	if _, err := os.Stat(paths.DashServer()); err != nil {
		return dashStarted, fmt.Errorf(
			"dashboard not built at %s\n  Re-run ./install.sh (it builds and stages the dashboard), or build it manually:\n    cd dash && npm ci && npm run build",
			paths.DashServer())
	}
	if _, err := os.Stat(paths.DBPath()); err != nil {
		return dashStarted, fmt.Errorf(
			"database not found at %s\n  Run `posthook init` and use an AI agent / make a commit to capture some events first",
			paths.DBPath())
	}

	node, err := exec.LookPath("node")
	if err != nil {
		return dashStarted, fmt.Errorf("the dashboard needs Node.js (>=24, for the built-in node:sqlite driver) on PATH, but `node` was not found. Install Node and retry")
	}

	if err := startDashDaemon(node, cfg); err != nil {
		return dashStarted, err
	}

	if !waitForPort(cfg.addr(), 30*time.Second) {
		return dashStarted, fmt.Errorf("dashboard server did not come up on %s within 30s. Check the log at %s",
			cfg.addr(), dashLogPath())
	}
	return dashStarted, nil
}

func runDash(noOpen bool) error {
	cfg := resolveDashConfig()
	status, err := ensureDashRunning(cfg)
	if err != nil {
		return err
	}
	if status == dashAlreadyRunning {
		logx.Infof("Dashboard already running at %s", cfg.url())
	} else {
		logx.Infof("Dashboard started at %s", cfg.url())
	}
	return finishDash(cfg, noOpen)
}

// autostartDashboard is called at the end of `posthook init` to bring the
// dashboard up in the background. It is best-effort and never fails init: a
// CLI-only install (no dashboard build, no Node) is a normal state, not an
// error. Set POSTHOOK_DASH_AUTOSTART=0 to skip.
func autostartDashboard() {
	if os.Getenv("POSTHOOK_DASH_AUTOSTART") == "0" {
		logx.Info("  auto-start disabled (POSTHOOK_DASH_AUTOSTART=0); launch later with: posthook dash")
		return
	}
	// Treat "not built" / "no Node" as expected on a CLI-only install — inform,
	// don't warn, and don't attempt a start that would error.
	if _, err := os.Stat(paths.DashServer()); err != nil {
		logx.Info("  dashboard not built; skipping auto-start (run ./install.sh with Node.js to enable `posthook dash`)")
		return
	}
	if _, err := exec.LookPath("node"); err != nil {
		logx.Info("  Node.js not found; skipping auto-start")
		return
	}

	cfg := resolveDashConfig()
	status, err := ensureDashRunning(cfg)
	if err != nil {
		logx.Warnf("dashboard auto-start failed: %v", err)
		return
	}
	if status == dashAlreadyRunning {
		logx.Infof("  already running at %s", cfg.url())
	} else {
		logx.Infof("  started at %s (open it with: posthook dash)", cfg.url())
	}
}

// startDashDaemon spawns the standalone Next.js server fully detached from this
// process (own session, stdio redirected to a log file) so it keeps running
// after `posthook dash` returns and the terminal closes.
func startDashDaemon(node string, cfg dashConfig) error {
	logFile, err := os.OpenFile(dashLogPath(), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("opening dashboard log: %w", err)
	}
	defer logFile.Close()

	// --disable-warning silences node:sqlite's ExperimentalWarning (the dashboard
	// DB driver) so the detached server's log stays clean.
	cmd := exec.Command(node, "--disable-warning=ExperimentalWarning", paths.DashServer())
	cmd.Dir = paths.DashDir()
	cmd.Env = append(os.Environ(),
		"POSTHOOK_DB="+paths.DBPath(),
		"PORT="+cfg.port,
		"HOSTNAME="+cfg.host,
		"NODE_ENV=production",
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	// Detach into its own session so it survives the parent shell.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting dashboard server: %w", err)
	}
	if err := os.WriteFile(paths.DashPidFile(), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644); err != nil {
		logx.Warnf("could not write dash pid file: %v", err)
	}
	// Release so we don't wait on / reap the detached child.
	return cmd.Process.Release()
}

func finishDash(cfg dashConfig, noOpen bool) error {
	if noOpen {
		logx.Infof("Open %s in your browser.", cfg.url())
		return nil
	}
	if err := openBrowser(cfg.url()); err != nil {
		logx.Warnf("could not open browser automatically: %v", err)
		logx.Infof("Open %s manually.", cfg.url())
	}
	return nil
}

func stopDash() error {
	data, err := os.ReadFile(paths.DashPidFile())
	if err != nil {
		if os.IsNotExist(err) {
			logx.Info("No dashboard server is tracked (no pid file). Nothing to stop.")
			return nil
		}
		return err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		logx.Warnf("pid file %s is corrupt; removing it.", paths.DashPidFile())
		_ = os.Remove(paths.DashPidFile())
		return nil
	}
	if proc, err := os.FindProcess(pid); err == nil {
		if sigErr := proc.Signal(syscall.SIGTERM); sigErr != nil && !errIsFinished(sigErr) {
			logx.Warnf("could not signal dashboard process %d: %v", pid, sigErr)
		} else {
			logx.Infof("Stopped dashboard server (pid %d).", pid)
		}
	}
	_ = os.Remove(paths.DashPidFile())
	return nil
}

func errIsFinished(err error) bool {
	return err == os.ErrProcessDone || err.Error() == "os: process already finished"
}

// portOpen reports whether something is accepting TCP connections at addr.
func portOpen(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func waitForPort(addr string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if portOpen(addr) {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default: // linux, bsd, ...
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}

// dashLogPath is ~/.posthook/dash.log (DashDir is ~/.posthook/dash).
func dashLogPath() string {
	return paths.DashDir() + ".log"
}
