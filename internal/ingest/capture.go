package ingest

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bilanc/posthook/internal/gitx"
	"github.com/bilanc/posthook/internal/lineranges"
	"github.com/bilanc/posthook/internal/logx"
	"github.com/bilanc/posthook/internal/store"
)

// Exact edit capture.
//
// For every agent event that writes files, ingest works out exactly which
// lines the event added or changed and records three things from that one
// answer, so they cannot drift apart:
//
//   - event_line_ranges: where those lines sit in the file, for commit
//     attribution and `posthook blame`;
//   - events.lines_generated: how many there are;
//   - events.lines_reworked: how many lines the event removed that the same
//     session had written to that file earlier.
//
// Where the answer comes from, most exact first:
//
//  1. Claude Code reports each Edit, MultiEdit and Write as a patch against
//     the file it read (tool_response.structuredPatch). The patch is used as
//     is. It is only in the hook payload: TrimPayload drops it before the
//     event is stored, so it has to be read here.
//  2. An old/new string pair from any other agent is diffed line by line at
//     the place the new string sits in the file, so the unchanged lines the
//     agent repeated around its change are not counted.
//  3. A tool that hands over a whole file (Write from other agents, a shell
//     heredoc) is diffed against posthook's last snapshot of that file, or
//     the version at HEAD, whichever is closer. With neither, the file is
//     new and every line counts.
//  4. apply_patch hunks are found by their context lines.
//
// A file outside any git repository, or one git ignores, can never reach a
// commit. It gets no ranges and counts for nothing; the event still records
// lines_generated = 0 so readers know it was measured.
//
// Events are handled by a background worker, after the fact and in order,
// so the file on disk may already hold later edits than the event in hand.
// Two rules follow. What an event itself carries (a patch, a whole file's
// content) is trusted over the disk. And a snapshot read from disk is dated
// with the time of the read: an event that fired before that read cannot use
// it as its "before", because the read may have seen that event's own write.
//
// Snapshots and the per-session ledger are line fingerprints
// (store.LineHash), never file content.

// fileCapture is what one event did to one file.
type fileCapture struct {
	path string         // canonical absolute path
	root string         // repository the file is in, "" when it is in none
	rel  sql.NullString // path inside root
	tool string         // for the debug log

	// countable: inside a repository and not ignored by git.
	countable bool

	change    lineranges.Change
	unlocated int

	// removed is the fingerprint of every line the edit removed. score
	// matches these against the session's ledger.
	removed []uint64

	// before is the file before the edit, one fingerprint per line, when
	// that is known exactly: Claude Code's originalFile, or the baseline a
	// whole-file write was diffed against.
	before    []uint64
	hasBefore bool

	// post is the file as read from disk while handling the event, readAt
	// when, and hasPost whether the read worked (the file may be gone).
	post    []byte
	hasPost bool
	readAt  time.Time

	// snapshot is the file as this event left it, kept as the baseline for
	// the next whole-file write, and snapshotAsOf the instant it was true.
	// snapshotBytes is the size of the content it was taken from.
	snapshot      []uint64
	hasSnapshot   bool
	snapshotAsOf  time.Time
	snapshotBytes int

	// generated and reworked are this file's share of the event's counts,
	// and lines the session's ledger for the file after the event. All set
	// by score.
	generated int
	reworked  int
	lines     []uint64
	saveLines bool
}

// editCapture is everything one event wrote.
type editCapture struct {
	files     []fileCapture
	generated int
	reworked  int
}

func newFileCapture(path, tool string) fileCapture {
	fc := fileCapture{path: path, tool: tool}
	fc.root = gitx.FindRepoRoot(path)
	if fc.root == "" {
		return fc
	}
	rel := gitx.RelPathInRepo(fc.root, path)
	if rel == "" {
		return fc
	}
	fc.rel = sql.NullString{String: rel, Valid: true}
	// check-ignore exits 0 only when a rule matches an untracked path; a
	// tracked file is never reported, whatever .gitignore says.
	fc.countable = gitx.Exec(fc.root, "check-ignore", "-q", "--", rel) != nil
	return fc
}

func (fc *fileCapture) readPost() {
	if content, err := os.ReadFile(fc.path); err == nil {
		fc.post, fc.hasPost, fc.readAt = content, true, time.Now()
	}
}

// setChange records a change whose removed lines are text.
func (fc *fileCapture) setChange(c lineranges.Change, unlocated int) {
	fc.change, fc.unlocated = c, unlocated
	fc.removed = hashAll(c.Removed)
}

// snapshotOf records content as the state this event left the file in, true
// from asOf.
func (fc *fileCapture) snapshotOf(content string, asOf time.Time) {
	fc.snapshot, fc.hasSnapshot = hashLines(content), true
	fc.snapshotAsOf, fc.snapshotBytes = asOf, len(content)
}

// snapshotFromDisk records the file as read while handling the event. It is
// only known to be true from the moment of the read: by then the file may
// hold edits later than this event.
func (fc *fileCapture) snapshotFromDisk() {
	if fc.hasPost {
		fc.snapshotOf(string(fc.post), fc.readAt)
	}
}

// captureToolEdit handles Edit, MultiEdit and Write shaped events, including
// Cursor's afterFileEdit. firedAt is when the hook fired.
func captureToolEdit(db *store.DB, path, toolName string, payload map[string]any, firedAt time.Time) *editCapture {
	input := lineRangeToolInput(payload)
	if input == nil {
		return nil
	}
	fc := newFileCapture(path, toolName)
	if !fc.countable {
		return &editCapture{files: []fileCapture{fc}}
	}
	fc.readPost()

	resp := toolResponse(payload)
	if hunks, ok := structuredPatch(resp); ok && (len(hunks) > 0 || toolName == "Write") {
		switch {
		case len(hunks) > 0:
			fc.setChange(lineranges.ChangeFromHunks(hunks), 0)
		case createdFile(resp):
			fc.setChange(lineranges.DiffChange("", writtenContent(*input, fc.post)), 0)
		default:
			// A Write that left the file as it was: an empty patch against
			// an existing file.
		}
		if original, ok := resp["originalFile"].(string); ok {
			fc.before, fc.hasBefore = hashLines(original), true
		} else if createdFile(resp) {
			fc.before, fc.hasBefore = nil, true
		}
		if toolName == "Write" && input.Content != "" {
			fc.snapshotOf(input.Content, firedAt)
		} else {
			fc.snapshotFromDisk()
		}
		return &editCapture{files: []fileCapture{fc}}
	}

	switch toolName {
	case "Write":
		after := writtenContent(*input, fc.post)
		if after == "" && !fc.hasPost {
			logx.Debugf("line-range: file gone, skipping (%s)", path)
			return nil
		}
		fc.change = overwriteChange(db, &fc, after, firedAt)
		if input.Content != "" {
			fc.snapshotOf(after, firedAt)
			if fc.hasPost {
				// The ranges are positions in the content the tool wrote;
				// put them where those lines are in the file now.
				fc.change, fc.unlocated = placeInFile(fc.change, after, string(fc.post))
			}
		} else {
			fc.snapshotFromDisk()
		}
	case "Edit":
		fc.setChange(lineranges.EditsChange(
			[]lineranges.Edit{{OldString: input.OldString, NewString: input.NewString}}, string(fc.post)))
		fc.snapshotFromDisk()
	case "MultiEdit":
		fc.setChange(lineranges.EditsChange(input.Edits, string(fc.post)))
		fc.snapshotFromDisk()
	default:
		return nil
	}
	return &editCapture{files: []fileCapture{fc}}
}

// captureApplyPatch handles Codex and Copilot apply_patch events.
func captureApplyPatch(files []lineranges.ApplyPatchFile, cwd string, workspaceRoots []string) *editCapture {
	c := &editCapture{}
	for _, file := range lineranges.MergeApplyPatchFiles(files) {
		if len(file.Hunks) == 0 {
			continue
		}
		path := gitx.Canonicalize(resolveEventFilePath(file.FilePath, cwd, workspaceRoots))
		fc := newFileCapture(path, "apply_patch")
		if fc.countable {
			fc.readPost()
			// With the file gone every hunk is unlocated: the lines still
			// count, they just have no position.
			fc.setChange(lineranges.LocateApplyPatch(file, string(fc.post)))
			fc.snapshotFromDisk()
		}
		c.files = append(c.files, fc)
	}
	if len(c.files) == 0 {
		return nil
	}
	return c
}

// captureShellWrites handles shell commands whose written text is in the
// command itself (heredoc, echo, printf). A write whose content comes from a
// program is recorded as a touched file by the caller and attributes nothing.
func captureShellWrites(db *store.DB, writes []lineranges.ShellWrite, cwd string, workspaceRoots []string, firedAt time.Time) *editCapture {
	c := &editCapture{}
	for _, w := range writes {
		if !w.Known || w.Content == "" {
			logx.Debugf("line-range: shell wrote %s with unknown content, not attributing lines", w.FilePath)
			continue
		}
		path := gitx.Canonicalize(resolveEventFilePath(w.FilePath, cwd, workspaceRoots))
		fc := newFileCapture(path, "shell")
		if !fc.countable {
			c.files = append(c.files, fc)
			continue
		}
		fc.readPost()
		if !fc.hasPost {
			logx.Debugf("line-range: file gone, skipping (%s)", path)
			continue
		}
		if w.Append {
			located := lineranges.Extract("ShellWrite", lineranges.ToolInput{Content: w.Content}, string(fc.post))
			// Appended: every line of it is new, where it now sits. If the
			// text is no longer in the file it still counts, without a
			// position.
			added := lineranges.Change{Ranges: located.Ranges, Added: lineranges.Lines(w.Content)}
			unlocated := 0
			if len(located.Ranges) == 0 {
				unlocated = 1
			}
			fc.setChange(added, unlocated)
			fc.snapshotFromDisk()
			c.files = append(c.files, fc)
			continue
		}

		// The command replaced the file with text it spelled out. Like a
		// Write, the event says what the file became, so that text is the
		// state it left behind and only what differs from the file's
		// previous state is new. The snapshot is the command's text, never
		// the disk: with two rewrites queued, the disk already holds the
		// second while the first is handled, and saving it would make the
		// second look like it changed nothing. It is saved even when none
		// of the text is in the file any more, which is exactly that case.
		fc.change = overwriteChange(db, &fc, w.Content, firedAt)
		fc.snapshotOf(w.Content, firedAt)
		if post := string(fc.post); strings.Contains(post, w.Content) || mostlyPresent(w.Content, post) {
			fc.change, fc.unlocated = placeInFile(fc.change, w.Content, post)
		} else {
			// Hardly any of it is in the file now: the command may not have
			// run the redirect, or the file has been rewritten since. Count
			// it and claim no position.
			fc.change.Ranges = nil
			fc.unlocated = 1
		}
		c.files = append(c.files, fc)
	}
	if len(c.files) == 0 {
		return nil
	}
	return c
}

// placeInFile puts a whole-file write's ranges where its added lines sit in
// the file as read. The ranges come from diffing the written content, so
// they are line numbers in that content; the worker reads the file later,
// when other edits may have moved or changed those lines, and every other
// source places its ranges in the file as read.
//
// If every added line still reads the same at its recorded position, the
// ranges stand and nothing is searched for: a copy of the text elsewhere in
// the file is not the write. Otherwise the written content is aligned with
// the file line by line, and each added line moves to the line it is kept
// as. An added line that is no longer in the file gets no range and still
// counts; unplaced reports whether there was one. When the two differ by
// more than the diff aligns, the recorded positions are kept.
func placeInFile(c lineranges.Change, written, file string) (placed lineranges.Change, unplaced int) {
	if addedLinesInPlace(c, file) {
		return c, 0
	}
	match, ok := lineranges.AlignLines(lineranges.Lines(written), lineranges.Lines(file))
	if !ok {
		return c, 0
	}
	var lines []int
	for _, r := range c.Ranges {
		for n := r.StartLine; n <= r.EndLine; n++ {
			if n <= len(match) && match[n-1] >= 0 {
				lines = append(lines, match[n-1]+1)
			} else {
				unplaced = 1
			}
		}
	}
	c.Ranges = lineranges.RangesOf(lines)
	return c, unplaced
}

// addedLinesInPlace reports whether every line the change added still reads
// the same at the position its ranges give it in file.
func addedLinesInPlace(c lineranges.Change, file string) bool {
	lines := lineranges.Lines(file)
	next := 0
	for _, r := range c.Ranges {
		for n := r.StartLine; n <= r.EndLine; n++ {
			if n > len(lines) || next >= len(c.Added) ||
				strings.TrimSuffix(lines[n-1], "\r") != strings.TrimSuffix(c.Added[next], "\r") {
				return false
			}
			next++
		}
	}
	return true
}

// mostlyPresent reports whether at least half the lines of written are still
// in file, in order. That is the evidence that a whole-file write happened
// when the file has since been edited and no longer holds the text verbatim.
func mostlyPresent(written, file string) bool {
	total := len(lineranges.Lines(written))
	if total == 0 {
		return false
	}
	gone := len(lineranges.DiffChange(written, file).Removed)
	return (total-gone)*2 >= total
}

// overwriteChange is the change a whole-file write made, for tools that do
// not report the previous content. The baseline is posthook's last snapshot
// of the file or the version at HEAD, whichever leaves the smaller change:
// both are states the file was in before this write, and the nearer one is
// the better guess at what the tool actually replaced. A stale snapshot (the
// file was committed and reworked by hand since) loses to HEAD; a file
// rewritten twice before its first commit has only the snapshot. With
// neither, the file is new.
//
// A snapshot dated after the event fired is not a "before" at all (see the
// note at the top of this file) and is skipped.
//
// The snapshot is line fingerprints, so the diff runs on fingerprints. The
// ranges come out the same; the added lines' text is read back from after,
// and the removed lines stay fingerprints, which is all score needs. It
// sets fc.removed and fc.before.
func overwriteChange(db *store.DB, fc *fileCapture, after string, firedAt time.Time) lineranges.Change {
	afterLines := lineranges.Lines(after)
	afterTokens := tokens(hashAll(afterLines))

	var bases [][]uint64
	if snap, asOf, ok := db.FileSnapshot(fc.path); ok && !asOf.After(firedAt) {
		bases = append(bases, snap)
	}
	if fc.root != "" && fc.rel.Valid {
		if head, err := gitx.Output(fc.root, "show", "HEAD:"+filepath.ToSlash(fc.rel.String)); err == nil {
			bases = append(bases, hashLines(string(head)))
		}
	}
	if len(bases) == 0 {
		bases = append(bases, nil) // a new file: everything is added
	}

	var best lineranges.Change
	var bestBase []uint64
	for i, base := range bases {
		if c := lineranges.DiffLineSlices(tokens(base), afterTokens); i == 0 || c.LinesAdded() < best.LinesAdded() {
			best, bestBase = c, base
		}
	}

	fc.removed = fromTokens(best.Removed)
	fc.before, fc.hasBefore = bestBase, true
	best.Removed = nil
	best.Added = best.Added[:0:0]
	for _, r := range best.Ranges {
		best.Added = append(best.Added, afterLines[r.StartLine-1:r.EndLine]...)
	}
	return best
}

// copiesBefore counts, per fingerprint, the lines the file held before the
// edit. Exact when the event carried the previous content or was diffed
// against a baseline; otherwise worked back from the file as read (what it
// holds now, less what the edit added, plus what it removed). known is false
// when the file could not be read either.
func (fc *fileCapture) copiesBefore() (pre map[uint64]int, known bool) {
	if fc.hasBefore {
		return counts(fc.before), true
	}
	if !fc.hasPost {
		return nil, false
	}
	pre = counts(hashLines(string(fc.post)))
	for _, added := range fc.change.Added {
		pre[store.LineHash(added)]--
	}
	for _, h := range fc.removed {
		pre[h]++
	}
	return pre, true
}

// score totals the lines the event generated and works out how many of the
// lines it removed were the session's own earlier work, per file.
//
// The ledger knows which lines a session wrote, not where they are, so a
// removed line that reads the same as one of the session's (a blank line, a
// closing brace) may be somebody else's copy. A removal is counted as rework
// only when the file's other copies of that line cannot explain it: with k
// copies removed, of which the file held `before` and the session wrote
// `own`, rework is what is left of k after the before-own copies that were
// not the session's. When ownership is ambiguous nothing is subtracted, so
// generated-minus-reworked never falls below what the session has standing.
func (c *editCapture) score(db *store.DB, sessionID string) {
	// One event can write the same file twice (two redirects in one shell
	// command); carry the ledger between them.
	remembered := map[string][]uint64{}
	for i := range c.files {
		fc := &c.files[i]
		if !fc.countable {
			continue
		}
		fc.generated = fc.change.LinesAdded()
		c.generated += fc.generated
		if sessionID == "" {
			continue
		}
		lines, seen := remembered[fc.path]
		if !seen {
			lines = db.SessionFileLines(sessionID, fc.path)
		}
		if len(fc.removed) > 0 && len(lines) > 0 {
			own := counts(lines)
			before, known := fc.copiesBefore()
			drop := map[uint64]int{}
			for h, k := range counts(fc.removed) {
				rework := min(own[h], k)
				if known {
					// At least the copies this edit removed were there.
					others := max(max(before[h], k)-own[h], 0)
					rework = min(own[h], max(k-others, 0))
				}
				if rework > 0 {
					drop[h] = rework
					fc.reworked += rework
					c.reworked += rework
				}
			}
			kept := lines[:0:0]
			for _, h := range lines {
				if drop[h] > 0 {
					drop[h]--
					continue
				}
				kept = append(kept, h)
			}
			lines = kept
		}
		for _, added := range fc.change.Added {
			lines = append(lines, store.LineHash(added))
		}
		remembered[fc.path] = lines
		fc.lines, fc.saveLines = lines, true
	}
}

// store writes the ranges and the working state for an event already
// inserted as eventID. Fail-soft: the caller logs and carries on.
func (c *editCapture) store(db *store.DB, eventID, sessionID string) error {
	for i := range c.files {
		fc := &c.files[i]
		if !fc.countable {
			logx.Debugf("line-range: %s is outside a repository or ignored by git, not attributing lines", fc.path)
			continue
		}
		out := lineranges.Extracted{Ranges: fc.change.Ranges, Unlocated: fc.unlocated}
		var content []byte
		if fc.hasPost {
			content = fc.post
		}
		if err := insertLineRanges(db, eventID, fc.path, fc.rel, content, fc.tool, out); err != nil {
			return err
		}
		if fc.hasSnapshot {
			var err error
			if fc.snapshotBytes > store.MaxSnapshotBytes {
				// Too large to keep; forget any older snapshot too, so a
				// later write is not diffed against a stale one.
				err = db.DropFileSnapshot(fc.path)
			} else {
				err = db.SaveFileSnapshot(fc.path, fc.snapshot, fc.snapshotAsOf)
			}
			if err != nil {
				return err
			}
		}
		if fc.saveLines && sessionID != "" {
			if err := db.SaveSessionFileLines(sessionID, fc.path, fc.lines); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *editCapture) generatedArg() sql.NullInt64 {
	if c == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(c.generated), Valid: true}
}

func (c *editCapture) reworkedArg() sql.NullInt64 {
	if c == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(c.reworked), Valid: true}
}

// fileLines is one file's share of an event that wrote several, as stored
// in events.file_lines.
type fileLines struct {
	RelFilePath    *string `json:"rel_file_path"`
	FilePath       string  `json:"file_path"`
	LinesGenerated int     `json:"lines_generated"`
	LinesReworked  int     `json:"lines_reworked"`
}

// fileLinesArg is events.file_lines: a JSON array with one element per file
// the event's counts belong to, in the order it wrote them, or NULL when
// that is exactly the one file the event row names (eventPath).
//
// lines_generated and lines_reworked on the event are one number each, and
// its row names a single path. That is enough for an Edit or a Write. It is
// not for a patch across files, where a reader could not say which file was
// reworked, nor for a shell command whose row names its first target while
// the lines posthook could measure went to another
// (`sed -i s/a/b/ a.go; echo x > b.go`). A file that counts for nothing
// (outside the repo, ignored) is listed with zeros; the elements add up to
// the event's two counts. A reader can therefore rely on this: no split
// means the event's own file has all of it.
func (c *editCapture) fileLinesArg(eventPath string) sql.NullString {
	if c == nil {
		return sql.NullString{}
	}
	var files []fileLines
	index := map[string]int{}
	for i := range c.files {
		fc := &c.files[i]
		at, seen := index[fc.path]
		if !seen {
			at = len(files)
			index[fc.path] = at
			entry := fileLines{FilePath: fc.path}
			if fc.rel.Valid {
				rel := fc.rel.String
				entry.RelFilePath = &rel
			}
			files = append(files, entry)
		}
		files[at].LinesGenerated += fc.generated
		files[at].LinesReworked += fc.reworked
	}
	if len(files) == 0 || (len(files) == 1 && files[0].FilePath == eventPath) {
		return sql.NullString{}
	}
	encoded, err := json.Marshal(files)
	if err != nil {
		return sql.NullString{}
	}
	return sql.NullString{String: string(encoded), Valid: true}
}

// hashLines fingerprints each line of text.
func hashLines(text string) []uint64 { return hashAll(lineranges.Lines(text)) }

func hashAll(lines []string) []uint64 {
	if len(lines) == 0 {
		return nil
	}
	out := make([]uint64, len(lines))
	for i, l := range lines {
		out[i] = store.LineHash(l)
	}
	return out
}

func counts(hashes []uint64) map[uint64]int {
	out := make(map[uint64]int, len(hashes))
	for _, h := range hashes {
		out[h]++
	}
	return out
}

// tokens spells fingerprints as text so the line diff can compare them.
func tokens(hashes []uint64) []string {
	out := make([]string, len(hashes))
	for i, h := range hashes {
		out[i] = strconv.FormatUint(h, 16)
	}
	return out
}

func fromTokens(toks []string) []uint64 {
	if len(toks) == 0 {
		return nil
	}
	out := make([]uint64, 0, len(toks))
	for _, t := range toks {
		if h, err := strconv.ParseUint(t, 16, 64); err == nil {
			out = append(out, h)
		}
	}
	return out
}

// writtenContent is the file content a Write-shaped event wrote: the content
// it carries, or the file on disk for the shapes that carry none.
func writtenContent(in lineranges.ToolInput, post []byte) string {
	if in.Content != "" {
		return in.Content
	}
	return string(post)
}

// toolResponse returns the hook payload's tool_response object. Some agents
// deliver it as a JSON string.
func toolResponse(payload map[string]any) map[string]any {
	switch v := payload["tool_response"].(type) {
	case map[string]any:
		return v
	case string:
		var m map[string]any
		if json.Unmarshal([]byte(v), &m) == nil {
			return m
		}
	}
	return nil
}

// structuredPatch reads tool_response.structuredPatch, the patch Claude Code
// computes for every file edit: a list of hunks, each {newStart, lines} with
// lines carrying a ' ', '+' or '-' marker. ok is false when the response has
// no patch in that shape; an empty patch is ok with no hunks.
func structuredPatch(resp map[string]any) ([]lineranges.PatchHunk, bool) {
	raw, ok := resp["structuredPatch"].([]any)
	if !ok {
		return nil, false
	}
	hunks := make([]lineranges.PatchHunk, 0, len(raw))
	for _, h := range raw {
		m, ok := h.(map[string]any)
		if !ok {
			return nil, false
		}
		start, ok := m["newStart"].(float64)
		if !ok {
			return nil, false
		}
		rawLines, ok := m["lines"].([]any)
		if !ok {
			return nil, false
		}
		lines := make([]string, 0, len(rawLines))
		for _, l := range rawLines {
			s, ok := l.(string)
			if !ok {
				return nil, false
			}
			lines = append(lines, s)
		}
		hunks = append(hunks, lineranges.PatchHunk{NewStart: int(start), Lines: lines})
	}
	return hunks, true
}

// createdFile reports whether a Write's response says the file did not exist
// before: type "create", or a null originalFile.
func createdFile(resp map[string]any) bool {
	if t, _ := resp["type"].(string); t == "create" {
		return true
	}
	original, present := resp["originalFile"]
	return present && original == nil
}
