// Package lineranges extracts line-range attribution from AI tool calls.
//
// Strategy: locate by string match against the post-edit file content.
// For Edit/MultiEdit we search for new_string in the file as it now exists
// (PostToolUse has already applied the change). For Write the whole file is
// new, so the range is 1..lines(content).
//
// Known limitations (Phase 1):
//   - MultiEdit with identical new_strings attributes both to the first match.
//   - Pure deletions (new_string empty) record no range.
//   - Shell-driven edits are recovered from the command text (see shell.go):
//     heredoc, here-string and echo/printf writes carry their content and are
//     located like an Edit; `sed -i` and program output only mark the file.
package lineranges

import "strings"

type LineRange struct {
	StartLine    int `json:"start_line"`
	EndLine      int `json:"end_line"`
	NewTextLines int `json:"new_text_lines"`
}

// Edit is one (old, new) substitution. Used by MultiEdit and apply_patch.
type Edit struct {
	OldString string `json:"old_string"`
	NewString string `json:"new_string"`
}

// ToolInput is the parsed shape we accept from agent payloads. Not all fields
// are populated for every tool; extractors look up only the ones they need.
type ToolInput struct {
	FilePath  string
	OldString string
	NewString string
	Content   string
	Edits     []Edit
}

// Extracted is the result of a single extract call.
type Extracted struct {
	Ranges []LineRange
	// Edits we recognized but couldn't locate (new_string not found in
	// post content). Useful as an accuracy counter.
	Unlocated int
}

// ApplyPatchFile describes a single file's worth of edits parsed from an
// apply_patch command (Codex CLI's bulk edit tool).
type ApplyPatchFile struct {
	FilePath     string
	Edits        []Edit
	LinesAdded   int
	LinesRemoved int
}

// linesIn counts how many lines `text` occupies. A trailing newline doesn't
// add an extra empty line.
func linesIn(text string) int {
	if len(text) == 0 {
		return 0
	}
	count := 0
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' {
			count++
		}
	}
	if text[len(text)-1] != '\n' {
		count++
	}
	return count
}

func newlinesBefore(text string, index int) int {
	count := 0
	for i := 0; i < index && i < len(text); i++ {
		if text[i] == '\n' {
			count++
		}
	}
	return count
}

type claimedRange struct {
	start int
	end   int // exclusive
}

func findUnclaimed(haystack, needle string, claimed []claimedRange) int {
	if len(needle) == 0 {
		return -1
	}
	from := 0
	for from <= len(haystack)-len(needle) {
		idx := strings.Index(haystack[from:], needle)
		if idx == -1 {
			return -1
		}
		idx += from
		end := idx + len(needle)
		overlaps := false
		for _, c := range claimed {
			if idx < c.end && end > c.start {
				overlaps = true
				break
			}
		}
		if !overlaps {
			return idx
		}
		from = idx + 1
	}
	return -1
}

func rangeForMatch(postContent string, matchIndex int, newString string) LineRange {
	startLine := 1 + newlinesBefore(postContent, matchIndex)
	newTextLines := linesIn(newString)
	endLine := startLine + newTextLines - 1
	if endLine < startLine {
		endLine = startLine
	}
	return LineRange{StartLine: startLine, EndLine: endLine, NewTextLines: newTextLines}
}

// Extract returns the line ranges that the named tool wrote into postContent.
//
// Matching is byte-exact first: that is what every edit to a consistently
// LF file needs, and in a file with mixed line endings it is the only thing
// that tells otherwise identical occurrences apart. Only when something is
// left unlocated is the search retried with CRLF folded to LF on both sides,
// because agents hand us LF text (hook payloads, apply_patch blocks, shell
// heredocs) even when the file on disk uses CRLF, and the odd payload carries
// CRLF into an LF file. Dropping the CR bytes leaves every '\n' in place, so
// line numbers computed on the folded content are the file's real line
// numbers and the new-text line counts are unchanged; the retry is kept only
// if it locates more edits than the exact pass.
func Extract(toolName string, in ToolInput, postContent string) Extracted {
	out := extract(toolName, in, postContent)
	if out.Unlocated == 0 || !mentionsCRLF(in, postContent) {
		return out
	}
	retry := extract(toolName, normalizeInput(in), normalizeNewlines(postContent))
	if retry.Unlocated < out.Unlocated {
		return retry
	}
	return out
}

func mentionsCRLF(in ToolInput, postContent string) bool {
	if strings.Contains(postContent, "\r\n") || strings.Contains(in.NewString, "\r\n") || strings.Contains(in.Content, "\r\n") {
		return true
	}
	for _, e := range in.Edits {
		if strings.Contains(e.NewString, "\r\n") {
			return true
		}
	}
	return false
}

func normalizeInput(in ToolInput) ToolInput {
	out := in
	out.NewString = normalizeNewlines(in.NewString)
	out.Content = normalizeNewlines(in.Content)
	if len(in.Edits) > 0 {
		out.Edits = make([]Edit, len(in.Edits))
		for i, e := range in.Edits {
			out.Edits[i] = Edit{OldString: e.OldString, NewString: normalizeNewlines(e.NewString)}
		}
	}
	return out
}

// extract is one byte-exact pass over postContent.
func extract(toolName string, in ToolInput, postContent string) Extracted {
	switch toolName {
	case "Write":
		content := in.Content
		if content == "" {
			content = postContent
		}
		total := linesIn(content)
		if total == 0 {
			return Extracted{}
		}
		return Extracted{Ranges: []LineRange{{StartLine: 1, EndLine: total, NewTextLines: total}}}

	case "Edit":
		if in.NewString == "" {
			return Extracted{}
		}
		idx := strings.Index(postContent, in.NewString)
		if idx == -1 {
			return Extracted{Unlocated: 1}
		}
		return Extracted{Ranges: []LineRange{rangeForMatch(postContent, idx, in.NewString)}}

	case "ShellWrite":
		// Content recovered from a shell command (heredoc, echo …). A whole
		// file write matches at offset 0; an append matches at the end, so
		// prefer the last occurrence.
		if in.Content == "" {
			return Extracted{}
		}
		idx := strings.LastIndex(postContent, in.Content)
		if idx == -1 {
			return Extracted{Unlocated: 1}
		}
		return Extracted{Ranges: []LineRange{rangeForMatch(postContent, idx, in.Content)}}

	case "MultiEdit":
		var ranges []LineRange
		var claimed []claimedRange
		unlocated := 0
		for _, edit := range in.Edits {
			if edit.NewString == "" {
				continue
			}
			idx := findUnclaimed(postContent, edit.NewString, claimed)
			if idx == -1 {
				unlocated++
				continue
			}
			claimed = append(claimed, claimedRange{start: idx, end: idx + len(edit.NewString)})
			ranges = append(ranges, rangeForMatch(postContent, idx, edit.NewString))
		}
		return Extracted{Ranges: ranges, Unlocated: unlocated}
	}
	return Extracted{}
}

// normalizeNewlines folds CRLF to LF for the fallback pass. Lone CRs are
// left alone: they are not line endings in any format we see.
func normalizeNewlines(s string) string {
	if !strings.Contains(s, "\r\n") {
		return s
	}
	return strings.ReplaceAll(s, "\r\n", "\n")
}

// ParseApplyPatch parses Codex CLI's apply_patch command text into a slice of
// per-file edits. We extract only added blocks (lines prefixed with `+`) since
// only insertions produce blame-relevant ranges.
func ParseApplyPatch(command string) []ApplyPatchFile {
	var files []ApplyPatchFile
	var current *ApplyPatchFile
	var addedBlock []string

	flush := func() {
		if current == nil || len(addedBlock) == 0 {
			return
		}
		ns := strings.Join(addedBlock, "\n")
		if ns != "" {
			current.Edits = append(current.Edits, Edit{NewString: ns})
		}
		addedBlock = nil
	}

	normalized := strings.ReplaceAll(command, "\r\n", "\n")
	for _, line := range strings.Split(normalized, "\n") {
		switch {
		case strings.HasPrefix(line, "*** Add File: "):
			flush()
			path := strings.TrimPrefix(line, "*** Add File: ")
			files = append(files, ApplyPatchFile{FilePath: path})
			current = &files[len(files)-1]
		case strings.HasPrefix(line, "*** Update File: "):
			flush()
			path := strings.TrimPrefix(line, "*** Update File: ")
			files = append(files, ApplyPatchFile{FilePath: path})
			current = &files[len(files)-1]
		case strings.HasPrefix(line, "*** Delete File: "):
			flush()
			current = nil
		case strings.HasPrefix(line, "*** Move to: "):
			if current != nil {
				current.FilePath = strings.TrimPrefix(line, "*** Move to: ")
			}
		case strings.HasPrefix(line, "*** "):
			flush()
		default:
			if current == nil {
				continue
			}
			if strings.HasPrefix(line, "+") {
				current.LinesAdded++
				addedBlock = append(addedBlock, line[1:])
			} else {
				flush()
				if strings.HasPrefix(line, "-") {
					current.LinesRemoved++
				}
			}
		}
	}
	flush()

	// Drop empty entries.
	out := files[:0]
	for _, f := range files {
		if f.FilePath != "" {
			out = append(out, f)
		}
	}
	return out
}
