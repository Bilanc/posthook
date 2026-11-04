package commands

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// stopGrace is how long a posthook background process (worker, dashboard
// server, sync loop) gets to exit after SIGTERM before it is killed.
const stopGrace = 5 * time.Second

// processAlive reports whether pid still exists. A pid we are not allowed
// to signal belongs to someone else's live process, so it counts as alive.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// signalAndWait sends sig to every pid and waits up to wait for them to go
// away. Returns the pids still alive at the deadline.
func signalAndWait(pids []int, sig syscall.Signal, wait time.Duration) []int {
	for _, pid := range pids {
		_ = syscall.Kill(pid, sig)
	}
	deadline := time.Now().Add(wait)
	for {
		var alive []int
		for _, pid := range pids {
			if processAlive(pid) {
				alive = append(alive, pid)
			}
		}
		if len(alive) == 0 || time.Now().After(deadline) {
			return alive
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// terminateAndWait stops posthook's own background processes: SIGTERM, a
// grace period for a clean exit, then SIGKILL for anything still running.
// Returns the pids that survived even that (normally none). The caller must
// not be the pids' parent, or they linger as zombies until reaped.
func terminateAndWait(pids []int, grace time.Duration) []int {
	alive := signalAndWait(pids, syscall.SIGTERM, grace)
	if len(alive) > 0 {
		alive = signalAndWait(alive, syscall.SIGKILL, time.Second)
	}
	return alive
}

// pgrepPIDs returns the pids of this user's processes whose full command
// line matches pattern, excluding the current process. Empty when pgrep is
// unavailable or nothing matches.
func pgrepPIDs(pattern string) []int {
	out, err := exec.Command("pgrep", "-u", strconv.Itoa(os.Getuid()), "-f", pattern).Output()
	if err != nil {
		return nil
	}
	var pids []int
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		pid, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil || pid <= 0 || pid == os.Getpid() {
			continue
		}
		pids = append(pids, pid)
	}
	return pids
}

// ownProcessPattern builds a pgrep -f pattern for one of posthook's own
// background processes running subcommand: this executable by path (how
// launchd / systemd and spawnWorker start it) or a bare `posthook` from PATH
// (how a person starts one by hand), followed by the subcommand and any
// further flags (`worker --managed`). Processes of another install — a dev
// build running from a scratch HOME, say — are not ours to stop.
func ownProcessPattern(subcommand string) string {
	alts := []string{"posthook"}
	if exe, err := os.Executable(); err == nil {
		alts = append(alts, regexp.QuoteMeta(exe))
		if real, err := filepath.EvalSymlinks(exe); err == nil && real != exe {
			alts = append(alts, regexp.QuoteMeta(real))
		}
	}
	return "^(" + strings.Join(alts, "|") + ") " + regexp.QuoteMeta(subcommand) + "( .*)?$"
}

// processCommand returns pid's command line as ps reports it, or "" when
// the process is gone. Used to check that a pid read from a file still
// belongs to the process that wrote it, since the OS reuses pids.
func processCommand(pid int) string {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// syncLoopPIDs finds this install's resident sync daemon (`posthook sync
// --loop`), whether launched by launchd / systemd or by hand.
func syncLoopPIDs() []int { return pgrepPIDs(ownProcessPattern("sync --loop")) }
