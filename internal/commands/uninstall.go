package commands

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/bilanc/posthook/internal/config"
	"github.com/bilanc/posthook/internal/ingest"
	"github.com/bilanc/posthook/internal/installers"
	"github.com/bilanc/posthook/internal/logx"
	"github.com/bilanc/posthook/internal/notes"
	"github.com/bilanc/posthook/internal/paths"
	"github.com/bilanc/posthook/internal/service"
	"github.com/bilanc/posthook/internal/spool"
	"github.com/bilanc/posthook/internal/store"

	"github.com/spf13/cobra"
)

func newUninstallCmd() *cobra.Command {
	var yes, dryRun, keepData bool
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove posthook from this machine completely",
		Long: `Remove every trace of posthook from this machine, so a later install starts
from scratch. In order:

  1. stop the background sync daemon, the dashboard server and the spool worker
  2. remove posthook's hook entries from Claude Code, Cursor and Codex CLI
     (your own hooks in those files are untouched)
  3. drain any queued events and, if cloud sync is configured, flush once more
     so nothing already captured is lost
  4. in every repo posthook has recorded: remove its post-commit hook, its
     notes refspec, and the local refs/notes/posthook* refs (notes already
     pushed to a remote are never touched)
  5. remove the git shadow symlink and unset init.templateDir
  6. delete ~/.posthook (database, config, dashboard bundle, logs)
  7. delete the posthook binary

--dry-run prints the full list for this machine without changing anything.
--keep-data leaves ~/.posthook/posthook.db and config.json in place (everything
else still goes); a later install picks them up again. POSTHOOK_DEBUG=1 lists
each repo the sweep changed.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUninstall(yes, dryRun, keepData)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "Don't ask for confirmation")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show what would be removed and exit")
	cmd.Flags().BoolVar(&keepData, "keep-data", false, "Keep ~/.posthook/posthook.db and config.json (identity + cloud settings)")
	return cmd
}

func runUninstall(yes, dryRun, keepData bool) error {
	p := gatherUninstallPlan(keepData)
	p.print()
	if dryRun {
		fmt.Println("Dry run — nothing was changed.")
		return nil
	}
	if !yes {
		ok, err := confirmOnTTY("Remove everything listed above? [y/N] ")
		if err != nil {
			return err
		}
		if !ok {
			fmt.Println("Aborted — nothing was changed.")
			return nil
		}
		fmt.Println()
	}
	return p.execute()
}

// uninstallPlan is everything posthook has left on this machine, gathered up
// front so one list drives --dry-run, the confirmation prompt and the removal
// itself. Gathering is read-only.
type uninstallPlan struct {
	keepData bool

	serviceInstalled bool
	serviceStatus    string
	dashPID          int // 0 when no dashboard server is tracked
	workerRunning    bool
	spoolPending     int
	cloudConfigured  bool

	agents []agentTrace

	shadow      ShadowHealth
	templateDir bool
	repos       []string

	dataDir       string
	dataDirExists bool
	dataSize      int64
	dbExists      bool

	binary string // "" when there is nothing safe to delete
}

// agentTrace is one agent config file that still carries a posthook hook.
type agentTrace struct {
	name      string
	path      string
	uninstall func() (installers.Result, error)
}

// keepDataFiles are the ~/.posthook entries --keep-data preserves: the
// database with its WAL sidecars, and the config (cloud sync + identity).
var keepDataFiles = map[string]bool{
	"posthook.db":     true,
	"posthook.db-wal": true,
	"posthook.db-shm": true,
	"config.json":     true,
}

func gatherUninstallPlan(keepData bool) uninstallPlan {
	p := uninstallPlan{keepData: keepData, dataDir: paths.PosthookDir()}

	p.serviceInstalled = service.Installed()
	if p.serviceInstalled {
		p.serviceStatus = service.Status()
	}
	p.dashPID = trackedDashPID()
	// workerRunning probes with O_CREATE, so only ask when the lock file is
	// already there — a dry run must not leave anything behind.
	if _, err := os.Stat(workerLockPath()); err == nil {
		p.workerRunning = workerRunning()
	}
	p.spoolPending, _ = spool.Pending()
	if cfg, err := config.Load(); err == nil {
		p.cloudConfigured = cfg.Cloud.Enabled && cfg.Cloud.Endpoint != "" && cfg.Cloud.Token != ""
	}

	for _, a := range []agentTrace{
		{"Claude Code", paths.ClaudeSettingsPath(), installers.UninstallClaudeCodeHooks},
		{"Cursor", paths.CursorHooksPath(), installers.UninstallCursorHooks},
		{"Codex CLI", paths.CodexConfigPath(), installers.UninstallCodexHooks},
	} {
		if fileContains(a.path, paths.PosthookMarker) {
			p.agents = append(p.agents, a)
		}
	}

	p.shadow = CheckShadowHealth()
	p.templateDir = installers.GlobalGitTemplateActive()

	if _, err := os.Stat(paths.DBPath()); err == nil {
		p.dbExists = true
		p.repos = knownRepos()
	}
	if st, err := os.Stat(p.dataDir); err == nil && st.IsDir() {
		p.dataDirExists = true
		p.dataSize = dirSize(p.dataDir)
	}
	p.binary = deletableBinary()
	return p
}

// drainFirst reports whether queued events are worth draining into the
// database before it goes: only when the data survives (--keep-data) or is
// about to be flushed to the cloud. Otherwise they vanish with ~/.posthook.
func (p uninstallPlan) drainFirst() bool {
	return p.spoolPending > 0 && p.dbExists && (p.keepData || p.cloudConfigured)
}

func (p uninstallPlan) print() {
	line := func(label, text string) { fmt.Printf("  %-18s %s\n", label, text) }

	fmt.Println("posthook uninstall will remove from this machine:")
	fmt.Println()
	fmt.Println("Background processes")
	if p.serviceInstalled {
		line("sync daemon", p.serviceStatus+" — stop and remove")
	} else {
		line("sync daemon", "not installed")
	}
	if p.dashPID > 0 {
		line("dashboard", fmt.Sprintf("running (pid %d) — stop", p.dashPID))
	} else {
		line("dashboard", "not running")
	}
	if p.workerRunning {
		line("spool worker", "running — stop")
	} else {
		line("spool worker", "not running")
	}

	fmt.Println()
	fmt.Println("Agent hooks (your own hooks in these files stay)")
	if len(p.agents) == 0 {
		line("", "none found")
	}
	for _, a := range p.agents {
		line(a.name, tildify(a.path))
	}

	fmt.Println()
	fmt.Println("Git")
	if p.shadow.SymlinkExists {
		line("shadow", tildify(p.shadow.ShadowPath)+" (symlink)")
	} else {
		line("shadow", "not installed")
	}
	if p.templateDir {
		line("init.templateDir", "unset (points at "+tildify(paths.GitTemplateDir())+")")
	} else {
		line("init.templateDir", "not set by posthook")
	}
	line("repos", fmt.Sprintf("%d known repo(s): post-commit hook, notes refspec, local refs/notes/posthook*", len(p.repos)))
	line("", "(notes already pushed to a remote are never touched)")

	fmt.Println()
	fmt.Println("Data")
	switch {
	case !p.dataDirExists:
		line(tildify(p.dataDir), "not present")
	case p.keepData:
		line(tildify(p.dataDir), humanSize(p.dataSize)+" — keeping posthook.db and config.json, removing the rest")
	default:
		line(tildify(p.dataDir), humanSize(p.dataSize)+" — database, config, dashboard bundle, logs")
	}
	if p.spoolPending > 0 {
		if p.drainFirst() {
			line("queued events", fmt.Sprintf("%d — drained into the database first", p.spoolPending))
		} else {
			line("queued events", fmt.Sprintf("%d — discarded with the database", p.spoolPending))
		}
	}
	if p.cloudConfigured && p.dbExists {
		line("cloud sync", "configured — one final flush before the database goes")
	}

	fmt.Println()
	fmt.Println("Binary")
	if p.binary != "" {
		line("", tildify(p.binary))
	} else {
		line("", "not deleting (running from a build directory)")
	}
	fmt.Println()
}

func (p uninstallPlan) execute() error {
	var problems []string
	problem := func(what string, err error) {
		logx.Warnf("%s: %v", what, err)
		problems = append(problems, what)
	}
	step := func(format string, args ...any) { fmt.Printf("  "+format+"\n", args...) }

	fmt.Println("Stopping background processes")
	if p.serviceInstalled {
		if err := service.Uninstall(); err != nil {
			problem("sync daemon", err)
		} else {
			step("sync daemon stopped and removed")
		}
	}
	if p.dashPID > 0 {
		if err := stopDash(); err != nil {
			problem("dashboard", err)
		}
	}
	if p.workerRunning {
		if _, err := stopWorker(); err != nil {
			problem("spool worker", err)
		} else {
			step("spool worker stopped")
		}
	}
	if !p.serviceInstalled && p.dashPID == 0 && !p.workerRunning {
		step("nothing running")
	}

	fmt.Println("Agent hooks")
	if len(p.agents) == 0 {
		step("none found")
	}
	for _, a := range p.agents {
		res, err := a.uninstall()
		if err != nil {
			problem(a.name, err)
			continue
		}
		step("%s", res.Message)
	}

	if p.drainFirst() || (p.cloudConfigured && p.dbExists) {
		fmt.Println("Captured data")
		if p.drainFirst() {
			n, err := spool.Drain(ingest.ProcessAgentEnvelope)
			if err != nil {
				problem("spool drain", err)
			}
			step("drained %d queued event(s) into the database", n)
		}
		if p.cloudConfigured && p.dbExists {
			if err := flushOnce(); err != nil {
				problem("final cloud flush", err)
			}
		}
	}

	fmt.Println("Git")
	if len(p.repos) > 0 {
		hooks, refspecs, refs := sweepRepos(p.repos)
		step("swept %d repo(s): removed %d post-commit hook(s), %d notes refspec(s), %d local notes ref(s)",
			len(p.repos), hooks, refspecs, refs)
	}
	if p.shadow.SymlinkExists {
		removed, shadowPath, err := removeShadow(resolvePosthookBinaryPath())
		switch {
		case err != nil:
			problem("git shadow", err)
		case removed:
			step("removed git shadow %s", tildify(shadowPath))
		}
	}
	if p.templateDir {
		res, err := installers.UninstallGlobalGitTemplate()
		if err != nil {
			problem("git template", err)
		} else {
			step("%s", res.Message)
		}
	}
	if len(p.repos) == 0 && !p.shadow.SymlinkExists && !p.templateDir {
		step("nothing to remove")
	}

	fmt.Println("Data")
	if p.dataDirExists {
		if err := store.Close(); err != nil {
			logx.Debugf("uninstall: close db: %v", err)
		}
		if p.keepData {
			if err := removeAllExcept(p.dataDir, keepDataFiles); err != nil {
				problem("data dir", err)
			} else {
				step("removed everything in %s except posthook.db and config.json", tildify(p.dataDir))
			}
		} else if err := os.RemoveAll(p.dataDir); err != nil {
			problem("data dir", err)
		} else {
			step("removed %s", tildify(p.dataDir))
		}
	} else {
		step("%s not present", tildify(p.dataDir))
	}

	fmt.Println("Binary")
	if p.binary != "" {
		if err := os.Remove(p.binary); err != nil {
			problem("binary", err)
		} else {
			step("removed %s", tildify(p.binary))
		}
	} else {
		step("left in place")
	}

	fmt.Println()
	fmt.Println("Done. Three things to know:")
	fmt.Println("  - Open a new shell (or run `hash -r`): the shell may still have the old git path cached.")
	fmt.Println("  - Restart any Claude Code / Cursor / Codex session that is running: they loaded the hooks")
	fmt.Println("    at startup and will report a failed hook until restarted.")
	fmt.Println("  - If you added ~/.local/bin to PATH in your shell rc, that line was left alone — it is a")
	fmt.Println("    standard directory other tools use too.")
	if p.keepData {
		fmt.Println("  - ~/.posthook/posthook.db and config.json were kept; a later install picks them up.")
	}

	if len(problems) > 0 {
		return fmt.Errorf("uninstall finished with %d problem(s): %s", len(problems), strings.Join(problems, ", "))
	}
	return nil
}

// sweepRepos removes posthook's per-repo traces from every root: the
// post-commit hook, the notes refspec(s) on every remote, and the local
// refs/notes/posthook* refs. Each repo is independent — one failure never
// stops the sweep. Returns how many of each were removed.
func sweepRepos(repos []string) (hooks, refspecs, refs int) {
	for _, root := range repos {
		removedHook, err := installers.RemoveRepoHook(root)
		if err != nil {
			logx.Warnf("%s: post-commit hook: %v", root, err)
		}
		if removedHook {
			hooks++
		}
		removedSpec := notes.RemoveTransport(root)
		if removedSpec {
			refspecs++
		}
		n := notes.DeleteLocalRefs(root)
		refs += n
		if removedHook || removedSpec || n > 0 {
			logx.Debugf("uninstall: %s: hook=%v refspec=%v notes refs=%d", root, removedHook, removedSpec, n)
		}
	}
	return hooks, refspecs, refs
}

// knownRepos returns every repository root the store has recorded that still
// exists on disk, deduplicated. The store knows a repo once posthook has seen
// it (a commit through the shadow or a hook, an agent event, or `posthook
// track`), so a repo created from the git template that never committed
// keeps its hook — it fails silently, since the binary it calls is gone.
func knownRepos() []string {
	db, err := store.Open()
	if err != nil {
		logx.Debugf("uninstall: open store: %v", err)
		return nil
	}
	roots, err := db.RepositoryRoots()
	if err != nil {
		logx.Debugf("uninstall: list repositories: %v", err)
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, root := range roots {
		if seen[root] {
			continue
		}
		seen[root] = true
		if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
			continue
		}
		out = append(out, root)
	}
	return out
}

// trackedDashPID returns the pid in ~/.posthook/dash.pid, or 0 when no
// dashboard server is tracked.
func trackedDashPID() int {
	data, err := os.ReadFile(paths.DashPidFile())
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// deletableBinary resolves the running posthook binary to the file to delete,
// or "" when deleting it would be wrong: a `go run` / `go build` scratch
// binary lives in a temp dir and belongs to the developer's build, not to an
// install.
func deletableBinary() string {
	exe := resolvePosthookBinaryPath()
	if exe == "" {
		return ""
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	if strings.HasPrefix(exe, os.TempDir()) || strings.Contains(exe, "go-build") {
		return ""
	}
	return exe
}

// removeAllExcept deletes every entry in dir except the named ones.
func removeAllExcept(dir string, keep map[string]bool) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if keep[e.Name()] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// confirmOnTTY asks a yes/no question on the controlling terminal, so it
// works even when stdin is a pipe. Without a terminal it errors rather than
// guessing — the caller should be re-run with --yes.
func confirmOnTTY(prompt string) (bool, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return false, errors.New("no terminal to confirm on — re-run with --yes to skip the prompt, or --dry-run to only list")
	}
	defer tty.Close()
	fmt.Fprint(tty, prompt)
	answer := strings.ToLower(readLine(bufio.NewReader(tty)))
	return answer == "y" || answer == "yes", nil
}

func fileContains(path, needle string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), needle)
}

// dirSize sums the regular files under dir. Best-effort: unreadable entries
// count as zero.
func dirSize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}

func humanSize(n int64) string {
	const unit = 1024
	switch {
	case n >= unit*unit*unit:
		return fmt.Sprintf("%.1f GB", float64(n)/(unit*unit*unit))
	case n >= unit*unit:
		return fmt.Sprintf("%.0f MB", float64(n)/(unit*unit))
	case n >= unit:
		return fmt.Sprintf("%.0f KB", float64(n)/unit)
	}
	return fmt.Sprintf("%d B", n)
}

// tildify shortens a path under the home directory to ~/… for display.
func tildify(path string) string {
	home := paths.Home()
	if home != "" && strings.HasPrefix(path, home+string(filepath.Separator)) {
		return "~" + path[len(home):]
	}
	return path
}
