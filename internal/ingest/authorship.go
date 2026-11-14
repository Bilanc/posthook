package ingest

import (
	"crypto/sha1"
	"encoding/hex"
	"strconv"
	"strings"

	"github.com/bilanc/posthook/internal/lineranges"
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
// author of each line. It replaces the plain snapshot save.
func (fc *fileCapture) updateAuthorship(db *store.DB, author store.LineAuthor) error {
	lines := lineranges.Lines(fc.snapshotText)
	prev, found := db.FileMap(fc.path)
	if found && !prev.HasAuthors() {
		prev = fc.seedAuthors(db, prev, author.EventID)
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
		for _, r := range fc.authored {
			for n := r.StartLine; n <= r.EndLine; n++ {
				if n >= 1 && n <= len(match) && match[n-1] >= 0 {
					prev.SetAuthor(match[n-1], author)
				}
			}
		}
		return db.SaveFileMap(fc.path, prev)
	}

	next, _ := prev.Carry(lines)
	next.AsOf = fc.snapshotAsOf
	next.Blob = gitBlobID(fc.snapshotText)
	if author.SessionID != "" {
		for _, r := range fc.authored {
			for n := r.StartLine; n <= r.EndLine; n++ {
				next.SetAuthor(n-1, author)
			}
		}
	}
	return db.SaveFileMap(fc.path, next)
}

// seedAuthors gives a snapshot from before the authorship map the authors
// its line ranges name: the edits made to the file since its last commit,
// at the positions they were recorded at. Those are the lines attribution by
// position would credit, so an edit in flight across the upgrade keeps its
// credit when the next edit turns the file's snapshot into a map. except is
// the event being handled, whose own ranges are not part of the old state.
func (fc *fileCapture) seedAuthors(db *store.DB, prev store.FileMap, except string) store.FileMap {
	prev.Owner = make([]int, len(prev.Hashes))
	prev.Commit = make([]int, len(prev.Hashes))
	if fc.root == "" || !fc.rel.Valid {
		return prev
	}
	for _, r := range db.UncommittedRanges(fc.root, fc.rel.String) {
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
