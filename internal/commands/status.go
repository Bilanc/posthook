package commands

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/bilanc/posthook/internal/config"
	"github.com/bilanc/posthook/internal/spool"
	"github.com/bilanc/posthook/internal/store"

	"github.com/spf13/cobra"
)

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show counts, shadow health, and recent activity",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStatus()
		},
	}
}

func runStatus() error {
	db, err := store.Open()
	if err != nil {
		return err
	}

	var totals struct {
		events, sessions, commits, repos int
	}
	err = db.QueryRow(`
		SELECT
			(SELECT COUNT(*) FROM events WHERE event_type != 'hook_misfire'),
			(SELECT COUNT(*) FROM sessions),
			(SELECT COUNT(*) FROM commits),
			(SELECT COUNT(*) FROM repositories)`).
		Scan(&totals.events, &totals.sessions, &totals.commits, &totals.repos)
	if err != nil {
		return err
	}

	fmt.Println("posthook — local store at ~/.posthook/posthook.db")
	fmt.Printf("  events:       %d\n", totals.events)
	fmt.Printf("  sessions:     %d\n", totals.sessions)
	fmt.Printf("  commits:      %d\n", totals.commits)
	fmt.Printf("  repositories: %d\n", totals.repos)
	printIdentityLine()
	fmt.Println()

	printIngestQueue()

	printShadowHealth(CheckShadowHealth())

	rows, err := db.Query(`
		SELECT agent_slug, COUNT(*) AS n
		FROM events
		WHERE event_type != 'hook_misfire'
		GROUP BY agent_slug
		ORDER BY n DESC`)
	if err != nil {
		return err
	}
	type agentCount struct {
		agent string
		n     int
	}
	var byAgent []agentCount
	for rows.Next() {
		var r agentCount
		if err := rows.Scan(&r.agent, &r.n); err != nil {
			rows.Close()
			return err
		}
		byAgent = append(byAgent, r)
	}
	rows.Close()
	if len(byAgent) > 0 {
		fmt.Println("Events by agent:")
		for _, r := range byAgent {
			fmt.Printf("  %-16s %d\n", r.agent, r.n)
		}
		fmt.Println()
	}

	var misfireTotal int
	var misfireLast24 sql.NullInt64
	err = db.QueryRow(`
		SELECT
			COUNT(*) AS total,
			SUM(CASE WHEN ts >= datetime('now', '-1 day') THEN 1 ELSE 0 END) AS last_24h
		FROM events
		WHERE event_type = 'hook_misfire'`).Scan(&misfireTotal, &misfireLast24)
	if err != nil {
		return err
	}
	if misfireTotal > 0 {
		fmt.Println("Hook health:")
		fmt.Printf("  misfires (total)    %d\n", misfireTotal)
		fmt.Printf("  misfires (last 24h) %d\n", misfireLast24.Int64)
		mrows, err := db.Query(`
			SELECT agent_slug, COUNT(*) AS n
			FROM events
			WHERE event_type = 'hook_misfire'
			GROUP BY agent_slug
			ORDER BY n DESC`)
		if err != nil {
			return err
		}
		for mrows.Next() {
			var a string
			var n int
			if err := mrows.Scan(&a, &n); err != nil {
				mrows.Close()
				return err
			}
			fmt.Printf("    %-14s %d\n", a, n)
		}
		mrows.Close()
		fmt.Println("  A misfire means a hook fired but stdin was empty. Run `posthook inspect")
		fmt.Println("  --type hook_misfire` for context. Check the agent's hook config if frequent.")
		fmt.Println()
	}

	commitRows, err := db.Query(`
		SELECT c.sha, c.lines_added, c.lines_removed, c.files_changed, c.committed_at, r.name AS repo
		FROM commits c
		JOIN repositories r ON r.id = c.repo_id
		ORDER BY c.committed_at DESC
		LIMIT 10`)
	if err != nil {
		return err
	}
	defer commitRows.Close()
	type commitRow struct {
		sha          string
		added        int
		removed      int
		filesChanged int
		committedAt  string
		repo         string
	}
	var recent []commitRow
	for commitRows.Next() {
		var r commitRow
		if err := commitRows.Scan(&r.sha, &r.added, &r.removed, &r.filesChanged, &r.committedAt, &r.repo); err != nil {
			return err
		}
		recent = append(recent, r)
	}
	if err := commitRows.Err(); err != nil {
		return err
	}
	if len(recent) > 0 {
		fmt.Println("Recent commits:")
		for _, c := range recent {
			stamp := c.committedAt
			if len(stamp) >= 16 {
				stamp = strings.Replace(stamp[:16], "T", " ", 1)
			}
			short := c.sha
			if len(short) >= 7 {
				short = short[:7]
			}
			fmt.Printf("  %s  %s  %-20s +%d/-%d (%d files)\n",
				stamp, short, c.repo, c.added, c.removed, c.filesChanged)
		}
	}
	return nil
}

// printIdentityLine shows who sessions are attributed to. When cloud sync is
// on and no identity is configured, attribution upstream depends on per-repo
// git config being right — worth a loud nudge.
func printIdentityLine() {
	cfg, err := config.Load()
	if err != nil {
		return
	}
	if cfg.Engineer.Email != "" {
		who := cfg.Engineer.Email
		if cfg.Engineer.Name != "" {
			who = cfg.Engineer.Name + " <" + cfg.Engineer.Email + ">"
		}
		fmt.Printf("  identity:     %s\n", who)
		return
	}
	fmt.Printf("  identity:     — not set\n")
	if cfg.Cloud.Enabled {
		fmt.Println()
		fmt.Println("  ⚠ Cloud sync is on but no engineer identity is set — sessions may sync")
		fmt.Println("    unattributed. Fix: posthook identity --setup")
	}
}

// printIngestQueue surfaces the async ingest path: agent hooks drop events in
// the spool and a background worker drains them into the store. A persistently
// non-empty spool with no worker means events aren't landing — worth flagging.
func printIngestQueue() {
	pending, err := spool.Pending()
	if err != nil {
		return
	}
	running := workerRunning()
	if pending == 0 && running {
		return // healthy and quiet
	}
	fmt.Println("Ingest queue:")
	fmt.Printf("  spooled events: %d\n", pending)
	if running {
		fmt.Println("  worker:         running")
	} else {
		fmt.Println("  worker:         not running")
		if pending > 0 {
			fmt.Println("  ⚠ Events are queued but no worker is draining them. A worker auto-starts")
			fmt.Println("    on the next agent tool call, or start one now: posthook worker")
		}
	}
	fmt.Println()
}

func printShadowHealth(h ShadowHealth) {
	if !h.Attempted {
		return
	}
	if h.Winning && h.SymlinkValid {
		fmt.Println("Git shadow:")
		fmt.Printf("  active   %s\n", h.ShadowPath)
		realGit := h.SavedRealGit
		if realGit == "" {
			realGit = "(unsaved)"
		}
		fmt.Printf("  real git %s\n", realGit)
		fmt.Println()
		return
	}

	fmt.Println("Git shadow: ⚠ NOT INTERCEPTING")
	switch {
	case !h.SymlinkExists:
		path := h.ShadowPath
		if path == "" {
			path = "(unknown)"
		}
		fmt.Printf("  Symlink missing at %s.\n", path)
		fmt.Println("  Fix: run `posthook install-shadow`.")
	case !h.SymlinkValid:
		fmt.Printf("  Symlink at %s does not point at the posthook binary.\n", h.ShadowPath)
		fmt.Println("  Fix: run `posthook uninstall-shadow` then `posthook install-shadow`.")
	case !h.Winning:
		fmt.Printf("  Symlink is in place at %s\n", h.ShadowPath)
		which := h.WhichGit
		if which == "" {
			which = "(nothing)"
		}
		fmt.Printf("  but `which git` returns %s — PATH order means our shadow is bypassed.\n", which)
		fmt.Println("  Fix: add to your shell rc and open a new shell:")
		if h.ShadowPath != "" {
			dir := strings.TrimSuffix(h.ShadowPath, "/git")
			fmt.Printf("    export PATH=\"%s:$PATH\"\n", dir)
		}
		fmt.Println("  Then verify with: which git")
	}
	fmt.Println("  While unfixed, git commits captured only via per-repo hooks (templateDir + posthook track).")
	fmt.Println()
}
