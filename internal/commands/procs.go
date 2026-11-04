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

// pidFileTolerance absorbs the one-second resolution of ps's elapsed time
// plus the moment between a process starting and its pid being written down.
const pidFileTolerance = 5 * time.Second

// processStartedBefore reports whether pid was already running at t. A pid
// read from a file is only as good as the process that wrote it: once that
// process is gone the OS may hand the pid to something else, and that
// something necessarily started after the file was written. Start time is
// derived from ps's elapsed time, which both macOS and Linux report.
func processStartedBefore(pid int, t time.Time) bool {
	elapsed, ok := processElapsed(pid)
	if !ok {
		return false
	}
	started := time.Now().Add(-elapsed)
	return !started.After(t.Add(pidFileTolerance))
}

// processElapsed is how long pid has been running according to ps, or
// ok=false when the process is gone.
func processElapsed(pid int) (time.Duration, bool) {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "etime=").Output()
	if err != nil {
		return 0, false
	}
	return parseElapsed(strings.TrimSpace(string(out)))
}

// parseElapsed parses ps's etime column: [[dd-]hh:]mm:ss.
func parseElapsed(s string) (time.Duration, bool) {
	var days int64
	if i := strings.IndexByte(s, '-'); i >= 0 {
		d, err := strconv.ParseInt(s[:i], 10, 64)
		if err != nil {
			return 0, false
		}
		days, s = d, s[i+1:]
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	var secs int64
	for _, part := range parts {
		n, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return 0, false
		}
		secs = secs*60 + n
	}
	return time.Duration(days*86400+secs) * time.Second, true
}

// syncLoopPIDs finds this install's resident sync daemon (`posthook sync
// --loop`), whether launched by launchd / systemd or by hand.
func syncLoopPIDs() []int { return pgrepPIDs(ownProcessPattern("sync --loop")) }
