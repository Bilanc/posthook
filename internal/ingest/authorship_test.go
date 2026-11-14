package ingest

import (
	"testing"

	"github.com/bilanc/posthook/internal/lineranges"
)

func TestApplyHunks(t *testing.T) {
	original := "a\nb\nc\nd\ne\nf\n"
	cases := []struct {
		name     string
		original string
		hunks    []lineranges.PatchHunk
		want     string
		ok       bool
	}{
		{"insert in the middle", original,
			[]lineranges.PatchHunk{{NewStart: 2, Lines: []string{" b", "+x", "+y", " c"}}},
			"a\nb\nx\ny\nc\nd\ne\nf\n", true},
		{"replace a line", original,
			[]lineranges.PatchHunk{{NewStart: 3, Lines: []string{"-c", "+C"}}},
			"a\nb\nC\nd\ne\nf\n", true},
		{"remove lines", original,
			[]lineranges.PatchHunk{{NewStart: 2, Lines: []string{" b", "-c", "-d", " e"}}},
			"a\nb\ne\nf\n", true},
		{"two hunks, the second placed after the first's growth", original,
			[]lineranges.PatchHunk{
				{NewStart: 1, Lines: []string{"+top", " a"}},
				{NewStart: 6, Lines: []string{" e", "+tail", " f"}},
			},
			"top\na\nb\nc\nd\ne\ntail\nf\n", true},
		{"insert at the very top", original,
			[]lineranges.PatchHunk{{NewStart: 1, Lines: []string{"+top"}}},
			"top\na\nb\nc\nd\ne\nf\n", true},
		{"append at the end", original,
			[]lineranges.PatchHunk{{NewStart: 6, Lines: []string{" f", "+g"}}},
			"a\nb\nc\nd\ne\nf\ng\n", true},
		{"into an empty file", "",
			[]lineranges.PatchHunk{{NewStart: 1, Lines: []string{"+only"}}},
			"only\n", true},
		{"everything removed", "a\n",
			[]lineranges.PatchHunk{{NewStart: 1, Lines: []string{"-a"}}},
			"", true},
		{"a CRLF file keeps its line endings on kept lines", "a\r\nb\r\n",
			[]lineranges.PatchHunk{{NewStart: 1, Lines: []string{" a", "+x", " b"}}},
			"a\r\nx\nb\r\n", true},
		{"no-newline marker is not a line", original,
			[]lineranges.PatchHunk{{NewStart: 6, Lines: []string{"-f", "+F", "\\ No newline at end of file"}}},
			"a\nb\nc\nd\ne\nF\n", true},
		{"context that is not there", original,
			[]lineranges.PatchHunk{{NewStart: 2, Lines: []string{" WRONG", "+x"}}},
			"", false},
		{"removed line that is not there", original,
			[]lineranges.PatchHunk{{NewStart: 2, Lines: []string{"-WRONG"}}},
			"", false},
		{"hunks out of order", original,
			[]lineranges.PatchHunk{
				{NewStart: 5, Lines: []string{" e", "+x"}},
				{NewStart: 1, Lines: []string{" a", "+y"}},
			},
			"", false},
		{"a hunk past the end of the file", original,
			[]lineranges.PatchHunk{{NewStart: 40, Lines: []string{"+x"}}},
			"", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := applyHunks(c.original, c.hunks)
			if ok != c.ok || (ok && got != c.want) {
				t.Errorf("applyHunks = %q, %v; want %q, %v", got, ok, c.want, c.ok)
			}
		})
	}
}

func TestGitBlobID(t *testing.T) {
	// The ids `git hash-object` gives the same content.
	for content, want := range map[string]string{
		"":        "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391",
		"hello\n": "ce013625030ba8dba906f756967f9e9ca394464a",
	} {
		if got := gitBlobID(content); got != want {
			t.Errorf("gitBlobID(%q) = %s, want %s", content, got, want)
		}
	}
}
