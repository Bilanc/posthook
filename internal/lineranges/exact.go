package lineranges

import "strings"

// Exact change extraction.
//
// Extract (lineranges.go) answers "where does the text the tool wrote sit in
// the file now". As a count of what the agent wrote, that over-counts: a
// Write is the whole file even when it changed two lines, and an Edit's
// new_string usually repeats the unchanged lines around the change. The
// functions here answer "which lines did the edit add or change", from a
// line diff, and report the lines the edit removed, so a caller can tell an
// agent rewriting its own earlier work from new output.

// PatchHunk is one hunk of a patch against a file. NewStart is the 1-based
// line in the post-edit file where the hunk begins. Each entry of Lines is a
// one-character marker (' ' context, '+' added, '-' removed) followed by the
// line text. Claude Code reports every Edit, MultiEdit and overwriting Write
// in this shape, as tool_response.structuredPatch.
type PatchHunk struct {
	NewStart int
	Lines    []string
}

// Change is exactly what one edit did to one file: the text of the lines it
// added or changed, the text of the lines it removed or replaced, and where
// the added lines sit in the post-edit file.
//
// Ranges cover the same lines as Added whenever the edit could be placed in
// the file. An edit whose text is no longer in the file (a formatter or a
// later edit got there first) keeps its Added and Removed lines, so it still
// counts, and has no Ranges, so it claims no position.
type Change struct {
	Ranges  []LineRange
	Added   []string
	Removed []string
}

// LinesAdded is the number of lines the change added or changed.
func (c Change) LinesAdded() int { return len(c.Added) }

// merge appends another change to this one.
func (c *Change) merge(o Change) {
	c.Ranges = append(c.Ranges, o.Ranges...)
	c.Added = append(c.Added, o.Added...)
	c.Removed = append(c.Removed, o.Removed...)
}

// maxDiffCells bounds the alignment table diffLines builds for the part of
// two texts that differs. Past it the differing part is treated as replaced
// wholesale, which over-counts but never under-counts. 4M cells is 16 MB and
// covers two differing regions of 2,000 lines each.
const maxDiffCells = 4_000_000

// ChangeFromHunks reads the added line ranges and removed lines straight off
// a patch. Added lines separated only by removed lines are adjacent in the
// post-edit file and form one range.
func ChangeFromHunks(hunks []PatchHunk) Change {
	var c Change
	for _, h := range hunks {
		w := rangeWriter{c: &c}
		n := h.NewStart
		if n < 1 {
			n = 1
		}
		for _, l := range h.Lines {
			marker := byte(' ')
			if l != "" {
				marker = l[0]
			}
			switch marker {
			case '+':
				w.add(n)
				c.Added = append(c.Added, l[1:])
				n++
			case '-':
				c.Removed = append(c.Removed, l[1:])
			case '\\':
				// "\ No newline at end of file": not a line of either side.
			default:
				w.flush()
				n++
			}
		}
		w.flush()
	}
	return c
}

// DiffChange compares a file's content before and after an edit.
func DiffChange(before, after string) Change {
	return diffLines(splitLines(before), splitLines(after), 1)
}

// EditChange is the change an Edit made, from its old and new strings.
// startLine is the 1-based line of the post-edit file where newString
// begins. Lines the two strings share, the context the agent repeated to
// anchor the edit, are not part of the change.
func EditChange(oldString, newString string, startLine int) Change {
	if startLine < 1 {
		startLine = 1
	}
	return diffLines(splitLines(oldString), splitLines(newString), startLine)
}

// EditsChange is the change a list of old/new string substitutions made, for
// agents whose hook payload carries no patch. Each new string is located in
// the post-edit content the way Extract does it (first occurrence no earlier
// edit has claimed, byte-exact, then with CRLF folded), and the lines the old
// and new strings share are left out of the change. It returns the number of
// edits whose new string is no longer in the file; those keep their added and
// removed lines and get no range.
//
// An edit with an empty new string is a deletion: it adds nothing and its old
// string's lines are reported removed.
func EditsChange(edits []Edit, postContent string) (Change, int) {
	c, unlocated := editsChange(edits, postContent)
	if unlocated == 0 {
		return c, 0
	}
	crlf := strings.Contains(postContent, "\r\n")
	for _, e := range edits {
		crlf = crlf || strings.Contains(e.NewString, "\r\n")
	}
	if !crlf {
		return c, unlocated
	}
	folded := make([]Edit, len(edits))
	for i, e := range edits {
		folded[i] = Edit{OldString: normalizeNewlines(e.OldString), NewString: normalizeNewlines(e.NewString)}
	}
	if retry, n := editsChange(folded, normalizeNewlines(postContent)); n < unlocated {
		return retry, n
	}
	return c, unlocated
}

func editsChange(edits []Edit, postContent string) (Change, int) {
	var c Change
	var claimed []claimedRange
	unlocated := 0
	for _, e := range edits {
		if e.NewString == "" {
			c.Removed = append(c.Removed, splitLines(e.OldString)...)
			continue
		}
		idx := findUnclaimed(postContent, e.NewString, claimed)
		if idx == -1 {
			unlocated++
			lost := EditChange(e.OldString, e.NewString, 1)
			lost.Ranges = nil
			c.merge(lost)
			continue
		}
		claimed = append(claimed, claimedRange{start: idx, end: idx + len(e.NewString)})
		c.merge(EditChange(e.OldString, e.NewString, 1+newlinesBefore(postContent, idx)))
	}
	return c, unlocated
}

// LocateApplyPatch finds each hunk of one patched file in the post-edit
// content and returns the lines the patch added there, plus the number of
// hunks it could not find. Every hunk's added and removed lines are in the
// result whether or not it was found; only a found hunk gets ranges.
//
// A hunk is located by its whole post-image, context lines included, searched
// from the end of the previous hunk. Searching for the added text alone, as
// Extract does, puts a short block (a lone "}" or "return x;") on the first
// line of the file that happens to read the same. Lines one hunk has claimed
// are not offered to the next.
//
// When the whole image is not in the file (the context was reformatted or
// edited since), the search is retried with one line of context either side
// of the added lines, then with none. Position and claims still apply, so a
// retry cannot land on lines an earlier hunk took.
func LocateApplyPatch(f ApplyPatchFile, postContent string) (Change, int) {
	post := splitLines(postContent)
	claimed := make([]bool, len(post))
	var c Change
	unlocated := 0
	cursor := 0
	for _, h := range f.Hunks {
		var image []string
		var addedAt []int // offsets into image of the added lines
		for _, l := range h.Lines {
			switch {
			case l == "":
				image = append(image, "")
			case l[0] == '-':
				c.Removed = append(c.Removed, l[1:])
			case l[0] == '+':
				addedAt = append(addedAt, len(image))
				image = append(image, l[1:])
				c.Added = append(c.Added, l[1:])
			default:
				image = append(image, l[1:])
			}
		}
		if len(addedAt) == 0 {
			continue
		}
		first, last := addedAt[0], addedAt[len(addedAt)-1]
		at, lead := -1, 0
		for _, context := range []int{len(image), 1, 0} {
			lo := max(first-context, 0)
			hi := min(last+context+1, len(image))
			if context < len(image) && lo == 0 && hi == len(image) {
				continue // same window as the attempt before
			}
			window := image[lo:hi]
			offsets := make([]int, len(addedAt))
			for i, a := range addedAt {
				offsets[i] = a - lo
			}
			found := indexUnclaimed(post, window, offsets, claimed, cursor)
			if found < 0 && cursor > 0 {
				found = indexUnclaimed(post, window, offsets, claimed, 0)
			}
			if found >= 0 {
				at, lead = found, lo
				break
			}
		}
		if at < 0 {
			unlocated++
			continue
		}
		w := rangeWriter{c: &c}
		next := 0
		for i := first; i <= last; i++ {
			line := at + i - lead
			if next < len(addedAt) && addedAt[next] == i {
				claimed[line] = true
				w.add(line + 1)
				next++
			} else {
				w.flush()
			}
		}
		w.flush()
		cursor = at - lead + len(image)
		if cursor > len(post) {
			cursor = len(post)
		}
	}
	return c, unlocated
}

// MergeApplyPatchFiles joins the sections of one patch that name the same
// file, in order, so LocateApplyPatch sees all of a file's hunks together and
// two sections cannot claim the same lines. Agents emit one "Update File"
// section per hunk as often as one per file.
func MergeApplyPatchFiles(files []ApplyPatchFile) []ApplyPatchFile {
	var out []ApplyPatchFile
	index := map[string]int{}
	for _, f := range files {
		i, seen := index[f.FilePath]
		if !seen {
			index[f.FilePath] = len(out)
			out = append(out, f)
			continue
		}
		out[i].Edits = append(out[i].Edits, f.Edits...)
		out[i].Hunks = append(out[i].Hunks, f.Hunks...)
		out[i].LinesAdded += f.LinesAdded
		out[i].LinesRemoved += f.LinesRemoved
	}
	return out
}

// rangeWriter collects consecutive added line numbers into LineRanges.
type rangeWriter struct {
	c     *Change
	start int
	end   int
}

func (w *rangeWriter) add(line int) {
	if w.start != 0 && line == w.end+1 {
		w.end = line
		return
	}
	w.flush()
	w.start, w.end = line, line
}

func (w *rangeWriter) flush() {
	if w.start == 0 {
		return
	}
	w.c.Ranges = append(w.c.Ranges, LineRange{
		StartLine:    w.start,
		EndLine:      w.end,
		NewTextLines: w.end - w.start + 1,
	})
	w.start, w.end = 0, 0
}

// diffLines aligns a against b and reports what b adds and a loses.
// firstLine is the file line number of b[0].
func diffLines(a, b []string, firstLine int) Change {
	prefix := 0
	for prefix < len(a) && prefix < len(b) && sameLine(a[prefix], b[prefix]) {
		prefix++
	}
	suffix := 0
	for suffix < len(a)-prefix && suffix < len(b)-prefix &&
		sameLine(a[len(a)-1-suffix], b[len(b)-1-suffix]) {
		suffix++
	}
	a = a[prefix : len(a)-suffix]
	b = b[prefix : len(b)-suffix]
	base := firstLine + prefix // file line of b[0]

	var c Change
	w := rangeWriter{c: &c}
	m, n := len(a), len(b)
	if m == 0 || n == 0 || m*n > maxDiffCells {
		c.Removed = append(c.Removed, a...)
		c.Added = append(c.Added, b...)
		for j := range b {
			w.add(base + j)
		}
		w.flush()
		return c
	}

	lcs, width := lcsTable(a, b)
	i, j := 0, 0
	for i < m && j < n {
		switch {
		case sameLine(a[i], b[j]):
			w.flush()
			i++
			j++
		case lcs[(i+1)*width+j] >= lcs[i*width+j+1]:
			c.Removed = append(c.Removed, a[i])
			i++
		default:
			w.add(base + j)
			c.Added = append(c.Added, b[j])
			j++
		}
	}
	c.Removed = append(c.Removed, a[i:]...)
	for ; j < n; j++ {
		w.add(base + j)
		c.Added = append(c.Added, b[j])
	}
	w.flush()
	return c
}

// lcsTable is the longest-common-subsequence table of a and b, filled from
// the end so a walk can run forward: entry i*width+j is the length of the
// LCS of a[i:] and b[j:].
func lcsTable(a, b []string) (lcs []int32, width int) {
	m, n := len(a), len(b)
	width = n + 1
	lcs = make([]int32, (m+1)*width)
	for i := m - 1; i >= 0; i-- {
		for j := n - 1; j >= 0; j-- {
			switch {
			case sameLine(a[i], b[j]):
				lcs[i*width+j] = lcs[(i+1)*width+j+1] + 1
			case lcs[(i+1)*width+j] >= lcs[i*width+j+1]:
				lcs[i*width+j] = lcs[(i+1)*width+j]
			default:
				lcs[i*width+j] = lcs[i*width+j+1]
			}
		}
	}
	return lcs, width
}

// AlignLines pairs the lines of a with the lines of b the way the diff does:
// match[i] is the index in b of the line a[i] is kept as, or -1 when the
// diff counts a[i] as removed. Pairs never cross, and where a line could be
// kept as more than one line of b the walk takes the earliest.
//
// ok is false when the part of the two that differs is larger than the diff
// aligns (maxDiffCells). The diff then treats that part as replaced
// wholesale, so there are no positions to read from it; only the common
// lines at either end are paired.
func AlignLines(a, b []string) (match []int, ok bool) {
	match = make([]int, len(a))
	for i := range match {
		match[i] = -1
	}
	prefix := 0
	for prefix < len(a) && prefix < len(b) && sameLine(a[prefix], b[prefix]) {
		match[prefix] = prefix
		prefix++
	}
	suffix := 0
	for suffix < len(a)-prefix && suffix < len(b)-prefix &&
		sameLine(a[len(a)-1-suffix], b[len(b)-1-suffix]) {
		match[len(a)-1-suffix] = len(b) - 1 - suffix
		suffix++
	}
	a = a[prefix : len(a)-suffix]
	b = b[prefix : len(b)-suffix]
	m, n := len(a), len(b)
	if m == 0 || n == 0 {
		return match, true
	}
	if m*n > maxDiffCells {
		return match, false
	}
	lcs, width := lcsTable(a, b)
	i, j := 0, 0
	for i < m && j < n {
		switch {
		case sameLine(a[i], b[j]):
			match[prefix+i] = prefix + j
			i++
			j++
		case lcs[(i+1)*width+j] >= lcs[i*width+j+1]:
			i++
		default:
			j++
		}
	}
	return match, true
}

// RangesOf groups ascending 1-based line numbers into ranges of consecutive
// lines.
func RangesOf(lines []int) []LineRange {
	var c Change
	w := rangeWriter{c: &c}
	for _, n := range lines {
		w.add(n)
	}
	w.flush()
	return c.Ranges
}

// indexUnclaimed returns the first index at or after from where needle
// occurs in hay as a run of lines, with none of its added lines (offsets
// addedAt) already claimed, or -1.
func indexUnclaimed(hay, needle []string, addedAt []int, claimed []bool, from int) int {
	for at := from; at+len(needle) <= len(hay); at++ {
		match := true
		for k := range needle {
			if !sameLine(hay[at+k], needle[k]) {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		free := true
		for _, k := range addedAt {
			if claimed[at+k] {
				free = false
				break
			}
		}
		if free {
			return at
		}
	}
	return -1
}

// splitLines splits text into lines. A trailing newline ends the last line;
// it does not start an empty one.
func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// sameLine compares two lines ignoring a trailing CR, so LF text from a hook
// payload matches the same line of a CRLF file.
func sameLine(a, b string) bool {
	return strings.TrimSuffix(a, "\r") == strings.TrimSuffix(b, "\r")
}

// Lines splits text into lines the way the diff counts them: a trailing
// newline ends the last line and does not start an empty one.
func Lines(text string) []string { return splitLines(text) }

// DiffLineSlices is DiffChange for content already split into lines. The
// lines need not be text: a caller that only has a fingerprint per line can
// diff the fingerprints and read the ranges off the result.
func DiffLineSlices(before, after []string) Change { return diffLines(before, after, 1) }
