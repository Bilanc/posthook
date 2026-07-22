package store

import "fmt"

// applyMigrations runs idempotent ALTER TABLE guards for every schema bump.
// Each block is gated on existence of a column so re-runs are safe.
func (db *DB) applyMigrations() error {
	// v1 → v2: events gains repo_id and rel_file_path.
	eventCols, err := db.columnSet("events")
	if err != nil {
		return err
	}
	if !eventCols["repo_id"] {
		if _, err := db.Exec(`ALTER TABLE events ADD COLUMN repo_id TEXT REFERENCES repositories(id)`); err != nil {
			return err
		}
	}
	if !eventCols["rel_file_path"] {
		if _, err := db.Exec(`ALTER TABLE events ADD COLUMN rel_file_path TEXT`); err != nil {
			return err
		}
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_events_repo ON events(repo_id)`); err != nil {
		return err
	}

	// v2 → v3: drop cost/token columns. Cross-agent counts and static
	// pricing both produced misleading numbers. The on-disk transcript still
	// has tokens if anyone needs them.
	sessionCols, err := db.columnSet("sessions")
	if err != nil {
		return err
	}
	for _, dead := range []string{
		"tokens_in", "tokens_out", "tokens_cache_read", "tokens_cache_write",
		"cost_usd", "tool_calls_count",
	} {
		if sessionCols[dead] {
			if _, err := db.Exec(fmt.Sprintf(`ALTER TABLE sessions DROP COLUMN %s`, dead)); err != nil {
				return err
			}
		}
	}
	eventColsV3, err := db.columnSet("events")
	if err != nil {
		return err
	}
	for _, dead := range []string{"tokens_in", "tokens_out"} {
		if eventColsV3[dead] {
			if _, err := db.Exec(fmt.Sprintf(`ALTER TABLE events DROP COLUMN %s`, dead)); err != nil {
				return err
			}
		}
	}

	// v4 → v5: sessions gain engineer_email and engineer_name. Captured from
	// `git config user.email` / `user.name` at session creation time.
	sessionColsV5, err := db.columnSet("sessions")
	if err != nil {
		return err
	}
	if !sessionColsV5["engineer_email"] {
		if _, err := db.Exec(`ALTER TABLE sessions ADD COLUMN engineer_email TEXT`); err != nil {
			return err
		}
	}
	if !sessionColsV5["engineer_name"] {
		if _, err := db.Exec(`ALTER TABLE sessions ADD COLUMN engineer_name TEXT`); err != nil {
			return err
		}
	}

	// v6 → v7: every syncable table gains synced_at TEXT NULL. Rows with
	// synced_at IS NULL are pending upload to the cloud endpoint; the sync
	// flush sets it to NOW() on 2xx, and ingest UPDATEs clear it again when
	// mutable rows change (sessions, repositories).
	for _, table := range SyncableTables {
		cols, err := db.columnSet(table)
		if err != nil {
			return err
		}
		if !cols["synced_at"] {
			if _, err := db.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN synced_at TEXT`, table)); err != nil {
				return err
			}
		}
		idx := fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_%s_synced ON %s(synced_at)`, table, table)
		if _, err := db.Exec(idx); err != nil {
			return err
		}
	}

	// v7 → v8: sessions re-gain token columns, this time NULLable. v3 dropped
	// them because cross-agent zeros were misleading; NULL now means "agent
	// doesn't report usage" (Cursor) while Claude Code and Codex fill them
	// from the Stop-hook transcript. Names differ from the v3 set on purpose —
	// the v3 drop block above still removes the old names on every open.
	sessionColsV8, err := db.columnSet("sessions")
	if err != nil {
		return err
	}
	for _, col := range []string{
		"input_tokens", "output_tokens", "cache_read_tokens", "cache_creation_tokens",
	} {
		if !sessionColsV8[col] {
			if _, err := db.Exec(fmt.Sprintf(`ALTER TABLE sessions ADD COLUMN %s INTEGER`, col)); err != nil {
				return err
			}
		}
	}

	// v13: dashboard event aggregation uses session_id/event_type. Cursor's
	// duplicate check matches PostToolUse to afterFileEdit by agent, event,
	// file, session, and nearby timestamp.
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_events_agent_type_file_session_ts
		ON events(agent_slug, event_type, file_path, session_id, ts)`); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_events_session_type
		ON events(session_id, event_type)`); err != nil {
		return err
	}
	return nil
}

func (db *DB) columnSet(table string) (map[string]bool, error) {
	rows, err := db.Query(fmt.Sprintf(`SELECT name FROM pragma_table_info('%s')`, table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

// normalizeEventTypes canonicalizes Cursor's camelCase event names to the
// PascalCase form Claude/Codex use, so cross-agent metrics share a single
// predicate.
func (db *DB) normalizeEventTypes() error {
	if _, err := db.Exec(`UPDATE events SET event_type = 'PreToolUse' WHERE event_type = 'preToolUse'`); err != nil {
		return err
	}
	_, err := db.Exec(`UPDATE events SET event_type = 'PostToolUse' WHERE event_type = 'postToolUse'`)
	return err
}
