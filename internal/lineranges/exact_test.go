package lineranges

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// These cover the over-counts a customer measured line by line against
// `git diff`: a full-file rewrite scored as the whole file, an Edit scored
// with the context it repeats, and apply_patch blocks placed by their text
// alone.

func lines(n int, format string) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, format+"\n", i)
	}
	return b.String()
}

func TestDiffChangeRewriteCountsOnlyNewLines(t *testing.T) {
	// A 26-line file rewritten whole with two lines appended. Extract("Write")
	// scores this 28; the edit added 2.
	before := lines(26, `    "table_%d": true,`)
	after := before + "    \"new_a\": true,\n    \"new_b\": true\n"

	got := DiffChange(before, after)
	want := Change{
		Ranges: []LineRange{{StartLine: 27, EndLine: 28, NewTextLines: 2}},
		Added:  []string{`    "new_a": true,`, `    "new_b": true`},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
	if whole := Extract("Write", ToolInput{Content: after}, after); whole.Ranges[0].NewTextLines != 28 {
		t.Fatalf("expected the legacy extractor to score the whole file, got %#v", whole)
	}
}

func TestDiffChangeReportsReplacedAndInsertedLines(t *testing.T) {
	before := "a\nb\nc\nd\ne\n"
	after := "a\nB\nc\nd\nnew1\nnew2\ne\n"

	got := DiffChange(before, after)
	want := Change{
		Ranges: []LineRange{
			{StartLine: 2, EndLine: 2, NewTextLines: 1},
			{StartLine: 5, EndLine: 6, NewTextLines: 2},
		},
		Added:   []string{"B", "new1", "new2"},
		Removed: []string{"b"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
	if got.LinesAdded() != 3 {
		t.Fatalf("LinesAdded = %d, want 3", got.LinesAdded())
	}
}

func TestDiffChangeNewAndEmptiedFiles(t *testing.T) {
	created := DiffChange("", "one\ntwo\n")
	if want := (Change{Ranges: []LineRange{{StartLine: 1, EndLine: 2, NewTextLines: 2}}, Added: []string{"one", "two"}}); !reflect.DeepEqual(created, want) {
		t.Fatalf("new file: got %#v\nwant %#v", created, want)
	}
	emptied := DiffChange("one\ntwo\n", "")
	if want := (Change{Removed: []string{"one", "two"}}); !reflect.DeepEqual(emptied, want) {
		t.Fatalf("emptied file: got %#v\nwant %#v", emptied, want)
	}
	if same := DiffChange("one\ntwo\n", "one\ntwo\n"); same.LinesAdded() != 0 || len(same.Removed) != 0 {
		t.Fatalf("identical content should be no change, got %#v", same)
	}
}

func TestDiffChangeIgnoresLineEndingDifferences(t *testing.T) {
	// The payload is LF, the file on disk CRLF: only the real change counts.
	got := DiffChange("a\r\nb\r\nc\r\n", "a\nb\nadded\nc\n")
	want := Change{Ranges: []LineRange{{StartLine: 3, EndLine: 3, NewTextLines: 1}}, Added: []string{"added"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
}

func TestDiffChangeFallsBackToWholesaleReplaceWhenTooLarge(t *testing.T) {
	// Two differing regions too large to align: never under-count.
	n := 2100
	before := lines(n, "old %d")
	after := lines(n, "new %d")
	got := DiffChange(before, after)
	if got.LinesAdded() != n || len(got.Removed) != n {
		t.Fatalf("added %d removed %d, want %d each", got.LinesAdded(), len(got.Removed), n)
	}
}

func TestEditChangeSkipsRepeatedContext(t *testing.T) {
	// The agent repeats three unchanged lines to anchor one new line. The
	// new string sits at lines 3-6 of the file; only line 5 is new.
	oldString := "function a() {\n    return 1;\n}"
	newString := "function a() {\n    return 1;\n    // added by agent\n}"
	file := "line1\nline2\n" + newString + "\nline7\n"

	got := EditChange(oldString, newString, 3)
	want := Change{Ranges: []LineRange{{StartLine: 5, EndLine: 5, NewTextLines: 1}}, Added: []string{"    // added by agent"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
	legacy := Extract("Edit", ToolInput{OldString: oldString, NewString: newString}, file)
	if legacy.Ranges[0].NewTextLines != 4 {
		t.Fatalf("expected the legacy extractor to score all four lines, got %#v", legacy)
	}
}

func TestEditChangeMidLineEdit(t *testing.T) {
	// An Edit that swaps part of one line changes that line.
	got := EditChange("retries = 3", "retries = 5", 12)
	want := Change{
		Ranges:  []LineRange{{StartLine: 12, EndLine: 12, NewTextLines: 1}},
		Added:   []string{"retries = 5"},
		Removed: []string{"retries = 3"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
}

func TestChangeFromHunksReadsAddedLinesAtTheirNewPositions(t *testing.T) {
	// structuredPatch as Claude Code reports it for an Edit.
	hunks := []PatchHunk{{
		NewStart: 10,
		Lines: []string{
			" func load() {",
			"-\treturn nil",
			"+\tcfg, err := read()",
			"+\tif err != nil {",
			" \t\treturn err",
			"+\t}",
			" }",
		},
	}}
	got := ChangeFromHunks(hunks)
	want := Change{
		Ranges: []LineRange{
			{StartLine: 11, EndLine: 12, NewTextLines: 2},
			{StartLine: 14, EndLine: 14, NewTextLines: 1},
		},
		Added:   []string{"\tcfg, err := read()", "\tif err != nil {", "\t}"},
		Removed: []string{"\treturn nil"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
}

func TestChangeFromHunksSeveralHunksAndNoNewlineMarker(t *testing.T) {
	hunks := []PatchHunk{
		{NewStart: 1, Lines: []string{"+header", " body"}},
		{NewStart: 40, Lines: []string{" tail", "-old last", "+new last", "\\ No newline at end of file"}},
	}
	got := ChangeFromHunks(hunks)
	want := Change{
		Ranges: []LineRange{
			{StartLine: 1, EndLine: 1, NewTextLines: 1},
			{StartLine: 41, EndLine: 41, NewTextLines: 1},
		},
		Added:   []string{"header", "new last"},
		Removed: []string{"old last"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
}

const questionPHP = `<?php
namespace Shop\Qa;

class Question
{
    public function getId()
    {
        return 1;
    }

    public function getTitle()
    {
        return 1;
    }
}
`

func TestLocateApplyPatchUsesContextNotFirstMatchingText(t *testing.T) {
	// Three sections of one file in one call, as Copilot emits them. The
	// last two change single lines whose text also appears earlier in the
	// file: "    {" at line 7 and "        return 1;" at line 8.
	patch := "*** Begin Patch\n" +
		"*** Update File: Question.php\n@@\n" +
		"+<?php\n+namespace Shop\\Qa;\n+\n+class Question\n+{\n+    public function getId()\n+    {\n+        return 1;\n" +
		"*** Update File: Question.php\n@@\n" +
		"     public function getTitle()\n-    (\n+    {\n" +
		"*** Update File: Question.php\n@@\n" +
		"-        return 2;\n+        return 1;\n" +
		"*** End Patch\n"

	files := MergeApplyPatchFiles(ParseApplyPatch(patch))
	if len(files) != 1 || len(files[0].Hunks) != 3 {
		t.Fatalf("parsed %#v", files)
	}
	got, unlocated := LocateApplyPatch(files[0], questionPHP)
	want := Change{
		Ranges: []LineRange{
			{StartLine: 1, EndLine: 8, NewTextLines: 8},
			{StartLine: 12, EndLine: 12, NewTextLines: 1},
			{StartLine: 13, EndLine: 13, NewTextLines: 1},
		},
		Added: []string{
			"<?php", `namespace Shop\Qa;`, "", "class Question", "{", "    public function getId()", "    {", "        return 1;",
			"    {", "        return 1;",
		},
		Removed: []string{"    (", "        return 2;"},
	}
	if unlocated != 0 || !reflect.DeepEqual(got, want) {
		t.Fatalf("unlocated %d\ngot  %#v\nwant %#v", unlocated, got, want)
	}

	// The text-only locator puts the two single lines on lines 7 and 8,
	// inside the first block.
	var legacy []LineRange
	for _, f := range ParseApplyPatch(patch) {
		legacy = append(legacy, Extract("MultiEdit", ToolInput{Edits: f.Edits}, questionPHP).Ranges...)
	}
	if !reflect.DeepEqual(legacy, []LineRange{
		{StartLine: 1, EndLine: 8, NewTextLines: 8},
		{StartLine: 7, EndLine: 7, NewTextLines: 1},
		{StartLine: 8, EndLine: 8, NewTextLines: 1},
	}) {
		t.Fatalf("legacy ranges changed: %#v", legacy)
	}
}

func TestLocateApplyPatchAddFileAndHunksInOneSection(t *testing.T) {
	patch := "*** Begin Patch\n" +
		"*** Add File: events.xml\n+<config>\n+    <event name=\"a\"/>\n+</config>\n" +
		"*** Update File: README.md\n@@ intro\n # Title\n+\n+New paragraph.\n@@ usage\n ## Usage\n-old step\n+new step\n" +
		"*** End Patch\n"
	files := MergeApplyPatchFiles(ParseApplyPatch(patch))
	if len(files) != 2 {
		t.Fatalf("parsed %#v", files)
	}

	added, unlocated := LocateApplyPatch(files[0], "<config>\n    <event name=\"a\"/>\n</config>\n")
	if want := (Change{
		Ranges: []LineRange{{StartLine: 1, EndLine: 3, NewTextLines: 3}},
		Added:  []string{"<config>", `    <event name="a"/>`, "</config>"},
	}); unlocated != 0 || !reflect.DeepEqual(added, want) {
		t.Fatalf("add file: unlocated %d, got %#v", unlocated, added)
	}

	readme := "# Title\n\nNew paragraph.\n\n## Usage\nnew step\n"
	updated, unlocated := LocateApplyPatch(files[1], readme)
	want := Change{
		Ranges: []LineRange{
			{StartLine: 2, EndLine: 3, NewTextLines: 2},
			{StartLine: 6, EndLine: 6, NewTextLines: 1},
		},
		Added:   []string{"", "New paragraph.", "new step"},
		Removed: []string{"old step"},
	}
	if unlocated != 0 || !reflect.DeepEqual(updated, want) {
		t.Fatalf("update: unlocated %d\ngot  %#v\nwant %#v", unlocated, updated, want)
	}
}

func TestLocateApplyPatchCountsHunksItCannotFind(t *testing.T) {
	// The file moved on before the worker read it. The hunk still counts
	// as one added line; it just has no position.
	patch := "*** Begin Patch\n*** Update File: a.go\n@@\n func a() {\n+\treturn 1\n }\n*** End Patch\n"
	files := ParseApplyPatch(patch)
	got, unlocated := LocateApplyPatch(files[0], "package a\n\nfunc b() {}\n")
	if unlocated != 1 || got.LinesAdded() != 1 || len(got.Ranges) != 0 {
		t.Fatalf("unlocated %d, got %#v", unlocated, got)
	}
}

func TestLocateApplyPatchFallsBackToLessContext(t *testing.T) {
	// A formatter re-indented the line above the hunk's first context line
	// after the patch landed, so the whole post-image is no longer in the
	// file. One line of context either side of the added line still is.
	patch := "*** Begin Patch\n*** Update File: a.go\n@@\n" +
		" func a() {\n \tx := 1\n+\ty := 2\n \treturn x\n }\n*** End Patch\n"
	files := ParseApplyPatch(patch)
	post := "package a\n\nfunc a()\n{\n\tx := 1\n\ty := 2\n\treturn x\n}\n"
	got, unlocated := LocateApplyPatch(files[0], post)
	want := Change{Ranges: []LineRange{{StartLine: 6, EndLine: 6, NewTextLines: 1}}, Added: []string{"\ty := 2"}}
	if unlocated != 0 || !reflect.DeepEqual(got, want) {
		t.Fatalf("unlocated %d\ngot  %#v\nwant %#v", unlocated, got, want)
	}
}

func TestEditsChangeLocatesEachEditAndSkipsContext(t *testing.T) {
	// Two substitutions with the same new text: the second must land on the
	// second occurrence, and neither counts the line it repeats as context.
	edits := []Edit{
		{OldString: "func a() {\n}", NewString: "func a() {\n\tlog()\n}"},
		{OldString: "func b() {\n}", NewString: "func b() {\n\tlog()\n}"},
	}
	post := "package p\n\nfunc a() {\n\tlog()\n}\n\nfunc b() {\n\tlog()\n}\n"
	got, unlocated := EditsChange(edits, post)
	want := Change{
		Ranges: []LineRange{
			{StartLine: 4, EndLine: 4, NewTextLines: 1},
			{StartLine: 8, EndLine: 8, NewTextLines: 1},
		},
		Added: []string{"\tlog()", "\tlog()"},
	}
	if unlocated != 0 || !reflect.DeepEqual(got, want) {
		t.Fatalf("unlocated %d\ngot  %#v\nwant %#v", unlocated, got, want)
	}
}

func TestEditsChangeKeepsTheCountOfAnEditItCannotPlace(t *testing.T) {
	// The new string is gone from the file. The edit still wrote one line.
	edits := []Edit{{OldString: "a = 1\nb = 2", NewString: "a = 1\nb = 3"}}
	got, unlocated := EditsChange(edits, "something else entirely\n")
	want := Change{Added: []string{"b = 3"}, Removed: []string{"b = 2"}}
	if unlocated != 1 || !reflect.DeepEqual(got, want) {
		t.Fatalf("unlocated %d\ngot  %#v\nwant %#v", unlocated, got, want)
	}
}

func TestEditsChangeDeletionAndCRLFFile(t *testing.T) {
	// A deletion adds nothing and reports what it removed.
	gone, unlocated := EditsChange([]Edit{{OldString: "x\ny\n", NewString: ""}}, "a\nb\n")
	if unlocated != 0 || gone.LinesAdded() != 0 || !reflect.DeepEqual(gone.Removed, []string{"x", "y"}) {
		t.Fatalf("deletion: unlocated %d, got %#v", unlocated, gone)
	}
	// LF payload against a CRLF file: located on the folded pass.
	got, unlocated := EditsChange(
		[]Edit{{OldString: "one\ntwo", NewString: "one\nTWO\nthree"}},
		"zero\r\none\r\nTWO\r\nthree\r\nfour\r\n",
	)
	want := Change{
		Ranges:  []LineRange{{StartLine: 3, EndLine: 4, NewTextLines: 2}},
		Added:   []string{"TWO", "three"},
		Removed: []string{"two"},
	}
	if unlocated != 0 || !reflect.DeepEqual(got, want) {
		t.Fatalf("crlf: unlocated %d\ngot  %#v\nwant %#v", unlocated, got, want)
	}
}

func TestParseApplyPatchKeepsLegacyEditsAndCounts(t *testing.T) {
	// Hunks are additive: the fields existing callers read are unchanged.
	patch := "*** Begin Patch\n*** Update File: a.md\n@@\n intro\n+one\n+two\n outro\n-gone\n*** End Patch\n"
	files := ParseApplyPatch(patch)
	if len(files) != 1 {
		t.Fatalf("parsed %#v", files)
	}
	f := files[0]
	if f.LinesAdded != 2 || f.LinesRemoved != 1 || !reflect.DeepEqual(f.Edits, []Edit{{NewString: "one\ntwo"}}) {
		t.Fatalf("legacy fields changed: %#v", f)
	}
	if want := []ApplyPatchHunk{{Lines: []string{" intro", "+one", "+two", " outro", "-gone"}}}; !reflect.DeepEqual(f.Hunks, want) {
		t.Fatalf("hunks: got %#v\nwant %#v", f.Hunks, want)
	}
}

func TestDiffLineSlicesMatchesDiffChange(t *testing.T) {
	// The same diff on lines already split, which is how a caller that holds
	// only a fingerprint per line uses it.
	before := []string{"a", "b", "c"}
	after := []string{"a", "B", "c", "d"}
	got := DiffLineSlices(before, after)
	want := DiffChange("a\nb\nc\n", "a\nB\nc\nd\n")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
	if len(before) != 3 || before[1] != "b" || after[3] != "d" {
		t.Fatal("the inputs were modified")
	}
}
