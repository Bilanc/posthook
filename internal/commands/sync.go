package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bilanc/posthook/internal/config"
	"github.com/bilanc/posthook/internal/store"
	pksync "github.com/bilanc/posthook/internal/sync"

	"github.com/spf13/cobra"
)

func newSyncCmd() *cobra.Command {
	var (
		loop        bool
		status      bool
		setEndpoint string
		setToken    string
		setEnabled  string // "true" / "false" / "" (unset = no change)
	)
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Flush local rows to the cloud ingest endpoint",
		Long: `Replicates rows from the local SQLite store to the configured cloud endpoint.

  posthook sync                 flush once and exit (default)
  posthook sync --loop          flush every flush_interval_seconds until killed
  posthook sync --status        show last-flush metadata + pending counts
  posthook sync --set-endpoint URL --set-token TOK --set-enabled true
                                write ~/.posthook/config.json
`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if setEndpoint != "" || setToken != "" || setEnabled != "" {
				return runSyncConfigure(setEndpoint, setToken, setEnabled)
			}
			if status {
				return runSyncStatus()
			}
			if loop {
				return runSyncLoop()
			}
			return runSyncOnce()
		},
	}
	cmd.Flags().BoolVar(&loop, "loop", false, "Flush in a loop on the configured interval (foreground)")
	cmd.Flags().BoolVar(&status, "status", false, "Print sync_state and pending row counts")
	cmd.Flags().StringVar(&setEndpoint, "set-endpoint", "", "Write cloud endpoint URL to ~/.posthook/config.json")
	cmd.Flags().StringVar(&setToken, "set-token", "", "Write cloud install token to ~/.posthook/config.json")
	cmd.Flags().StringVar(&setEnabled, "set-enabled", "", "Set cloud.enabled to true or false")
	return cmd
}

func runSyncOnce() error {
	db, err := store.Open()
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := pksync.Flush(ctx, db, cfg.Cloud)
	if err != nil {
		return err
	}
	if res.Skipped {
		fmt.Printf("sync skipped: %s\n", res.Reason)
		return nil
	}
	total := 0
	for _, n := range res.Synced {
		total += n
	}
	if total == 0 {
		fmt.Printf("sync: nothing pending (%dms)\n", res.DurationMS)
		return nil
	}
	fmt.Printf("sync: flushed %d row(s) in %dms\n", total, res.DurationMS)
	for _, t := range store.SyncableTables {
		if n := res.Synced[t]; n > 0 {
			fmt.Printf("  %-22s %d\n", t, n)
		}
	}
	return nil
}

func runSyncLoop() error {
	db, err := store.Open()
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	interval := time.Duration(cfg.Cloud.FlushIntervalSecs) * time.Second
	fmt.Printf("sync loop: every %s, endpoint=%s (Ctrl-C to stop)\n", interval, cfg.Cloud.Endpoint)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	tick := time.NewTicker(interval)
	defer tick.Stop()

	// Re-load config every tick so flag-flips via `posthook sync --set-*` take
	// effect without restarting the loop.
	flushOnce := func() {
		cfg, err := config.Load()
		if err != nil {
			fmt.Fprintf(os.Stderr, "sync: config load failed: %v\n", err)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		res, err := pksync.Flush(ctx, db, cfg.Cloud)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sync: %v\n", err)
			return
		}
		if res.Skipped {
			return
		}
		total := 0
		for _, n := range res.Synced {
			total += n
		}
		if total > 0 {
			fmt.Printf("[%s] sync: %d row(s) in %dms\n", time.Now().Format("15:04:05"), total, res.DurationMS)
		}
	}

	flushOnce()
	for {
		select {
		case <-sigCh:
			fmt.Println("sync loop: stopping")
			return nil
		case <-tick.C:
			flushOnce()
		}
	}
}

func runSyncStatus() error {
	db, err := store.Open()
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	fmt.Println("posthook sync — configuration:")
	fmt.Printf("  enabled:        %v\n", cfg.Cloud.Enabled)
	fmt.Printf("  endpoint:       %s\n", orDash(cfg.Cloud.Endpoint))
	fmt.Printf("  token:          %s\n", maskToken(cfg.Cloud.Token))
	fmt.Printf("  flush interval: %ds\n", cfg.Cloud.FlushIntervalSecs)
	fmt.Println()

	rows, err := pksync.ReadStatus(db)
	if err != nil {
		return err
	}
	fmt.Printf("%-22s %8s %20s %20s %s\n", "table", "pending", "last_success", "last_attempt", "last_error")
	for _, r := range rows {
		fmt.Printf("%-22s %8d %20s %20s %s\n",
			r.Table,
			r.PendingRows,
			orDash(r.LastSuccess.String),
			orDash(r.LastAttempt.String),
			truncate(r.LastError.String, 60),
		)
	}
	return nil
}

func runSyncConfigure(endpoint, token, enabled string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	// Load() applies env overrides; strip them before save so we persist only
	// what the user explicitly set on this command line plus any prior file
	// contents.
	disk, _ := readDiskConfig()
	if endpoint != "" {
		disk.Cloud.Endpoint = endpoint
	}
	if token != "" {
		disk.Cloud.Token = token
	}
	switch enabled {
	case "true", "1":
		disk.Cloud.Enabled = true
	case "false", "0":
		disk.Cloud.Enabled = false
	}
	if disk.Cloud.FlushIntervalSecs <= 0 {
		disk.Cloud.FlushIntervalSecs = cfg.Cloud.FlushIntervalSecs
	}
	if err := config.Save(disk); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n", config.Path())
	fmt.Printf("  enabled:  %v\n", disk.Cloud.Enabled)
	fmt.Printf("  endpoint: %s\n", orDash(disk.Cloud.Endpoint))
	fmt.Printf("  token:    %s\n", maskToken(disk.Cloud.Token))
	return nil
}

// readDiskConfig reads ~/.posthook/config.json without applying env overrides,
// so `posthook sync --set-*` writes only the on-disk delta and leaves env-only
// settings out of the file.
func readDiskConfig() (config.Config, error) {
	var c config.Config
	b, err := os.ReadFile(config.Path())
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return c, err
	}
	return c, json.Unmarshal(b, &c)
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func maskToken(s string) string {
	if s == "" {
		return "—"
	}
	if len(s) <= 6 {
		return "******"
	}
	return s[:4] + "…" + s[len(s)-2:]
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
