package commands

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"testing"
	"time"
)

// startChild runs a shell command detached enough for the test: a goroutine
// reaps it so a killed child never lingers as a zombie, which
// processAlive would otherwise still count as alive.
func startChild(t *testing.T, script string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	go func() { _ = cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd
}

func TestTerminateAndWaitStopsACooperativeProcess(t *testing.T) {
	cmd := startChild(t, "exec sleep 30")
	pid := cmd.Process.Pid
	if !processAlive(pid) {
		t.Fatal("child should be alive before the signal")
	}
	start := time.Now()
	if alive := terminateAndWait([]int{pid}, 2*time.Second); len(alive) != 0 {
		t.Fatalf("still alive: %v", alive)
	}
	if time.Since(start) > 1500*time.Millisecond {
		t.Fatal("a cooperative process should exit well before the grace period ends")
	}
	if processAlive(pid) {
		t.Fatal("child still alive after terminateAndWait")
	}
}

func TestTerminateAndWaitKillsAProcessThatIgnoresTerm(t *testing.T) {
	cmd := startChild(t, `trap "" TERM; while :; do sleep 1; done`)
	pid := cmd.Process.Pid
	time.Sleep(100 * time.Millisecond) // let the trap install
	if alive := terminateAndWait([]int{pid}, 300*time.Millisecond); len(alive) != 0 {
		t.Fatalf("still alive after SIGKILL: %v", alive)
	}
	if processAlive(pid) {
		t.Fatal("child survived SIGKILL")
	}
}

func TestProcessAliveIsFalseForAnExitedPid(t *testing.T) {
	cmd := startChild(t, "exit 0")
	deadline := time.Now().Add(2 * time.Second)
	for processAlive(cmd.Process.Pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if processAlive(cmd.Process.Pid) {
		t.Fatal("an exited, reaped child should not be alive")
	}
}

func TestPgrepPIDsFindsThisUsersMatchingProcess(t *testing.T) {
	marker := fmt.Sprintf("posthook-procs-test-%d", time.Now().UnixNano())
	// The marker sits in sh's own argv, so sh (not sleep) is the match.
	cmd := startChild(t, "sleep 30; echo "+marker)
	pid := cmd.Process.Pid
	deadline := time.Now().Add(2 * time.Second)
	var pids []int
	for len(pids) == 0 && time.Now().Before(deadline) {
		pids = pgrepPIDs(marker)
		time.Sleep(50 * time.Millisecond)
	}
	found := false
	for _, p := range pids {
		if p == pid {
			found = true
		}
	}
	if !found {
		t.Fatalf("pgrepPIDs(%q) = %v, want it to include %d", marker, pids, pid)
	}
	if alive := terminateAndWait(pids, time.Second); len(alive) != 0 {
		t.Fatalf("still alive: %v", alive)
	}
	if got := pgrepPIDs(marker); len(got) != 0 {
		t.Fatalf("pgrepPIDs after stop = %v, want none", got)
	}
}

func TestOwnProcessPatternMatchesOnlyThisBinaryOrBarePosthook(t *testing.T) {
	pat := regexp.MustCompile(ownProcessPattern("sync --loop"))
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{exe + " sync --loop", "posthook sync --loop"} {
		if !pat.MatchString(want) {
			t.Errorf("%q should match %s", want, pat)
		}
	}
	for _, dont := range []string{
		"/somewhere/else/posthook sync --loop", // another install's daemon
		exe + " sync",                          // one-shot flush, not the loop
		exe + " sync --loopy",
		"posthook worker",
		"vim posthook sync --loop",
	} {
		if pat.MatchString(dont) {
			t.Errorf("%q should not match %s", dont, pat)
		}
	}

	// Trailing flags belong to the same process: a managed worker is still
	// a worker.
	worker := regexp.MustCompile(ownProcessPattern("worker"))
	for _, want := range []string{exe + " worker", exe + " worker --managed", "posthook worker --managed"} {
		if !worker.MatchString(want) {
			t.Errorf("%q should match %s", want, worker)
		}
	}
	if worker.MatchString(exe + " workers") {
		t.Errorf("%q should not match %s", exe+" workers", worker)
	}
}

func TestParseElapsed(t *testing.T) {
	cases := map[string]time.Duration{
		"00:01":       time.Second,
		"45:07":       45*time.Minute + 7*time.Second,
		"01:02:03":    time.Hour + 2*time.Minute + 3*time.Second,
		"44-10:01:05": 44*24*time.Hour + 10*time.Hour + time.Minute + 5*time.Second,
	}
	for in, want := range cases {
		got, ok := parseElapsed(in)
		if !ok || got != want {
			t.Errorf("parseElapsed(%q) = %v, %v; want %v, true", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "7", "a:b", "1:2:3:4", "x-01:02"} {
		if _, ok := parseElapsed(bad); ok {
			t.Errorf("parseElapsed(%q) should fail", bad)
		}
	}
}

func TestProcessStartedBeforeTellsAFreshPidFromAnOldOne(t *testing.T) {
	cmd := startChild(t, "sleep 30; :")
	pid := cmd.Process.Pid
	time.Sleep(200 * time.Millisecond)
	if !processStartedBefore(pid, time.Now()) {
		t.Fatal("a process started a moment ago should count as started before now")
	}
	// A pid file written a minute before this process existed must not vouch
	// for it: that is what pid reuse looks like.
	if processStartedBefore(pid, time.Now().Add(-time.Minute)) {
		t.Fatal("a process should not count as started before a time that predates it")
	}
	_ = cmd.Process.Kill()
	deadline := time.Now().Add(2 * time.Second)
	for processAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if processStartedBefore(pid, time.Now()) {
		t.Fatal("an exited process should not count as started at all")
	}
}
