package commands

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bilanc/posthook/internal/config"
	"github.com/bilanc/posthook/internal/store"

	"github.com/spf13/cobra"
)

func newPruneCmd() *cobra.Command {
	var (
		olderThan        string
		dryRun           bool
		vacuum           bool
		includeUnsynced  bool
		setRetentionDays int
	)
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Delete old events and prompts from the local store",
		Long: `Deletes agent events and typed prompts older than the retention window from
~/.posthook/posthook.db. Sessions, commits, attribution, and the edit events
that back attribution are always kept, whatever their age.

The background worker prunes automatically (at most once a day) using
storage.retention_days from ~/.posthook/config.json — default 365, 0 = keep
forever. When cloud sync is on, only rows already flushed upstream are pruned.

  posthook prune                         prune using the configured retention
  posthook prune --older-than 90d        prune rows older than 90 days (Nd/Nw/Nm/Ny)
  posthook prune --dry-run               count what a prune would delete
  posthook prune --vacuum                also shrink the database file afterwards
  posthook prune --include-unsynced      prune rows cloud sync hasn't flushed yet
  posthook prune --set-retention-days N  write storage.retention_days (0 = forever)

Deleting rows frees pages inside the file but does not shrink it; --vacuum
rebuilds the file so the space goes back to the filesystem.
`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("set-retention-days") {
				return runPruneSetRetention(setRetentionDays)
			}
			return runPrune(olderThan, dryRun, vacuum, includeUnsynced)
		},
	}
	cmd.Flags().StringVar(&olderThan, "older-than", "", "Prune rows older than this age: Nd, Nw, Nm (30-day months) or Ny (default: configured retention)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Report what would be deleted without changing anything")
	cmd.Flags().BoolVar(&vacuum, "vacuum", false, "Run VACUUM after pruning to shrink the database file")
	cmd.Flags().BoolVar(&includeUnsynced, "include-unsynced", false, "With cloud sync on, also prune rows that haven't been flushed upstream")
	cmd.Flags().IntVar(&setRetentionDays, "set-retention-days", 0, "Write storage.retention_days to ~/.posthook/config.json and exit (0 = keep forever)")
	return cmd
}

// satisfiesRetentionGate reports whether a manual prune of the given age
// counts as the worker's daily pass. Only a pass at least as strict as the
// configured retention does: recording a looser --older-than would postpone
// the configured prune by a day.
func satisfiesRetentionGate(age time.Duration, retentionDays int) bool {
	if retentionDays <= 0 {
		return false
	}
	return age <= time.Duration(retentionDays)*24*time.Hour
}

func runPrune(olderThan string, dryRun, vacuum, includeUnsynced bool) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	var age time.Duration
	var ageLabel string
	if olderThan != "" {
		if age, err = parseRetention(olderThan); err != nil {
			return err
		}
		ageLabel = olderThan
	} else {
		days := cfg.RetentionDays()
		if days == 0 {
			fmt.Println("retention is off (storage.retention_days = 0): nothing is pruned automatically.")
			fmt.Println("  prune once:   posthook prune --older-than 365d")
			fmt.Println("  turn it on:   posthook prune --set-retention-days 365")
			return nil
		}
		age = time.Duration(days) * 24 * time.Hour
		ageLabel = fmt.Sprintf("%d days", days)
	}
	requireSynced := cfg.Cloud.Enabled && !includeUnsynced

	db, err := store.Open()
	if err != nil {
		return err
	}
	before, _, err := db.FileSize()
	if err != nil {
		return err
	}

	res, err := db.Prune(store.PruneOptions{OlderThan: age, RequireSynced: requireSynced, DryRun: dryRun})
	if err != nil {
		return err
	}
	if !dryRun && satisfiesRetentionGate(age, cfg.RetentionDays()) {
		if err := db.RecordPrune(time.Now()); err != nil {
			return err
		}
	}

	verb := "deleted"
	if dryRun {
		verb = "would delete"
	}
	fmt.Printf("posthook prune — rows older than %s (%s)\n", res.Cutoff.Local().Format("2006-01-02"), ageLabel)
	if dryRun {
		fmt.Println("  dry run:      no changes made")
	}
	fmt.Printf("  events:       %s %d (edit events behind attribution are kept)\n", verb, res.Events)
	fmt.Printf("  prompts:      %s %d\n", verb, res.Prompts)
	if requireSynced {
		fmt.Println("  cloud sync:   on — only rows already flushed upstream are pruned (--include-unsynced overrides)")
	}

	if vacuum && !dryRun {
		if err := db.Vacuum(); err != nil {
			return fmt.Errorf("vacuum: %w", err)
		}
		shrunk, _, err := db.FileSize()
		if err != nil {
			return err
		}
		fmt.Printf("  db size:      %s → %s (vacuumed)\n", fmtBytes(before), fmtBytes(shrunk))
		return nil
	}

	after, reclaimable, err := db.FileSize()
	if err != nil {
		return err
	}
	fmt.Printf("  db size:      %s → %s (%s reclaimable)\n", fmtBytes(before), fmtBytes(after), fmtBytes(reclaimable))
	if after > 0 && reclaimable*10 > after {
		fmt.Printf("  hint:         run `posthook prune --vacuum` to shrink the file by %s\n", fmtBytes(reclaimable))
	}
	return nil
}

// runPruneSetRetention persists storage.retention_days. Like `sync --set-*`
// it edits the on-disk file rather than the env-merged view, so an env
// override never gets baked into config.json.
func runPruneSetRetention(days int) error {
	disk, err := config.ReadDisk()
	if err != nil {
		return err
	}
	if days < 0 {
		days = 0
	}
	disk.Storage.RetentionDays = &days
	if err := config.Save(disk); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n", config.Path())
	if days == 0 {
		fmt.Println("  retention: off — events and prompts are kept forever")
	} else {
		fmt.Printf("  retention: %d days — the background worker prunes older events and prompts daily\n", days)
	}
	return nil
}

// retentionMaxDays keeps N×24h inside time.Duration's ~292-year range.
const retentionMaxDays = 100000

// parseRetention turns "90d", "12w", "6m" (30-day months) or "2y" (365-day
// years) into a duration. A unit is required so a bare number can't be
// misread as days when the user meant something else.
func parseRetention(s string) (time.Duration, error) {
	in := strings.TrimSpace(strings.ToLower(s))
	bad := func() (time.Duration, error) {
		return 0, fmt.Errorf("invalid retention %q: use a positive number followed by d, w, m or y (e.g. 90d, 12w, 6m, 1y)", s)
	}
	if len(in) < 2 {
		return bad()
	}
	n, err := strconv.Atoi(in[:len(in)-1])
	if err != nil || n <= 0 || strings.HasPrefix(in, "+") {
		return bad()
	}
	var days int
	switch in[len(in)-1] {
	case 'd':
		days = n
	case 'w':
		days = n * 7
	case 'm':
		days = n * 30
	case 'y':
		days = n * 365
	default:
		return bad()
	}
	if days > retentionMaxDays {
		return 0, fmt.Errorf("invalid retention %q: at most %d days", s, retentionMaxDays)
	}
	return time.Duration(days) * 24 * time.Hour, nil
}

// fmtBytes renders a byte count with one decimal in MB, or GB from 1 GB up.
func fmtBytes(n int64) string {
	const (
		mb = 1024 * 1024
		gb = 1024 * mb
	)
	if n >= gb {
		return fmt.Sprintf("%.1f GB", float64(n)/gb)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/mb)
}
