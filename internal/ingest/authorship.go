package ingest

import (
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/bilanc/posthook/internal/lineranges"
	"github.com/bilanc/posthook/internal/logx"
	"github.com/bilanc/posthook/internal/store"
)

// Keeping the authorship map (store/filemap.go) up to date from agent edits.
//
// Every edit event already leaves a snapshot of the file it wrote (see
// capture.go). The map is that snapshot with an author per line, so it is
// updated in the same step: the previous map is lined up with the content
// the event left, lines the two share keep their author, and the lines this
// event added become its own. A line that is in neither, something changed
// in the file between two agent edits, has no author: nobody posthook saw
// wrote it.
//
// The positions of the event's lines have to be positions in the content
// the snapshot is taken from. For an event that says what the file became
// (a Write, a shell heredoc, a Claude Code edit, whose patch is applied to
// the previous content it reports) that is the event's own text, true from
// the moment the hook fired whatever the disk holds by the time the worker
// gets to it. For the others it is the file as read.

// wrote records the lines this event added, as ranges in the content its
// snapshot is taken from.
func (fc *fileCapture) wrote(ranges []lineranges.LineRange) {
	fc.authored = append(fc.authored[:0:0], ranges...)
}

// updateAuthorship saves the state this event left the file in, with the
// author of each line. It replaces the plain snapshot save. firedAt is when
// the event's hook fired.
func (fc *fileCapture) updateAuthorship(db *store.DB, author store.LineAuthor, firedAt time.Time) error {
	lines := lineranges.Lines(fc.snapshotText)
	prev, found := db.FileMap(fc.path)
	if found && !prev.HasAuthors() {
		prev = seedMapAuthors(db, fc.root, fc.rel.String, prev, author.EventID)
	}

	if found && prev.AsOf.After(fc.snapshotAsOf) {
		// The row is a later state of the file than the one this event left:
		// an event that fired after this one was handled first, or the row
		// was read from a disk that already held later edits. Putting the
		// older content back would lose what came after, so the row stays
		// and this event's lines are found in it.
		if author.SessionID == "" || len(fc.authored) == 0 {
			return nil
		}
		mine := make([]uint64, len(lines))
		theirs := prev.Norms
		exact := len(prev.Norms) != len(prev.Hashes)
		if exact {
			theirs = prev.Hashes
		}
		for i, l := range lines {
			if exact {
				mine[i] = store.LineHash(l)
			} else {
				mine[i] = store.NormHash(l)
			}
		}
		match := lineranges.AlignHashes(mine, theirs)
		// taken are this event's lines that the row has under another
		// event's name, by that event.
		taken := map[string][]uint64{}
		holders := map[string]store.LineAuthor{}
		for _, r := range fc.authored {
			for n := r.StartLine; n <= r.EndLine; n++ {
				if n < 1 || n > len(match) || match[n-1] < 0 {
					continue
				}
				at := match[n-1]
				if was, ok := prev.AuthorOf(at); ok && was.EventID != author.EventID {
					taken[was.EventID] = append(taken[was.EventID], prev.Hashes[at])
					holders[was.EventID] = was
				}
				prev.SetAuthor(at, author)
			}
		}
		if err := db.SaveFileMap(fc.path, prev); err != nil {
			return err
		}
		for eventID, hashes := range taken {
			takeBackFromLook(db, holders[eventID], fc.path, hashes, prev)
		}
		return nil
	}

	next, from := prev.Carry(lines)
	next.AsOf = fc.snapshotAsOf
	next.Blob = gitBlobID(fc.snapshotText)
	mine := make([]bool, len(lines))
	for _, r := range fc.authored {
		for n := r.StartLine; n <= r.EndLine; n++ {
			if n >= 1 && n <= len(lines) {
				mine[n-1] = true
				if author.SessionID != "" {
					next.SetAuthor(n-1, author)
				}
			}
		}
	}
	// Lines that are in neither the previous copy nor this edit changed some
	// other way since the last look at the file. When a shell tool call ran
	// in between and its look at the working tree is still due (looks.go),
	// they may be what that command wrote: they are held for the look, which
	// confirms or drops them. Otherwise they have no author.
	//
	// A file with no map yet is taken to have been unchanged at the last
	// look, so all of it that is not in git is new. After a look that could
	// not read every changed file that does not follow (see runLook), and
	// nothing is held in such a file: its lines may have been there, unseen.
	if fc.root != "" {
		if look, due := db.PendingShellLook(fc.root, firedAt, author.EventID); due && firedAt.Sub(look.FiredAt) <= maxLookAge &&
			(found || !lastLookWasPartial(db, fc.root)) {
			held := false
			holder := look.Author(true)
			for j := range lines {
				if from[j] < 0 && !mine[j] {
					next.SetAuthor(j, holder)
					held = true
				}
			}
			if held {
				if err := db.HoldFileForLook(look.ID, fc.path); err != nil {
					return err
				}
			}
		}
	}
	return db.SaveFileMap(fc.path, next)
}

// takeBackFromLook undoes the credit a look gave a shell call for lines that
// turn out to be another event's. A look reads the disk; when an edit had
// already been written but its event was not yet handled (the worker was
// behind, or the look ran between the tool's write and its hook), the look
// credits the edit's lines to its shell call. When the edit is then handled
// it claims its lines in the map, and here the shell call gives the count
// back: lines_generated, its share in the per-file split, the session's
// ledger, and the line ranges the look recorded for it. Those are rewritten
// from current, the file's map now that the edit has its lines: a commit
// attributed by position reads the ranges, and so do blame and the cloud.
// Only for a shell call: no other event is credited by a look.
func takeBackFromLook(db *store.DB, holder store.LineAuthor, path string, hashes []uint64, current store.FileMap) {
	var tool, split sql.NullString
	var generated sql.NullInt64
	if err := db.QueryRow(`SELECT json_extract(payload, '$.tool_name'), lines_generated, file_lines FROM events WHERE id = ?`,
		holder.EventID).Scan(&tool, &generated, &split); err != nil || !shellToolNames[tool.String] || !generated.Valid {
		return
	}
	n := len(hashes)
	var encoded sql.NullString
	if split.Valid {
		var files []fileLines
		if json.Unmarshal([]byte(split.String), &files) == nil {
			for i := range files {
				if files[i].FilePath == path {
					files[i].LinesGenerated = max(files[i].LinesGenerated-n, 0)
				}
			}
			if raw, err := json.Marshal(files); err == nil {
				encoded = sql.NullString{String: string(raw), Valid: true}
			}
		}
	}
	if _, err := db.Exec(`UPDATE events SET lines_generated = ?, file_lines = ?, synced_at = NULL WHERE id = ?`,
		max(int(generated.Int64)-n, 0), encoded, holder.EventID); err != nil {
		logx.Warnf("taking back %d line(s) from shell event %s failed: %v", n, holder.EventID, err)
		return
	}
	if holder.SessionID != "" {
		drop := counts(hashes)
		ledger := db.SessionFileLines(holder.SessionID, path)
		kept := ledger[:0:0]
		for _, h := range ledger {
			if drop[h] > 0 {
				drop[h]--
				continue
			}
			kept = append(kept, h)
		}
		_ = db.SaveSessionFileLines(holder.SessionID, path, kept)
	}
	var still []int
	for i := range current.Hashes {
		if a, ok := current.AuthorOf(i); ok && a.EventID == holder.EventID {
			still = append(still, i+1)
		}
	}
	if err := db.SetEventLineRanges(holder.EventID, path, store.LineRuns(still)); err != nil {
		logx.Warnf("rewriting the line ranges of shell event %s failed: %v", holder.EventID, err)
	}
	logx.Debugf("shell event %s gave back %d line(s) of %s that a later edit wrote", holder.EventID, n, path)
}

// lastLookWasPartial reports whether the last look at a repository could not
// read every changed file.
func lastLookWasPartial(db *store.DB, root string) bool {
	last, looked := db.RepoLook(root)
	return looked && last.Partial
}

// seedMapAuthors gives a snapshot from before the authorship map the authors
// its line ranges name: the edits made to the file since its last commit,
// at the positions they were recorded at. Those are the lines attribution by
// position would credit, so an edit in flight across the upgrade keeps its
// credit when the next look at the file turns its snapshot into a map. root
// and rel say where the file is; except is an event whose own ranges are not
// part of the old state (the one being handled).
func seedMapAuthors(db *store.DB, root, rel string, prev store.FileMap, except string) store.FileMap {
	prev.Owner = make([]int, len(prev.Hashes))
	prev.Commit = make([]int, len(prev.Hashes))
	if root == "" || rel == "" {
		return prev
	}
	for _, r := range db.UncommittedRanges(root, rel) {
		if r.Author.EventID == except || r.Author.SessionID == "" {
			continue
		}
		for n := r.Start; n <= r.End; n++ {
			if n >= 1 && n <= len(prev.Owner) {
				prev.SetAuthor(n-1, r.Author)
			}
		}
	}
	return prev
}

// applyHunks applies a patch to the content it was made against and returns
// the result. ok is false when the patch does not fit: a context or removed
// line is not where the patch says it is.
func applyHunks(original string, hunks []lineranges.PatchHunk) (after string, ok bool) {
	old := lineranges.Lines(original)
	out := make([]string, 0, len(old)+8)
	at := 0    // next line of old to copy
	delta := 0 // lines the hunks so far added, less those they removed
	for _, h := range hunks {
		start := h.NewStart - 1 - delta
		if h.NewStart < 1 {
			start = at
		}
		if start < at || start > len(old) {
			return "", false
		}
		out = append(out, old[at:start]...)
		at = start
		for _, l := range h.Lines {
			marker, text := byte(' '), ""
			if l != "" {
				marker, text = l[0], l[1:]
			}
			switch marker {
			case '+':
				out = append(out, text)
				delta++
			case '\\':
				// "\ No newline at end of file": not a line of either side.
			case '-':
				if at >= len(old) || !sameText(old[at], text) {
					return "", false
				}
				at++
				delta--
			default:
				if at >= len(old) || !sameText(old[at], text) {
					return "", false
				}
				out = append(out, old[at])
				at++
			}
		}
	}
	out = append(out, old[at:]...)
	if len(out) == 0 {
		return "", true
	}
	return strings.Join(out, "\n") + "\n", true
}

func sameText(a, b string) bool {
	return strings.TrimSuffix(a, "\r") == strings.TrimSuffix(b, "\r")
}

// sameLines reports whether two texts hold the same lines, a trailing CR
// aside.
func sameLines(a, b string) bool {
	la, lb := lineranges.Lines(a), lineranges.Lines(b)
	if len(la) != len(lb) {
		return false
	}
	for i := range la {
		if !sameText(la[i], lb[i]) {
			return false
		}
	}
	return true
}

// gitBlobID is the id git gives content as a blob (SHA-1 object format). A
// commit that holds the same blob holds this content byte for byte. In a
// repository that uses another object format the ids never match, and the
// map is lined up with the committed file instead.
func gitBlobID(content string) string {
	h := sha1.New()
	_, _ = h.Write([]byte("blob " + strconv.Itoa(len(content)) + "\x00"))
	_, _ = h.Write([]byte(content))
	return hex.EncodeToString(h.Sum(nil))
}
