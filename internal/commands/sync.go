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
	"github.com/bilanc/posthook/internal/spool"
	"github.com/bilanc/posthook/internal/store"
	pksync "github.com/bilanc/posthook/internal/sync"

	"github.com/spf13/cobra"
)

func newSyncCmd() *cobra.Command {
	var (
		loop        bool
		status      bool
		preview     bool
		setEndpoint string
		setToken    string
		setEnabled  string // "true" / "false" / "" (unset = no change)
		setSyncMode string // "full" / "metadata_only" / "" (unset = no change)
	)
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Flush local rows to the cloud ingest endpoint",
		Long: `Replicates rows from the local SQLite store to the configured cloud endpoint.

  posthook sync                 drain the spool, flush once and exit (default)
  posthook sync --loop          flush every flush_interval_seconds until killed
  posthook sync --status        show last-flush metadata + pending counts
  posthook sync --preview       print the exact JSON the next flush would send
                                (nothing is sent or marked synced; pipe to jq)
  posthook sync --set-endpoint URL --set-token TOK --set-enabled true
                                write ~/.posthook/config.json
  posthook sync --set-sync-mode metadata_only
                                keep prompts, code and tool output on this
                                machine; sync sessions, tokens, turns, paths
                                and line counts only (default: full)

If the team server reports that the org's policy is metadata-only, the next
flush switches this machine to metadata_only by itself. The server can only
ever tighten a machine, never widen it.
`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if setEndpoint != "" || setToken != "" || setEnabled != "" || setSyncMode != "" {
				return runSyncConfigure(setEndpoint, setToken, setEnabled, setSyncMode)
			}
			if status {
				return runSyncStatus()
			}
			if preview {
				return runSyncPreview()
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
	cmd.Flags().BoolVar(&preview, "preview", false, "Print the next flush's JSON body without sending it")
	cmd.Flags().StringVar(&setSyncMode, "set-sync-mode", "", "Set cloud.sync_mode: full or metadata_only")
	return cmd
}

// spoolDrainWait bounds how long a one-shot sync waits for the worker to
// drain events that hooks spooled moments earlier (e.g. a Stop hook running
// `posthook ingest; posthook sync`). The worker normally catches up within
// one poll interval; the bound only matters if it's wedged.
const spoolDrainWait = 15 * time.Second

// flushDeadline bounds the whole one-shot flush, across all batches.
const flushDeadline = 60 * time.Second

// awaitSpoolDrain makes sure a worker is running and waits until the spool is
// empty, so a one-shot flush includes rows from events that were spooled just
// before it ran. Returns an error if records are still queued when the bound
// elapses, so a zero exit never implies they were flushed.
func awaitSpoolDrain() error {
	ensureWorker()
	deadline := time.Now().Add(spoolDrainWait)
	for {
		p, err := spool.Pending()
		if err != nil {
			return fmt.Errorf("spool: %w", err)
		}
		if p == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("spool: %d event(s) still queued after %s; worker not draining (see %s)", p, spoolDrainWait, workerLogPath())
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func runSyncOnce() error {
	drainErr := awaitSpoolDrain()
	if err := flushOnce(); err != nil {
		return err
	}
	return drainErr
}

// flushOnce ships everything already in the store, independent of the spool.
// Flush sends one batch per table per call, so it repeats until no table
// returns a full batch; a one-shot sync that exits zero has nothing left.
func flushOnce() error {
	db, err := store.Open()
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), flushDeadline)
	defer cancel()
	synced := map[string]int{}
	var durationMS int64
	for {
		res, _, err := flushAndAdopt(ctx, db, &cfg.Cloud)
		if err != nil {
			return err
		}
		if res.Skipped {
			fmt.Printf("sync skipped: %s\n", res.Reason)
			return nil
		}
		for t, n := range res.Synced {
			synced[t] += n
		}
		durationMS += res.DurationMS
		if !res.More {
			break
		}
		if ctx.Err() != nil {
			left, err := pksync.Pending(db)
			if err != nil {
				return err
			}
			if left == 0 {
				break
			}
			return fmt.Errorf("sync: %d row(s) still pending after %s", left, flushDeadline)
		}
	}
	total := 0
	for _, n := range synced {
		total += n
	}
	if total == 0 {
		fmt.Printf("sync: nothing pending (%dms)\n", durationMS)
		return nil
	}
	fmt.Printf("sync: flushed %d row(s) in %dms\n", total, durationMS)
	for _, t := range store.SyncableTables {
		if n := synced[t]; n > 0 {
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

	// A resident loop keeps its start-time code image forever, so after an
	// upgrade it would keep flushing the old sync schema — tables added since
	// (e.g. session_prompts) silently never sync. Watch the binary on disk and
	// exit non-zero when it changes: launchd (KeepAlive) and systemd
	// (Restart=on-failure) both relaunch us on the new version.
	binaryChanged := watchBinary()

	loop := &syncLoopState{db: db}
	loop.tick()
	for {
		select {
		case <-sigCh:
			fmt.Println("sync loop: stopping")
			return nil
		case <-tick.C:
			if binaryChanged() {
				fmt.Println("sync loop: posthook binary was upgraded — exiting so the service supervisor restarts on the new version")
				os.Exit(1)
			}
			loop.tick()
		}
	}
}

// syncLoopState is what the daemon carries from one tick to the next. Config
// is re-read from disk on every tick so `posthook sync --set-*` takes effect
// without a restart — which means a server-reported metadata-only policy has
// to be remembered here as well, for the case where writing it to config.json
// failed: a fresh Load would otherwise widen the very next tick back to full.
//
// The remembered policy is keyed to cloud.sync_mode as it stood on disk when
// the policy was taken (diskMode). It yields when that value changes — which
// is what `posthook sync --set-sync-mode full`, the documented way to widen a
// machine after an org goes back to full, does — and to nothing else, so an
// identity, token or retention edit never lifts it. If the org is in fact
// still metadata-only, the server says so again on the next flush and the
// policy is re-adopted then.
type syncLoopState struct {
	db              *store.DB
	orgMetadataOnly bool
	diskMode        string
}

// tick re-loads config, re-applies a remembered org policy unless
// cloud.sync_mode was changed on disk since, flushes once and adopts whatever
// policy the server reported this time.
func (s *syncLoopState) tick() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "sync: config load failed: %v\n", err)
		return
	}
	if s.orgMetadataOnly {
		s.reassertPolicy(&cfg.Cloud)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, orgMetadataOnly, err := flushAndAdopt(ctx, s.db, &cfg.Cloud)
	if orgMetadataOnly && !s.orgMetadataOnly {
		// Record the on-disk mode after adoptServerPolicy's own write, so
		// that write is not mistaken for a local change.
		s.orgMetadataOnly, s.diskMode = true, diskSyncMode()
	}
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

// reassertPolicy applies the remembered metadata-only policy to a freshly
// loaded config, or drops it when cloud.sync_mode was changed on disk since
// the policy was taken. While the file still resolves to full (the write
// failed when the policy arrived) the write is retried, quietly, so the disk
// converges and both `posthook status` and a later widen see the truth. An
// unrecognised on-disk value is left for a person to fix: it pauses sync,
// which is tighter still.
func (s *syncLoopState) reassertPolicy(c *config.CloudConfig) {
	disk := diskSyncMode()
	if disk != s.diskMode {
		s.orgMetadataOnly, s.diskMode = false, ""
		fmt.Printf("sync: cloud.sync_mode changed in %s — following it (a metadata-only org policy is re-applied on the next flush)\n", config.Path())
		return
	}
	pinMetadataOnly(c)
	if mode, err := config.NormalizeSyncMode(disk); err != nil || mode == config.SyncModeMetadataOnly {
		return
	}
	// Accepted race: an explicit `--set-sync-mode full` run in the one tick
	// between the directory becoming writable and this write converging the
	// file writes full over full, which is not a change, and is overwritten
	// here; the command has to be run again. The alternatives are worse —
	// lifting on any rewrite while the file still says full ships a batch in
	// full on an unrelated edit, and not retrying leaves the file saying
	// full while the daemon sends metadata-only until a restart.
	if changed, err := config.SetSyncMode(config.SyncModeMetadataOnly); err == nil && changed {
		s.diskMode = diskSyncMode()
		fmt.Printf("sync: org policy is metadata_only — now written to %s\n", config.Path())
	}
}

// diskSyncMode is cloud.sync_mode exactly as config.json holds it: "" when
// unset or unreadable.
func diskSyncMode() string {
	disk, err := config.ReadDisk()
	if err != nil {
		return ""
	}
	return disk.Cloud.SyncMode
}

// flushAndAdopt runs one Flush and adopts the org policy from its reply
// whether or not the flush as a whole succeeded. Flush reads the reply on
// 2xx, before it marks the rows synced; if that marking then fails the policy
// is still in res, and dropping it would send the same rows out again in
// full on the next attempt. Reports whether the server said metadata-only.
func flushAndAdopt(ctx context.Context, db *store.DB, c *config.CloudConfig) (res pksync.Result, orgMetadataOnly bool, err error) {
	res, err = pksync.Flush(ctx, db, *c)
	orgMetadataOnly = adoptServerPolicy(c, res)
	return res, orgMetadataOnly, err
}

// pinMetadataOnly re-asserts a remembered metadata-only org policy on a
// freshly loaded config. An unrecognised sync_mode is left alone: that pauses
// sync altogether, which is tighter still.
func pinMetadataOnly(c *config.CloudConfig) {
	if !c.MetadataOnly() {
		c.SyncMode = config.SyncModeMetadataOnly
	}
}

// watchBinary snapshots the running executable's on-disk identity and returns
// a check reporting whether it has since been replaced. Errors (binary
// unresolvable, or stat racing a reinstall mid-swap) read as "unchanged" —
// the next tick sees the settled file.
func watchBinary() func() bool {
	exe, err := os.Executable()
	if err != nil {
		return func() bool { return false }
	}
	orig, err := os.Stat(exe)
	if err != nil {
		return func() bool { return false }
	}
	return func() bool {
		st, err := os.Stat(exe)
		return err == nil &&
			(!st.ModTime().Equal(orig.ModTime()) || st.Size() != orig.Size())
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
	fmt.Printf("  sync mode:      %s\n", describeSyncMode(cfg.Cloud))
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

// runSyncPreview prints the batch the next flush would send. The banner goes
// to stderr so stdout is pure JSON and `posthook sync --preview | jq` works.
// Config errors (e.g. an unknown sync_mode) surface here the same way they
// would stop a real flush.
func runSyncPreview() error {
	db, err := store.Open()
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	payload, err := pksync.Preview(db, cfg.Cloud)
	if err != nil {
		return err
	}
	rows := 0
	for _, r := range payload.Tables {
		rows += len(r)
	}
	fmt.Fprintf(os.Stderr, "posthook sync --preview: %d pending row(s) across %d table(s); sync mode: %s\n",
		rows, len(payload.Tables), describeSyncMode(cfg.Cloud))
	fmt.Fprintf(os.Stderr, "This is the exact body the next flush would POST to %s. Nothing was sent or marked synced.\n",
		orDash(cfg.Cloud.Endpoint))
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(payload)
}

// describeSyncMode renders the mode with the one-line meaning a reader of
// `posthook status` needs, so nobody has to look the constant up. An
// unrecognised value is called out as such: sync is paused on it.
func describeSyncMode(c config.CloudConfig) string {
	mode, err := c.ResolveSyncMode()
	if err != nil {
		return fmt.Sprintf("⚠ %v — sync is paused until this is fixed (posthook sync --set-sync-mode %s|%s)",
			err, config.SyncModeFull, config.SyncModeMetadataOnly)
	}
	if mode == config.SyncModeMetadataOnly {
		return config.SyncModeMetadataOnly + " (prompts, code and tool output stay on this machine)"
	}
	return config.SyncModeFull + " (prompts and verbatim tool-call payloads are sent)"
}

// adoptServerPolicy tightens this machine when the server's reply says the
// org is metadata-only and the batch just sent went out in full mode: the
// in-memory config flips so the rest of this run is redacted, and the mode is
// persisted so the daemon's next tick starts there. A reply can only ever
// tighten a client — "full" never widens a machine set to metadata_only — so
// a misconfigured or compromised server cannot pull content off a laptop.
// Reports whether the server said metadata-only at all, so a resident loop
// can keep the mode pinned across config reloads even if the write failed.
func adoptServerPolicy(c *config.CloudConfig, res pksync.Result) bool {
	if res.ServerSyncMode != config.SyncModeMetadataOnly {
		return false
	}
	if c.MetadataOnly() {
		return true
	}
	c.SyncMode = config.SyncModeMetadataOnly
	changed, err := config.SetSyncMode(config.SyncModeMetadataOnly)
	switch {
	case err != nil:
		fmt.Fprintf(os.Stderr, "sync: org policy is metadata_only; switched this run but could not update %s: %v\n", config.Path(), err)
	case changed:
		fmt.Printf("sync: org policy is metadata_only — this machine now syncs metadata only (%s)\n", config.Path())
	}
	return true
}

func runSyncConfigure(endpoint, token, enabled, syncMode string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	// Validate before touching the file: a privacy setting must never be
	// half-applied or silently coerced.
	if syncMode != "" {
		mode, err := config.NormalizeSyncMode(syncMode)
		if err != nil {
			return err
		}
		syncMode = mode
	}
	// Load() applies env overrides; strip them before save so we persist only
	// what the user explicitly set on this command line plus any prior file
	// contents.
	disk, err := config.ReadDisk()
	if err != nil {
		return err
	}
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
	if syncMode != "" {
		disk.Cloud.SyncMode = syncMode
	}
	if disk.Cloud.FlushIntervalSecs <= 0 {
		disk.Cloud.FlushIntervalSecs = cfg.Cloud.FlushIntervalSecs
	}
	if err := config.Save(disk); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n", config.Path())
	fmt.Printf("  enabled:   %v\n", disk.Cloud.Enabled)
	fmt.Printf("  endpoint:  %s\n", orDash(disk.Cloud.Endpoint))
	fmt.Printf("  token:     %s\n", maskToken(disk.Cloud.Token))
	fmt.Printf("  sync mode: %s\n", describeSyncMode(disk.Cloud))
	return nil
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
