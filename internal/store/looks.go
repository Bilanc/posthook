package store

import (
	"database/sql"
	"encoding/json"
	"time"
)

// Working state for looks at a working tree (ingest/looks.go).
//
// An edit made by a script, a formatter or a shell command names no file and
// carries no text in its hook payload. posthook finds it by looking at the
// repository afterwards: which files differ from what it last saw. Two
// tables hold what that needs between events:
//
//   - pending_looks: the looks that are due. One is queued when a shell tool
//     call ends and when a prompt is submitted, and run once the worker has
//     caught up with every event that had fired by then, so an edit made
//     right after a shell command claims its own lines first.
//   - repo_looks: per repository, the commit HEAD pointed at the last time
//     it was looked at, and when. A look compares against that to tell lines
//     a commit made here brought in from lines the working tree gained, and
//     is what makes the first look at a repository record without crediting.
//
// Both are working state, not history, and never leave the machine.

// Look kinds.
const (
	LookShell  = "shell"
	LookPrompt = "prompt"
)

// PendingLook is one look that is due.
type PendingLook struct {
	ID        int64
	Kind      string
	EventID   string
	SessionID string
	Agent     string
	EventTS   string
	Root      string
	FiredAt   time.Time
	// StartedAt is when the session's previous event fired: the shell call
	// ran some time between the two. Zero when that is not known.
	StartedAt time.Time
	// Command is the text of the shell command.
	Command string
	// MovesContent: the command ran a git command that brings existing
	// content into the working tree, so nothing it finds is credited.
	MovesContent bool
	// Commits: the command ran `git commit`.
	Commits bool
	// Files are paths that hold lines kept for this look (FileMap.HeldFor).
	Files []string
}

// Author is the look's event as the author of the lines it is credited.
func (l PendingLook) Author(held bool) LineAuthor {
	return LineAuthor{EventID: l.EventID, SessionID: l.SessionID, Agent: l.Agent, TS: l.EventTS, Held: held}
}

// AddPendingLook queues a look.
func (db *DB) AddPendingLook(l PendingLook) error {
	_, err := db.Exec(`
		INSERT INTO pending_looks (kind, event_id, session_id, agent_slug, event_ts, root, fired_at, started_at, command, moves_content, commits)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		l.Kind, l.EventID, nullableOf(l.SessionID), nullableOf(l.Agent), nullableOf(l.EventTS), l.Root,
		l.FiredAt.UTC().Format(time.RFC3339Nano), nullableTime(l.StartedAt), nullableOf(l.Command), l.MovesContent, l.Commits)
	return err
}

const pendingLookColumns = `id, kind, event_id, COALESCE(session_id, ''), COALESCE(agent_slug, ''),
	COALESCE(event_ts, ''), root, fired_at, COALESCE(started_at, ''), COALESCE(command, ''),
	moves_content, commits, COALESCE(files, '')`

func scanPendingLook(scan func(...any) error) (PendingLook, error) {
	var l PendingLook
	var fired, started, files string
	if err := scan(&l.ID, &l.Kind, &l.EventID, &l.SessionID, &l.Agent, &l.EventTS, &l.Root, &fired, &started, &l.Command,
		&l.MovesContent, &l.Commits, &files); err != nil {
		return l, err
	}
	l.FiredAt, _ = time.Parse(time.RFC3339Nano, fired)
	if started != "" {
		l.StartedAt, _ = time.Parse(time.RFC3339Nano, started)
	}
	if files != "" {
		_ = json.Unmarshal([]byte(files), &l.Files)
	}
	return l, nil
}

// PendingLooks returns the looks whose hook fired no later than before,
// oldest first.
func (db *DB) PendingLooks(before time.Time) ([]PendingLook, error) {
	rows, err := db.Query(`SELECT ` + pendingLookColumns + ` FROM pending_looks ORDER BY fired_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingLook
	for rows.Next() {
		l, err := scanPendingLook(rows.Scan)
		if err != nil {
			return nil, err
		}
		if !l.FiredAt.After(before) {
			out = append(out, l)
		}
	}
	return out, rows.Err()
}

// CountPendingLooks is how many looks are due or still settling.
func (db *DB) CountPendingLooks() int {
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM pending_looks`).Scan(&n)
	return n
}

// PendingShellLook returns the newest shell look queued for root whose hook
// fired no later than firedBy, other than the one for event except.
func (db *DB) PendingShellLook(root string, firedBy time.Time, except string) (PendingLook, bool) {
	rows, err := db.Query(`SELECT `+pendingLookColumns+` FROM pending_looks WHERE root = ? AND kind = ? AND event_id != ? ORDER BY fired_at DESC, id DESC`,
		root, LookShell, except)
	if err != nil {
		return PendingLook{}, false
	}
	defer rows.Close()
	for rows.Next() {
		l, err := scanPendingLook(rows.Scan)
		if err != nil {
			return PendingLook{}, false
		}
		if !l.FiredAt.After(firedBy) {
			return l, true
		}
	}
	return PendingLook{}, false
}

// CommittingLooks returns the queued shell looks at root whose command ran
// `git commit`.
func (db *DB) CommittingLooks(root string) []PendingLook {
	rows, err := db.Query(`SELECT `+pendingLookColumns+` FROM pending_looks WHERE root = ? AND kind = ? AND commits != 0`, root, LookShell)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []PendingLook
	for rows.Next() {
		if l, err := scanPendingLook(rows.Scan); err == nil {
			out = append(out, l)
		}
	}
	return out
}

func nullableTime(t time.Time) sql.NullString {
	if t.IsZero() {
		return sql.NullString{}
	}
	return sql.NullString{String: t.UTC().Format(time.RFC3339Nano), Valid: true}
}

// HoldFileForLook notes that path has lines held for a look.
func (db *DB) HoldFileForLook(id int64, path string) error {
	var raw sql.NullString
	if err := db.QueryRow(`SELECT files FROM pending_looks WHERE id = ?`, id).Scan(&raw); err != nil {
		return err
	}
	var files []string
	if raw.Valid && raw.String != "" {
		_ = json.Unmarshal([]byte(raw.String), &files)
	}
	for _, f := range files {
		if f == path {
			return nil
		}
	}
	encoded, err := json.Marshal(append(files, path))
	if err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE pending_looks SET files = ? WHERE id = ?`, string(encoded), id)
	return err
}

// DeletePendingLook removes a look that has been run.
func (db *DB) DeletePendingLook(id int64) error {
	_, err := db.Exec(`DELETE FROM pending_looks WHERE id = ?`, id)
	return err
}

// RepoLook is what the last look at a repository saw.
type RepoLook struct {
	// Head is the commit HEAD pointed at, empty in a repository with no
	// commit yet.
	Head     string
	LookedAt time.Time
	// SlowUntil: git did not answer in time; no look before this instant.
	SlowUntil time.Time
	// Partial: more files had changed than the look reads. A changed file
	// with no map may then have been changed before that look, unseen.
	Partial bool
	// Blind: a look that was due since then could not be made (git did not
	// answer). What has changed since LookedAt can no longer be put down to
	// one command, so the next look that succeeds only records.
	Blind bool
}

// RepoLook returns the last look at root.
func (db *DB) RepoLook(root string) (RepoLook, bool) {
	var head, slow sql.NullString
	var at string
	var partial, blind bool
	if err := db.QueryRow(`SELECT head, looked_at, slow_until, partial, blind FROM repo_looks WHERE root = ?`, root).Scan(&head, &at, &slow, &partial, &blind); err != nil {
		return RepoLook{}, false
	}
	l := RepoLook{Head: head.String, Partial: partial, Blind: blind}
	l.LookedAt, _ = time.Parse(time.RFC3339Nano, at)
	if slow.Valid {
		l.SlowUntil, _ = time.Parse(time.RFC3339Nano, slow.String)
	}
	return l, true
}

// SaveRepoLook records a look at root.
func (db *DB) SaveRepoLook(root string, l RepoLook) error {
	var slow sql.NullString
	if !l.SlowUntil.IsZero() {
		slow = sql.NullString{String: l.SlowUntil.UTC().Format(time.RFC3339Nano), Valid: true}
	}
	_, err := db.Exec(`
		INSERT INTO repo_looks (root, head, looked_at, slow_until, partial, blind) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(root) DO UPDATE SET head = excluded.head, looked_at = excluded.looked_at,
		  slow_until = excluded.slow_until, partial = excluded.partial, blind = excluded.blind`,
		root, nullableOf(l.Head), l.LookedAt.UTC().Format(time.RFC3339Nano), slow, l.Partial, l.Blind)
	return err
}
