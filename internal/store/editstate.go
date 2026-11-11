package store

import (
	"bytes"
	"compress/gzip"
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
//   - file_snapshots: the content of each file as the last agent edit left
//     it. A tool that overwrites a whole file reports only the new content;
//     diffing it against the snapshot is what turns "the file is 300 lines"
//     into "this write changed 4".
//   - session_file_lines: for each session and file, the lines that session
//     has written and not since removed. When a later edit of the same
//     session removes one of them, that line is rework, not new output.
//
// Both tables hold file content (snapshots verbatim, lines as hashes), so
// neither is in SyncableTables and nothing here ever leaves the machine.
// Rows are dropped once they have not been touched for editStateMaxAge:
// after that long the file has moved on and the snapshot is no better a
// baseline than HEAD.

// maxSnapshotBytes is the largest file a snapshot is kept for. A larger file
// is diffed against HEAD instead.
const maxSnapshotBytes = 1 << 20

// maxSessionFileLines bounds how many written lines are remembered for one
// session and file. Past it the oldest are forgotten.
const maxSessionFileLines = 50_000

const editStateMaxAge = 30 * 24 * time.Hour

// lastEditStatePruneKey is the maintenance row gating PruneEditStateIfDue.
const lastEditStatePruneKey = "edit_state_last_prune_at"

// FileSnapshot returns the content of path as the last agent edit left it.
func (db *DB) FileSnapshot(path string) (string, bool) {
	var blob []byte
	if err := db.QueryRow(`SELECT content FROM file_snapshots WHERE file_path = ?`, path).Scan(&blob); err != nil {
		return "", false
	}
	zr, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		return "", false
	}
	defer zr.Close()
	content, err := io.ReadAll(io.LimitReader(zr, maxSnapshotBytes+1))
	if err != nil || len(content) > maxSnapshotBytes {
		return "", false
	}
	return string(content), true
}

// SaveFileSnapshot records content as the state an agent edit left path in.
// A file over maxSnapshotBytes is not kept, and any older snapshot of it is
// dropped so a later write is not diffed against a stale copy.
func (db *DB) SaveFileSnapshot(path string, content []byte) error {
	if len(content) > maxSnapshotBytes {
		_, err := db.Exec(`DELETE FROM file_snapshots WHERE file_path = ?`, path)
		return err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(content); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	_, err := db.Exec(`
		INSERT INTO file_snapshots (file_path, content, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(file_path) DO UPDATE SET content = excluded.content, updated_at = excluded.updated_at`,
		path, buf.Bytes(), time.Now().UTC().Format(time.RFC3339))
	return err
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
	out := make([]uint64, 0, len(blob)/8)
	for i := 0; i+8 <= len(blob); i += 8 {
		out = append(out, binary.BigEndian.Uint64(blob[i:]))
	}
	return out
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
	blob := make([]byte, 8*len(hashes))
	for i, h := range hashes {
		binary.BigEndian.PutUint64(blob[8*i:], h)
	}
	_, err := db.Exec(`
		INSERT INTO session_file_lines (session_id, file_path, line_hashes, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(session_id, file_path) DO UPDATE SET line_hashes = excluded.line_hashes, updated_at = excluded.updated_at`,
		sessionID, path, blob, time.Now().UTC().Format(time.RFC3339))
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
