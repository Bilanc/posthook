package store

import (
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"hash/fnv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bilanc/posthook/internal/lineranges"
)

// The authorship map.
//
// A file_snapshots row is a file as posthook last saw it, one fingerprint per
// line. The map adds who wrote each line: the agent event that added it, or
// nobody. Each time posthook looks at the file again (an agent edit, a
// commit) the old copy is lined up with the new one. A line that is still
// there keeps its author wherever it now sits; a line that is new gets the
// author of whatever caused the look.
//
// That is what lets a commit be attributed by content. At commit the map is
// lined up with the committed file, and an added line counts as AI only if
// it lines up with a line that has an author. A line somebody changed after
// the agent wrote it lines up with nothing and is theirs.
//
// Two copies are lined up on the whitespace-insensitive fingerprint, so a
// line that format-on-save re-indented is still the same line.
//
// Each line also remembers the last commit it was counted in. A commit that
// adds a line again which one of its ancestors was already credited for (a
// revert of a revert) does not count it a second time; a commit that
// replaces the one that counted it (an amend, a rebase, a cherry-pick) does.

// LineAuthor is the agent event that wrote a line.
type LineAuthor struct {
	EventID   string `json:"e"`
	SessionID string `json:"s"`
	Agent     string `json:"a,omitempty"`
	TS        string `json:"t,omitempty"`
	// Held marks a line that is kept for a shell tool call whose look at the
	// working tree is still due (looks.go): the line appeared after the
	// command ran and before posthook looked. Until that look confirms it,
	// the line counts as nobody's.
	Held bool `json:"h,omitempty"`
}

// FileMap is one file's authorship map.
type FileMap struct {
	// Hashes and Norms are the exact and the whitespace-insensitive
	// fingerprint of each line. Norms is nil on a row written before v17.
	Hashes []uint64
	Norms  []uint64
	// Owner[i] is 1 + the index in Authors of the event that wrote line i,
	// or 0 for nobody. Commit[i] is 1 + the index in Commits of the commit
	// line i was last counted in, or 0. Both are nil on a row written before
	// v17: such a file has no authors yet.
	Owner   []int
	Commit  []int
	Authors []LineAuthor
	Commits []string
	// AsOf is the instant the content was true (see FileSnapshot).
	AsOf time.Time
	// Blob is the git blob id of the content when it is known byte for
	// byte, so a commit that holds the same blob needs no line-up at all.
	Blob string

	// ownersAsRead is the stored authorship as FileMap read it, which
	// UpdateFileMapMarks compares the row with before writing.
	ownersAsRead []byte
}

// HasAuthors reports whether the map carries authorship at all. False for a
// row written before v17, whatever its lines.
func (m FileMap) HasAuthors() bool { return m.Owner != nil }

// AuthorOf returns the author of line i (0-based). A line that is only held
// for a look that is still due has none.
func (m FileMap) AuthorOf(i int) (LineAuthor, bool) {
	a, ok := m.authorEntry(i)
	if !ok || a.Held {
		return LineAuthor{}, false
	}
	return a, true
}

// HeldFor returns the event line i (0-based) is held for, if it is.
func (m FileMap) HeldFor(i int) (LineAuthor, bool) {
	a, ok := m.authorEntry(i)
	if !ok || !a.Held {
		return LineAuthor{}, false
	}
	return a, true
}

func (m FileMap) authorEntry(i int) (LineAuthor, bool) {
	if i < 0 || i >= len(m.Owner) || m.Owner[i] <= 0 || m.Owner[i] > len(m.Authors) {
		return LineAuthor{}, false
	}
	return m.Authors[m.Owner[i]-1], true
}

// ClearAuthor leaves line i (0-based) with no author.
func (m *FileMap) ClearAuthor(i int) {
	if i >= 0 && i < len(m.Owner) {
		m.Owner[i] = 0
		if i < len(m.Commit) {
			m.Commit[i] = 0
		}
	}
}

// CommitOf returns the sha of the commit line i (0-based) was last counted
// in, empty when in none.
func (m FileMap) CommitOf(i int) string {
	if i < 0 || i >= len(m.Commit) || m.Commit[i] <= 0 || m.Commit[i] > len(m.Commits) {
		return ""
	}
	return m.Commits[m.Commit[i]-1]
}

// SetAuthor makes author the writer of line i (0-based). A line that is
// written anew has been counted in no commit yet.
func (m *FileMap) SetAuthor(i int, author LineAuthor) {
	if i < 0 || i >= len(m.Owner) {
		return
	}
	at := -1
	for k, a := range m.Authors {
		if a.EventID == author.EventID && a.Held == author.Held {
			at = k
			break
		}
	}
	if at < 0 {
		// Never append in place: Carry shares Authors with the map it came
		// from.
		m.Authors = append(m.Authors[:len(m.Authors):len(m.Authors)], author)
		at = len(m.Authors) - 1
	}
	m.Owner[i] = at + 1
	m.Commit[i] = 0
}

// SetCommit records sha as the commit line i (0-based) was counted in.
func (m *FileMap) SetCommit(i int, sha string) {
	if i < 0 || i >= len(m.Commit) || sha == "" {
		return
	}
	at := -1
	for k, c := range m.Commits {
		if c == sha {
			at = k
			break
		}
	}
	if at < 0 {
		m.Commits = append(m.Commits[:len(m.Commits):len(m.Commits)], sha)
		at = len(m.Commits) - 1
	}
	m.Commit[i] = at + 1
}

// Carry lines the map up with a newer copy of the file, given as its lines,
// and returns the map of that copy: every line the two share keeps its
// author and its commit, and every other line has neither. from[j] is the
// index of the old line that new line j was carried from, or -1.
//
// Lines are paired in order first, the way a diff pairs them. A line with an
// author that the diff counts as removed is then looked for among the new
// lines the diff counts as added: a block that was moved inside the file is
// still the same lines. Lines with no letter or digit in them (a brace, a
// blank line) are not looked for on their own, there are too many alike; they
// are carried when they sit next to a line that was carried with an author
// and a copy of them with an author went missing.
func (m FileMap) Carry(lines []string) (next FileMap, from []int) {
	n := len(lines)
	next = FileMap{
		Hashes:  make([]uint64, n),
		Norms:   make([]uint64, n),
		Owner:   make([]int, n),
		Commit:  make([]int, n),
		Authors: m.Authors,
		Commits: m.Commits,
	}
	from = make([]int, n)
	for j, l := range lines {
		next.Hashes[j] = LineHash(l)
		next.Norms[j] = NormHash(l)
		from[j] = -1
	}
	if len(m.Hashes) == 0 || n == 0 {
		return next, from
	}
	oldKey, newKey := m.Norms, next.Norms
	if len(m.Norms) != len(m.Hashes) {
		// A row from before v17 has no whitespace-insensitive fingerprints.
		oldKey, newKey = m.Hashes, next.Hashes
	}
	match := lineranges.AlignHashes(oldKey, newKey)
	for i, j := range match {
		if j >= 0 {
			from[j] = i
		}
	}
	if !m.HasAuthors() {
		return next, from
	}
	// free marks the old lines that have an author and that the diff counts
	// as removed; waiting lists them by fingerprint, in order.
	free := make([]bool, len(oldKey))
	waiting := map[uint64][]int{}
	for i, j := range match {
		if j < 0 && i < len(m.Owner) && m.Owner[i] > 0 {
			free[i] = true
			waiting[oldKey[i]] = append(waiting[oldKey[i]], i)
		}
	}
	if len(waiting) > 0 {
		for j := range lines {
			if from[j] >= 0 || Trivial(lines[j]) {
				continue
			}
			queue := waiting[newKey[j]]
			for len(queue) > 0 && !free[queue[0]] {
				queue = queue[1:]
			}
			if len(queue) == 0 {
				waiting[newKey[j]] = queue
				continue
			}
			i := queue[0]
			waiting[newKey[j]] = queue[1:]
			from[j], free[i] = i, false
			// The lines around it that moved with it, a brace or a blank
			// line among them, are the same block.
			for nj, oi := j+1, i+1; nj < n && oi < len(oldKey) && from[nj] < 0 && free[oi] && newKey[nj] == oldKey[oi]; nj, oi = nj+1, oi+1 {
				from[nj], free[oi] = oi, false
			}
			for nj, oi := j-1, i-1; nj >= 0 && oi >= 0 && from[nj] < 0 && free[oi] && newKey[nj] == oldKey[oi]; nj, oi = nj-1, oi-1 {
				from[nj], free[oi] = oi, false
			}
		}
		// A brace or a blank line the diff paired with another copy of
		// itself leaves its own copy unpaired at the edge of a moved block.
		// Next to a carried line that has an author, such a line takes an
		// authored copy that went missing, if there is one.
		authored := func(j int) bool {
			return j >= 0 && j < n && from[j] >= 0 && from[j] < len(m.Owner) && m.Owner[from[j]] > 0
		}
		adopt := func(j int) {
			queue := waiting[newKey[j]]
			for len(queue) > 0 && !free[queue[0]] {
				queue = queue[1:]
			}
			if len(queue) > 0 {
				from[j], free[queue[0]] = queue[0], false
				queue = queue[1:]
			}
			waiting[newKey[j]] = queue
		}
		for j := 0; j < n; j++ {
			if from[j] < 0 && Trivial(lines[j]) && authored(j-1) {
				adopt(j)
			}
		}
		for j := n - 1; j >= 0; j-- {
			if from[j] < 0 && Trivial(lines[j]) && authored(j+1) {
				adopt(j)
			}
		}
	}
	for j, i := range from {
		if i >= 0 && i < len(m.Owner) {
			next.Owner[j] = m.Owner[i]
			if i < len(m.Commit) {
				next.Commit[j] = m.Commit[i]
			}
		}
	}
	return next, from
}

// NormHash identifies a line whatever its whitespace: every space, tab and
// line-ending character is left out before hashing. Two lines that differ
// only in indentation, alignment or spacing read the same.
func NormHash(line string) uint64 {
	h := fnv.New64a()
	var buf [utf8.UTFMax]byte
	for _, r := range line {
		if unicode.IsSpace(r) {
			continue
		}
		n := utf8.EncodeRune(buf[:], r)
		_, _ = h.Write(buf[:n])
	}
	return h.Sum64()
}

// Trivial reports whether a line has no letter or digit in it. Such lines (a
// blank line, a lone brace) are too alike to say anything about who wrote
// them on their own.
func Trivial(line string) bool {
	return strings.IndexFunc(line, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) < 0
}

// mapMeta is the JSON in file_snapshots.owners.
type mapMeta struct {
	Authors []LineAuthor `json:"authors,omitempty"`
	Commits []string     `json:"commits,omitempty"`
}

// FileMap returns the authorship map of path.
func (db *DB) FileMap(path string) (FileMap, bool) {
	var hashes, norms, owners []byte
	var meta, blob sql.NullString
	var at string
	if err := db.QueryRow(`
		SELECT line_hashes, updated_at, line_norms, line_owners, owners, blob_sha
		FROM file_snapshots WHERE file_path = ?`, path).Scan(&hashes, &at, &norms, &owners, &meta, &blob); err != nil {
		return FileMap{}, false
	}
	m := FileMap{Hashes: decodeHashes(hashes), Blob: blob.String}
	m.AsOf, _ = time.Parse(time.RFC3339Nano, at)
	if len(owners) > 0 || len(norms) > 0 {
		if decoded := decodeHashes(norms); len(decoded) == len(m.Hashes) {
			m.Norms = decoded
		}
	}
	if len(owners) > 0 {
		m.ownersAsRead = owners
		m.Owner, m.Commit = decodeOwners(owners, len(m.Hashes))
		var parsed mapMeta
		if meta.Valid && json.Unmarshal([]byte(meta.String), &parsed) == nil {
			m.Authors, m.Commits = parsed.Authors, parsed.Commits
		}
	}
	return m, true
}

// SaveFileMap records m as the state of path.
func (db *DB) SaveFileMap(path string, m FileMap) error {
	norms, owners, meta := m.encode()
	_, err := db.Exec(`
		INSERT INTO file_snapshots (file_path, line_hashes, updated_at, line_norms, line_owners, owners, blob_sha)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(file_path) DO UPDATE SET
		  line_hashes = excluded.line_hashes, updated_at = excluded.updated_at,
		  line_norms = excluded.line_norms, line_owners = excluded.line_owners,
		  owners = excluded.owners, blob_sha = excluded.blob_sha`,
		path, encodeHashes(m.Hashes), m.AsOf.UTC().Format(time.RFC3339Nano), norms, owners, meta, nullableOf(m.Blob))
	return err
}

// UpdateFileMapMarks writes the authors and commit marks of m over the row
// for path, provided the row is still as m was read: the same content and
// the same authorship. The commit path runs in the git shadow while the
// worker may be handling an edit of the same file, or settling who wrote
// lines whose text did not change; if the worker got there first its row is
// left alone and false is returned.
func (db *DB) UpdateFileMapMarks(path string, m FileMap) (bool, error) {
	_, owners, meta := m.encode()
	res, err := db.Exec(`
		UPDATE file_snapshots SET line_owners = ?, owners = ?
		WHERE file_path = ? AND updated_at = ? AND line_hashes = ? AND line_owners = ?`,
		owners, meta, path, m.AsOf.UTC().Format(time.RFC3339Nano), encodeHashes(m.Hashes), m.ownersAsRead)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ownersLayout is the first byte of file_snapshots.line_owners. It also keeps
// the blob of an empty file from being empty, which reads back as NULL.
const ownersLayout = 1

// encode renders the v17 columns. Authors and commits no line refers to any
// more are left out.
func (m FileMap) encode() (norms, owners []byte, meta sql.NullString) {
	if m.Norms != nil && len(m.Norms) == len(m.Hashes) {
		norms = encodeHashes(m.Norms)
	}
	if !m.HasAuthors() {
		return norms, nil, sql.NullString{}
	}
	authorAt := make([]int, len(m.Authors)+1)
	commitAt := make([]int, len(m.Commits)+1)
	var kept mapMeta
	owners = make([]byte, 1, 1+2*len(m.Owner))
	owners[0] = ownersLayout
	for i := range m.Owner {
		owner, commit := m.Owner[i], 0
		if i < len(m.Commit) {
			commit = m.Commit[i]
		}
		if owner <= 0 || owner > len(m.Authors) {
			owner, commit = 0, 0
		} else {
			if authorAt[owner] == 0 {
				kept.Authors = append(kept.Authors, m.Authors[owner-1])
				authorAt[owner] = len(kept.Authors)
			}
			owner = authorAt[owner]
			if commit <= 0 || commit > len(m.Commits) {
				commit = 0
			} else {
				if commitAt[commit] == 0 {
					kept.Commits = append(kept.Commits, m.Commits[commit-1])
					commitAt[commit] = len(kept.Commits)
				}
				commit = commitAt[commit]
			}
		}
		owners = binary.AppendUvarint(owners, uint64(owner))
		owners = binary.AppendUvarint(owners, uint64(commit))
	}
	encoded, err := json.Marshal(kept)
	if err != nil {
		encoded = []byte("{}")
	}
	return norms, owners, sql.NullString{String: string(encoded), Valid: true}
}

// decodeOwners reads the per-line (author, commit) pairs. A blob that does
// not hold exactly lines pairs is damaged and reads as no author on any line.
func decodeOwners(blob []byte, lines int) (owner, commit []int) {
	owner, commit = make([]int, lines), make([]int, lines)
	if len(blob) == 0 || blob[0] != ownersLayout {
		return owner, commit
	}
	rest := blob[1:]
	for i := 0; i < lines; i++ {
		a, n := binary.Uvarint(rest)
		if n <= 0 {
			return make([]int, lines), make([]int, lines)
		}
		rest = rest[n:]
		c, n := binary.Uvarint(rest)
		if n <= 0 {
			return make([]int, lines), make([]int, lines)
		}
		rest = rest[n:]
		owner[i], commit[i] = int(a), int(c)
	}
	return owner, commit
}

// ContentAttributionSince is when this database began to match commits by
// content: when schema v17 was applied, or when the database was created if
// that was later. A commit made before it is attributed by line position, as
// every commit was then.
func (db *DB) ContentAttributionSince() (time.Time, bool) {
	var v sql.NullString
	if err := db.QueryRow(`SELECT MIN(applied_at) FROM schema_version WHERE version >= ?`, contentAttributionVersion).Scan(&v); err != nil || !v.Valid {
		return time.Time{}, false
	}
	t, err := time.Parse("2006-01-02 15:04:05", v.String)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}
