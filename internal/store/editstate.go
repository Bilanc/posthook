package store

import (
	"encoding/binary"
	"hash/fnv"
	"io"
	"strings"
	"time"
)

// Local working state for exact edit capture (ingest/capture.go).
//
// Two things have to be remembered between agent events to say exactly what
// an edit changed, and neither is in a hook payload:
//
//   - file_snapshots: each file as the last agent edit left it, as one
//     fingerprint per line. A tool that overwrites a whole file reports only
//     the new content; diffing it against the snapshot is what turns "the
//     file is 300 lines" into "this write changed 4".
//   - session_file_lines: for each session and file, the lines that session
//     has written and not since removed. When a later edit of the same
//     session removes one of them, that line is rework, not new output.
//
// Neither table holds file content. Both hold LineHash fingerprints, which
// is all a line diff and a rework match need: the text of an added line is
// always in the event being handled, and a removed line only has to be
// recognised, never read. They are still derived from file content, so
// neither is in SyncableTables and nothing here ever leaves the machine.
// Rows are dropped once they have not been touched for editStateMaxAge:
// after that long the file has moved on and the snapshot is no better a
// baseline than HEAD.

// MaxSnapshotBytes is the largest file a snapshot is kept for. A larger file
// is diffed against HEAD instead.
const MaxSnapshotBytes = 1 << 20

// maxSessionFileLines bounds how many written lines are remembered for one
// session and file. Past it the oldest are forgotten.
const maxSessionFileLines = 50_000

const editStateMaxAge = 30 * 24 * time.Hour

// lastEditStatePruneKey is the maintenance row gating PruneEditStateIfDue.
const lastEditStatePruneKey = "edit_state_last_prune_at"

// FileSnapshot returns path as the last agent edit left it, one fingerprint
// per line, and the instant that content was true: when the event that
// wrote it fired, if the content came from the event itself, or when the
// file was read from disk otherwise. A caller that needs the state before
// some event must not use a snapshot dated after that event fired; the read
// may already have seen the event's own write.
func (db *DB) FileSnapshot(path string) (hashes []uint64, asOf time.Time, ok bool) {
	var blob []byte
	var at string
	if err := db.QueryRow(`SELECT line_hashes, updated_at FROM file_snapshots WHERE file_path = ?`, path).Scan(&blob, &at); err != nil {
		return nil, time.Time{}, false
	}
	// An unparseable instant reads as the zero time, which is before every
	// event: the snapshot is then always usable, as it was before instants
	// were recorded.
	asOf, _ = time.Parse(time.RFC3339Nano, at)
	return decodeHashes(blob), asOf, true
}

// SaveFileSnapshot records hashes as the state of path at asOf. An empty
// list is a valid snapshot of an empty file.
func (db *DB) SaveFileSnapshot(path string, hashes []uint64, asOf time.Time) error {
	_, err := db.Exec(`
		INSERT INTO file_snapshots (file_path, line_hashes, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(file_path) DO UPDATE SET line_hashes = excluded.line_hashes, updated_at = excluded.updated_at`,
		path, encodeHashes(hashes), asOf.UTC().Format(time.RFC3339Nano))
	return err
}

// DropFileSnapshot forgets the snapshot of path. Used when the file has
// grown past MaxSnapshotBytes, so a later write is not diffed against a
// stale copy.
func (db *DB) DropFileSnapshot(path string) error {
	_, err := db.Exec(`DELETE FROM file_snapshots WHERE file_path = ?`, path)
	return err
}

func encodeHashes(hashes []uint64) []byte {
	blob := make([]byte, 8*len(hashes))
	for i, h := range hashes {
		binary.BigEndian.PutUint64(blob[8*i:], h)
	}
	return blob
}

func decodeHashes(blob []byte) []uint64 {
	out := make([]uint64, 0, len(blob)/8)
	for i := 0; i+8 <= len(blob); i += 8 {
		out = append(out, binary.BigEndian.Uint64(blob[i:]))
	}
	return out
}

// LineHash identifies a line of text for rework matching. A trailing CR is
// ignored, so a line reads the same in an LF payload and a CRLF file.
func LineHash(line string) uint64 {
	h := fnv.New64a()
	_, _ = io.WriteString(h, strings.TrimSuffix(line, "\r"))
	return h.Sum64()
}

// SessionFileLines returns the hashes of the lines sessionID has written to
// path and not since removed, oldest first, one entry per line.
func (db *DB) SessionFileLines(sessionID, path string) []uint64 {
	var blob []byte
	if err := db.QueryRow(`SELECT line_hashes FROM session_file_lines WHERE session_id = ? AND file_path = ?`,
		sessionID, path).Scan(&blob); err != nil {
		return nil
	}
	return decodeHashes(blob)
}

// SaveSessionFileLines replaces the remembered lines for sessionID and path.
func (db *DB) SaveSessionFileLines(sessionID, path string, hashes []uint64) error {
	if len(hashes) == 0 {
		_, err := db.Exec(`DELETE FROM session_file_lines WHERE session_id = ? AND file_path = ?`, sessionID, path)
		return err
	}
	if len(hashes) > maxSessionFileLines {
		hashes = hashes[len(hashes)-maxSessionFileLines:]
	}
	_, err := db.Exec(`
		INSERT INTO session_file_lines (session_id, file_path, line_hashes, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(session_id, file_path) DO UPDATE SET line_hashes = excluded.line_hashes, updated_at = excluded.updated_at`,
		sessionID, path, encodeHashes(hashes), time.Now().UTC().Format(time.RFC3339))
	return err
}

// PruneEditStateIfDue drops snapshots and remembered lines untouched for
// editStateMaxAge, at most once per pruneInterval. It runs whatever the
// retention setting is: this is working state, not history. ran is false
// when the last pass was less than a day ago.
func (db *DB) PruneEditStateIfDue() (removed int64, ran bool, err error) {
	var v string
	if db.QueryRow(`SELECT value FROM maintenance WHERE key = ?`, lastEditStatePruneKey).Scan(&v) == nil {
		if last, perr := time.Parse(time.RFC3339, v); perr == nil {
			if since := time.Since(last); since >= 0 && since < pruneInterval {
				return 0, false, nil
			}
		}
	}
	cutoff := time.Now().UTC().Add(-editStateMaxAge).Format(time.RFC3339)
	for _, table := range []string{"file_snapshots", "session_file_lines"} {
		r, err := db.Exec(`DELETE FROM `+table+` WHERE updated_at < ?`, cutoff)
		if err != nil {
			return removed, true, err
		}
		n, _ := r.RowsAffected()
		removed += n
	}
	if err := db.ensureMaintenanceTable(); err != nil {
		return removed, true, err
	}
	_, err = db.Exec(`INSERT INTO maintenance (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		lastEditStatePruneKey, time.Now().UTC().Format(time.RFC3339))
	return removed, true, err
}
