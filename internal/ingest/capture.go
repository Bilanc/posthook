package ingest

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"

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
//     heredoc) is diffed against posthook's last copy of that file, or the
//     version at HEAD, whichever is closer. With neither, the file is new
//     and every line counts.
//  4. apply_patch hunks are found by their context lines.
//
// A file outside any git repository, or one git ignores, can never reach a
// commit. It gets no ranges and counts for nothing; the event still records
// lines_generated = 0 so readers know it was measured.

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

	// post is the file as read from disk while handling the event, and
	// hasPost whether that read worked (the file may already be gone).
	post    []byte
	hasPost bool

	// snapshot is the file as this event left it, kept as the baseline for
	// the next whole-file write. nil leaves the stored snapshot alone.
	snapshot []byte

	// lines is the session's remembered lines for this file after this
	// event; set by score.
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
		fc.post, fc.hasPost = content, true
	}
}

// captureToolEdit handles Edit, MultiEdit and Write shaped events, including
// Cursor's afterFileEdit.
func captureToolEdit(db *store.DB, path, toolName string, payload map[string]any) *editCapture {
	input := lineRangeToolInput(payload)
	if input == nil {
		return nil
	}
	fc := newFileCapture(path, toolName)
	if !fc.countable {
		return &editCapture{files: []fileCapture{fc}}
	}
	fc.readPost()
	fc.snapshot = fc.post

	if hunks, ok := structuredPatch(toolResponse(payload)); ok && (len(hunks) > 0 || toolName == "Write") {
		switch {
		case len(hunks) > 0:
			fc.change = lineranges.ChangeFromHunks(hunks)
		case createdFile(toolResponse(payload)):
			fc.change = lineranges.DiffChange("", writtenContent(*input, fc.post))
		default:
			// A Write that left the file as it was: an empty patch against
			// an existing file.
		}
		if toolName == "Write" && input.Content != "" {
			fc.snapshot = []byte(input.Content)
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
		fc.change = overwriteChange(db, &fc, after)
		fc.snapshot = []byte(after)
	case "Edit":
		fc.change, fc.unlocated = lineranges.EditsChange(
			[]lineranges.Edit{{OldString: input.OldString, NewString: input.NewString}}, string(fc.post))
	case "MultiEdit":
		fc.change, fc.unlocated = lineranges.EditsChange(input.Edits, string(fc.post))
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
			fc.snapshot = fc.post
			// With the file gone every hunk is unlocated: the lines still
			// count, they just have no position.
			fc.change, fc.unlocated = lineranges.LocateApplyPatch(file, string(fc.post))
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
func captureShellWrites(db *store.DB, writes []lineranges.ShellWrite, cwd string, workspaceRoots []string) *editCapture {
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
		fc.snapshot = fc.post
		located := lineranges.Extract("ShellWrite", lineranges.ToolInput{Content: w.Content}, string(fc.post))
		switch {
		case len(located.Ranges) == 0:
			// The text is not in the file as the command spelled it (a
			// variable expanded, or the file changed since). Count it and
			// claim no position.
			fc.unlocated = 1
			if w.Append {
				fc.change = lineranges.Change{Added: lineranges.Lines(w.Content)}
			} else {
				fc.change = overwriteChange(db, &fc, w.Content)
				fc.change.Ranges = nil
			}
		case !w.Append && located.Ranges[0].StartLine == 1:
			// The command replaced the file: only what differs from the
			// file's previous content is new.
			fc.change = overwriteChange(db, &fc, w.Content)
		default:
			// Appended: every line of it is new, where it now sits.
			fc.change = lineranges.Change{Ranges: located.Ranges, Added: lineranges.Lines(w.Content)}
		}
		c.files = append(c.files, fc)
	}
	if len(c.files) == 0 {
		return nil
	}
	return c
}

// overwriteChange is the change a whole-file write made, for tools that do
// not report the previous content. The baseline is posthook's last copy of
// the file or the version at HEAD, whichever leaves the smaller change: both
// are states the file was in before this write, and the nearer one is the
// better guess at what the tool actually replaced. A stale snapshot (the
// file was committed and reworked by hand since) loses to HEAD; a file
// rewritten twice before its first commit has only the snapshot. With
// neither, the file is new.
func overwriteChange(db *store.DB, fc *fileCapture, after string) lineranges.Change {
	var bases []string
	if snap, ok := db.FileSnapshot(fc.path); ok {
		bases = append(bases, snap)
	}
	if fc.root != "" && fc.rel.Valid {
		if head, err := gitx.Output(fc.root, "show", "HEAD:"+filepath.ToSlash(fc.rel.String)); err == nil {
			bases = append(bases, string(head))
		}
	}
	if len(bases) == 0 {
		return lineranges.DiffChange("", after)
	}
	best := lineranges.DiffChange(bases[0], after)
	for _, base := range bases[1:] {
		if c := lineranges.DiffChange(base, after); c.LinesAdded() < best.LinesAdded() {
			best = c
		}
	}
	return best
}

// score totals the lines the event generated and works out how many of the
// lines it removed were the session's own earlier work, per file.
func (c *editCapture) score(db *store.DB, sessionID string) {
	// One event can write the same file twice (two redirects in one shell
	// command); carry the remembered lines between them.
	remembered := map[string][]uint64{}
	for i := range c.files {
		fc := &c.files[i]
		if !fc.countable {
			continue
		}
		c.generated += fc.change.LinesAdded()
		if sessionID == "" {
			continue
		}
		lines, seen := remembered[fc.path]
		if !seen {
			lines = db.SessionFileLines(sessionID, fc.path)
		}
		if len(fc.change.Removed) > 0 && len(lines) > 0 {
			drop := map[uint64]int{}
			have := map[uint64]int{}
			for _, h := range lines {
				have[h]++
			}
			for _, removed := range fc.change.Removed {
				h := store.LineHash(removed)
				if have[h] > drop[h] {
					drop[h]++
					c.reworked++
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
		if fc.snapshot != nil {
			if err := db.SaveFileSnapshot(fc.path, fc.snapshot); err != nil {
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
