// Package sync flushes locally-captured posthook rows to the cloud ingest
// endpoint. The local SQLite store is authoritative — sync is a faithful
// upstream replica, no redaction, no schema rewrites.
//
// Each syncable table carries a synced_at TEXT column (NULL = pending) — the
// only sync state we keep, a per-row "what's new since last flush" cursor.
// Flush reads up to batchSize pending rows per table in FK order, ships them in
// a single POST to {endpoint}/posthook/ingest with a Bearer token, and on 2xx
// marks the rows synced_at = NOW() in a transaction. The server appends the
// body as one blob (append-only, at-least-once); there is no idempotency key.
// Failures are recorded to the sync_state table for `posthook sync --status`.
//
// Because the landing zone is append-only and dbt dedups (latest received_at
// wins per source key) at the staging boundary, two things resolve for free:
// a lost-response retry that re-sends rows is harmless, and recomputed rows
// (commit_sessions / commit_session_files are rebuilt via DELETE+INSERT in
// attribution.go) simply re-sync as a newer blob and win the dedup — no
// server-side per-commit reconciliation needed.
package sync

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bilanc/posthook/internal/config"
	"github.com/bilanc/posthook/internal/store"
)

const (
	defaultBatchSize = 1000
	httpTimeout      = 30 * time.Second
	ingestPath       = "/posthook/ingest"
)

// tablePrimaryKeys maps each syncable table to the columns that uniquely
// identify a row. Used by markSynced to scope its UPDATE to exactly the rows
// we just shipped (other writers may have inserted more pending rows since
// we SELECTed).
var tablePrimaryKeys = map[string][]string{
	"repositories":         {"id"},
	"sessions":             {"id"},
	"events":               {"id"},
	"commits":              {"id"},
	"commit_files":         {"commit_id", "file_path"},
	"commit_sessions":      {"commit_id", "session_id"},
	"commit_session_files": {"commit_id", "session_id", "file_path"},
	"event_line_ranges":    {"id"},
	"session_prompts":      {"id"},
}

// Result is the per-flush summary surfaced by `posthook sync --once`.
type Result struct {
	Synced     map[string]int `json:"synced"`
	DurationMS int64          `json:"duration_ms"`
	Skipped    bool           `json:"skipped,omitempty"`
	Reason     string         `json:"reason,omitempty"`
}

type ingestPayload struct {
	SchemaVersion int                         `json:"schema_version"`
	Tables        map[string][]map[string]any `json:"tables"`
}

// Flush pulls up to batchSize pending rows per syncable table, ships them in
// one POST, and marks them synced on 2xx. Safe to run concurrently with the
// ingest pipeline — SetMaxOpenConns(1) + WAL serialize writes.
func Flush(ctx context.Context, db *store.DB, cfg config.CloudConfig) (Result, error) {
	start := time.Now()
	res := Result{Synced: map[string]int{}}

	if !cfg.Enabled {
		res.Skipped, res.Reason = true, "cloud.enabled=false"
		return res, nil
	}
	if cfg.Endpoint == "" || cfg.Token == "" {
		res.Skipped, res.Reason = true, "missing endpoint or token"
		return res, nil
	}

	type pendingRows struct {
		rows []map[string]any
		keys []map[string]any
	}
	pending := map[string]pendingRows{}
	// v9: adds the session_prompts table (typed prompts per session).
	payload := ingestPayload{SchemaVersion: 9, Tables: map[string][]map[string]any{}}

	for _, table := range store.SyncableTables {
		rows, keys, err := selectPending(db, table, defaultBatchSize)
		if err != nil {
			recordError(db, table, err)
			return res, fmt.Errorf("select pending from %s: %w", table, err)
		}
		if len(rows) == 0 {
			continue
		}
		pending[table] = pendingRows{rows: rows, keys: keys}
		payload.Tables[table] = rows
	}
	if len(payload.Tables) == 0 {
		res.DurationMS = time.Since(start).Milliseconds()
		return res, nil
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return res, err
	}
	url := strings.TrimRight(cfg.Endpoint, "/") + ingestPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return res, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("User-Agent", "posthook-sync/1")

	client := &http.Client{Timeout: httpTimeout}
	resp, err := client.Do(req)
	if err != nil {
		for t := range pending {
			recordError(db, t, err)
		}
		return res, fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		ingestErr := fmt.Errorf("ingest returned %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
		for t := range pending {
			recordError(db, t, ingestErr)
		}
		return res, ingestErr
	}
	_, _ = io.Copy(io.Discard, resp.Body)

	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, table := range store.SyncableTables {
		p, ok := pending[table]
		if !ok {
			continue
		}
		if err := markSynced(db, table, p.keys, now); err != nil {
			return res, fmt.Errorf("mark synced %s: %w", table, err)
		}
		recordSuccess(db, table, len(p.rows))
		res.Synced[table] = len(p.rows)
	}
	res.DurationMS = time.Since(start).Milliseconds()
	return res, nil
}

// selectPending streams up to limit unsync'd rows from table as
// map[column]value, plus a primary-key-only projection used by markSynced.
// synced_at is stripped from the payload — it's local-only metadata.
func selectPending(db *store.DB, table string, limit int) ([]map[string]any, []map[string]any, error) {
	pkCols, ok := tablePrimaryKeys[table]
	if !ok {
		return nil, nil, fmt.Errorf("unknown table %q", table)
	}
	pkSet := map[string]bool{}
	for _, c := range pkCols {
		pkSet[c] = true
	}

	q := fmt.Sprintf(`SELECT * FROM %s WHERE synced_at IS NULL LIMIT ?`, table)
	rows, err := db.Query(q, limit)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, nil, err
	}

	var outRows, outKeys []map[string]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, nil, err
		}
		row := make(map[string]any, len(cols))
		key := make(map[string]any, len(pkCols))
		for i, c := range cols {
			if c == "synced_at" {
				continue
			}
			v := normalize(vals[i])
			row[c] = v
			if pkSet[c] {
				key[c] = v
			}
		}
		outRows = append(outRows, row)
		outKeys = append(outKeys, key)
	}
	return outRows, outKeys, rows.Err()
}

// normalize unwraps SQLite TEXT columns from []byte to string so the JSON
// encoder produces "foo" instead of base64("foo").
func normalize(v any) any {
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return v
}

// markSynced sets synced_at = ts for exactly the rows whose primary-key
// values appear in keys. Runs in a single transaction with a prepared
// statement so 1000 marks against local SQLite are fast.
func markSynced(db *store.DB, table string, keys []map[string]any, ts string) error {
	if len(keys) == 0 {
		return nil
	}
	pkCols, ok := tablePrimaryKeys[table]
	if !ok {
		return fmt.Errorf("unknown table %q", table)
	}
	whereParts := make([]string, len(pkCols))
	for i, c := range pkCols {
		whereParts[i] = c + " = ?"
	}
	q := fmt.Sprintf(`UPDATE %s SET synced_at = ? WHERE %s`, table, strings.Join(whereParts, " AND "))

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.Prepare(q)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, k := range keys {
		args := make([]any, 0, 1+len(pkCols))
		args = append(args, ts)
		for _, c := range pkCols {
			args = append(args, k[c])
		}
		if _, err := stmt.Exec(args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func recordSuccess(db *store.DB, table string, rowCount int) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = db.Exec(`
		INSERT INTO sync_state (table_name, last_attempt_at, last_success_at, last_row_count, last_error)
		VALUES (?, ?, ?, ?, NULL)
		ON CONFLICT(table_name) DO UPDATE SET
			last_attempt_at = excluded.last_attempt_at,
			last_success_at = excluded.last_success_at,
			last_row_count  = excluded.last_row_count,
			last_error      = NULL`,
		table, now, now, rowCount)
}

func recordError(db *store.DB, table string, err error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = db.Exec(`
		INSERT INTO sync_state (table_name, last_attempt_at, last_error)
		VALUES (?, ?, ?)
		ON CONFLICT(table_name) DO UPDATE SET
			last_attempt_at = excluded.last_attempt_at,
			last_error      = excluded.last_error`,
		table, now, err.Error())
}

// Status row from sync_state, surfaced by `posthook sync --status`.
type StatusRow struct {
	Table         string         `json:"table"`
	LastAttempt   sql.NullString `json:"last_attempt_at"`
	LastSuccess   sql.NullString `json:"last_success_at"`
	LastRowCount  sql.NullInt64  `json:"last_row_count"`
	LastError     sql.NullString `json:"last_error"`
	PendingRows   int            `json:"pending_rows"`
}

// ReadStatus returns per-table sync_state plus a live count of pending rows.
func ReadStatus(db *store.DB) ([]StatusRow, error) {
	var out []StatusRow
	for _, table := range store.SyncableTables {
		var row StatusRow
		row.Table = table
		err := db.QueryRow(`
			SELECT last_attempt_at, last_success_at, last_row_count, last_error
			FROM sync_state WHERE table_name = ?`, table).
			Scan(&row.LastAttempt, &row.LastSuccess, &row.LastRowCount, &row.LastError)
		if err != nil && err != sql.ErrNoRows {
			return nil, err
		}
		if err := db.QueryRow(fmt.Sprintf(
			`SELECT COUNT(*) FROM %s WHERE synced_at IS NULL`, table)).Scan(&row.PendingRows); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
}
