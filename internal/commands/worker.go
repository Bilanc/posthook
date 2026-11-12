package commands

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bilanc/posthook/internal/config"
	"github.com/bilanc/posthook/internal/ingest"
	"github.com/bilanc/posthook/internal/logx"
	"github.com/bilanc/posthook/internal/paths"
	"github.com/bilanc/posthook/internal/spool"
	"github.com/bilanc/posthook/internal/store"

	"github.com/spf13/cobra"
)

const (
	// workerPollInterval is how often an idle worker re-checks the spool. The
	// check is a single readdir, so this is cheap; events still land within a
	// second or two of the hook firing.
	workerPollInterval = 1 * time.Second

	// workerIdleTimeout is how long a lazily-spawned worker keeps running with
	// an empty spool before exiting. The next hook fire re-spawns it. A managed
	// worker (--managed, run by the OS service) ignores this and stays up.
	workerIdleTimeout = 2 * time.Minute

	// workerRetentionInterval is how often a managed worker re-checks whether
	// the daily retention prune is due. The store gates the prune itself to
	// once per 24h, so the check is a single small SELECT.
	workerRetentionInterval = 1 * time.Hour
)

func workerLockPath() string { return filepath.Join(paths.PosthookDir(), "worker.lock") }
func workerLogPath() string  { return filepath.Join(paths.PosthookDir(), "worker.log") }

func newWorkerCmd() *cobra.Command {
	var managed bool
	cmd := &cobra.Command{
		Use:   "worker",
		Short: "Drain the agent-event spool into the store (background daemon)",
		Long: `Runs the background loop that drains ~/.posthook/spool into the SQLite store.

The agent hooks only write events to the spool — fast and pile-up-proof. This
worker does the real ingest. It's a singleton (guarded by a lock file): a second
invocation exits immediately. A lazily-spawned worker idle-exits after a couple
minutes with an empty spool; pass --managed (used by the OS service) to keep it
running indefinitely.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorker(managed)
		},
	}
	cmd.Flags().BoolVar(&managed, "managed", false, "Run indefinitely (for the OS-supervised service); don't idle-exit")
	return cmd
}

func runWorker(managed bool) error {
	if err := os.MkdirAll(paths.PosthookDir(), 0o755); err != nil {
		return err
	}

	// Singleton: hold an exclusive, non-blocking flock for the worker's whole
	// lifetime. If another worker holds it, exit quietly — there's nothing to do.
	lockFile, err := os.OpenFile(workerLockPath(), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		logx.Debugf("worker: another instance is running, exiting")
		return nil
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
	// Record our pid in the lock file so `posthook uninstall` can signal us.
	// The flock is the real guard; the pid is advisory and only read while
	// the lock is held.
	_ = lockFile.Truncate(0)
	_, _ = lockFile.WriteAt([]byte(strconv.Itoa(os.Getpid())), 0)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	logx.Debugf("worker: started (managed=%v)", managed)
	// The worker is the one long-lived process that holds the store open, so
	// it owns age-based retention. A lazily-spawned worker checks once at
	// start; a managed one re-checks hourly. Never fatal: a failed prune just
	// means the database stays a little larger until the next pass.
	pruneIfDue()
	lastPruneCheck := time.Now()

	lastWork := time.Now()
	for {
		if managed && time.Since(lastPruneCheck) >= workerRetentionInterval {
			pruneIfDue()
			lastPruneCheck = time.Now()
		}

		n, err := spool.Drain(ingest.ProcessAgentEnvelope)
		if err != nil {
			// A failed drain (e.g. DB momentarily locked) leaves the record for
			// the next pass. Log and back off rather than spin.
			logx.Warnf("worker: drain error: %v", err)
		}
		if n > 0 {
			lastWork = time.Now()
			logx.Debugf("worker: drained %d event(s)", n)
		}
		// Commits the git shadow saw being pushed that were never recorded
		// (see ingest/catchup.go). After the events, so a commit is
		// attributed with every edit that led up to it already in the store.
		jobs, err := spool.DrainCommitJobs(ingest.ProcessCommitJob)
		if err != nil {
			logx.Warnf("worker: commit catch-up error: %v", err)
		}
		if jobs > 0 {
			lastWork = time.Now()
			logx.Debugf("worker: handled %d commit catch-up job(s)", jobs)
		}

		if !managed && time.Since(lastWork) > workerIdleTimeout {
			// Guard against exiting just as an event lands: if anything is
			// still queued, keep going rather than strand it until the next
			// hook fire.
			p, _ := spool.Pending()
			j, _ := spool.PendingCommitJobs()
			if p+j > 0 {
				lastWork = time.Now()
				continue
			}
			logx.Debugf("worker: idle for %s, exiting", workerIdleTimeout)
			return nil
		}

		select {
		case <-sigCh:
			logx.Debugf("worker: signal received, exiting")
			return nil
		case <-time.After(workerPollInterval):
		}
	}
}

// pruneIfDue runs the daily retention prune when it is due, logging the
// outcome. Errors are logged and swallowed — the worker's job is draining
// the spool, and retention must never take that down.
func pruneIfDue() {
	cfg, err := config.Load()
	if err != nil {
		logx.Debugf("worker: retention skipped: %v", err)
		return
	}
	db, err := store.Open()
	if err != nil {
		logx.Debugf("worker: retention skipped: %v", err)
		return
	}
	// Capture's working state (file snapshots, remembered lines) expires on
	// its own clock, whether or not event retention is switched on.
	if removed, ran, err := db.PruneEditStateIfDue(); err != nil {
		logx.Debugf("worker: edit-state prune failed: %v", err)
	} else if ran && removed > 0 {
		logx.Debugf("worker: dropped %d stale edit-state row(s)", removed)
	}
	days := cfg.RetentionDays()
	if days == 0 {
		return
	}
	res, ran, err := db.PruneIfDue(store.PruneOptions{
		OlderThan:     time.Duration(days) * 24 * time.Hour,
		RequireSynced: cfg.Cloud.Enabled,
	})
	if err != nil {
		logx.Warnf("worker: retention prune failed: %v", err)
		return
	}
	if ran {
		logx.Infof("worker: retention pruned %d event(s) and %d prompt(s) older than %s (%d days)",
			res.Events, res.Prompts, res.Cutoff.Format("2006-01-02"), days)
	}
}

// stopWorker asks a running spool worker to exit and waits for it to let go
// of the lock: SIGTERM, a grace period, then SIGKILL. Returns whether a
// worker was running at all, and an error if one is still holding the lock
// afterwards (or could not be identified).
func stopWorker() (bool, error) {
	if !workerRunning() {
		return false, nil
	}
	pids := workerPIDs()
	if len(pids) == 0 {
		return true, errors.New("a worker holds the lock but its pid could not be found; it exits on its own within 2 minutes")
	}
	if alive := terminateAndWait(pids, stopGrace); len(alive) > 0 {
		return true, fmt.Errorf("worker (pid %v) did not exit", alive)
	}
	if workerRunning() {
		return true, fmt.Errorf("worker lock %s is still held after stopping pid %v", workerLockPath(), pids)
	}
	return true, nil
}

// workerPIDs finds the running worker: the pid it wrote into the lock file,
// or — for a worker started by an older binary that did not record one — a
// pgrep for its command line.
func workerPIDs() []int {
	if data, err := os.ReadFile(workerLockPath()); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
			return []int{pid}
		}
	}
	return pgrepPIDs(ownProcessPattern("worker"))
}

// workerRunning reports whether a worker currently holds the singleton lock.
// It probes with a non-blocking flock: if it can acquire (then immediately
// release) the lock, no worker is running. Read-only — never spawns.
func workerRunning() bool {
	f, err := os.OpenFile(workerLockPath(), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return true // someone else holds it
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

// ensureWorker spawns a background worker if one isn't already running. It's
// called from the hot ingest path, so it must be near-free in the common case
// (worker already up): a single non-blocking flock probe. Only when the probe
// shows no live worker does it spawn a detached process. All failures are
// swallowed — a missing worker just means events drain a little later.
func ensureWorker() {
	// Probe the lock without holding it: if we can acquire it, no worker is
	// running. Release immediately and spawn one (the spawned process re-takes
	// the lock for its lifetime). If we can't, a worker is already up.
	f, err := os.OpenFile(workerLockPath(), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return // worker already running
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	spawnWorker()
}

// spawnWorker starts `posthook worker` fully detached: its own session, no
// controlling terminal, output to the worker log, and not waited on. If two
// hooks race here both spawned workers contend for the lock and one exits, so
// the singleton invariant holds.
func spawnWorker() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	logFile, err := os.OpenFile(workerLogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		logFile = nil
	}

	cmd := exec.Command(exe, "worker")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = nil
	if logFile != nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}
	if err := cmd.Start(); err != nil {
		if logFile != nil {
			_ = logFile.Close()
		}
		return
	}
	// Release our handle; the child keeps its own. Don't Wait — it's detached.
	if logFile != nil {
		_ = logFile.Close()
	}
	_ = cmd.Process.Release()
	logx.Debugf("worker: spawned background drainer (pid %d)", cmd.Process.Pid)
}
