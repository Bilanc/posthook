package lineranges

import (
	"reflect"
	"testing"
)

// Agents send LF text in hook payloads and apply_patch blocks regardless of
// the file's line endings; these cover CRLF files receiving LF text, the
// reverse, and that line numbers are unaffected by the dropped CR bytes.

func TestExtractEditLocatesLFTextInCRLFFile(t *testing.T) {
	file := "package a\r\n\r\nfunc old() {}\r\nfunc added() {\r\n\treturn\r\n}\r\n\r\nfunc tail() {}\r\n"
	got := Extract("Edit", ToolInput{NewString: "func added() {\n\treturn\n}"}, file)
	want := Extracted{Ranges: []LineRange{{StartLine: 4, EndLine: 6, NewTextLines: 3}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
}

func TestExtractEditLocatesCRLFTextInLFFile(t *testing.T) {
	file := "a\nb\nnew\nc\n"
	got := Extract("Edit", ToolInput{NewString: "new\r\n"}, file)
	want := Extracted{Ranges: []LineRange{{StartLine: 3, EndLine: 3, NewTextLines: 1}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
}

func TestExtractMultiEditClaimsOffsetsConsistentlyInCRLFFile(t *testing.T) {
	// The same text appears twice; the second edit must land on the second
	// occurrence, which only works if claimed offsets and the search run on
	// the same (normalized) content.
	file := "x\r\nfoo\r\ny\r\nfoo\r\nz\r\n"
	got := Extract("MultiEdit", ToolInput{Edits: []Edit{{NewString: "foo"}, {NewString: "foo"}}}, file)
	want := Extracted{Ranges: []LineRange{
		{StartLine: 2, EndLine: 2, NewTextLines: 1},
		{StartLine: 4, EndLine: 4, NewTextLines: 1},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
}

func TestExtractShellWriteAppendInCRLFFile(t *testing.T) {
	file := "first\r\nsecond\r\nappended\r\n"
	got := Extract("ShellWrite", ToolInput{Content: "appended\n"}, file)
	want := Extracted{Ranges: []LineRange{{StartLine: 3, EndLine: 3, NewTextLines: 1}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
}

func TestExtractApplyPatchBlocksInCRLFFile(t *testing.T) {
	// ParseApplyPatch folds the patch to LF; the file keeps CRLF.
	patch := "*** Begin Patch\n*** Update File: a.md\n@@\n intro\n+one\n+two\n outro\n*** End Patch\n"
	files := ParseApplyPatch(patch)
	if len(files) != 1 || len(files[0].Edits) != 1 {
		t.Fatalf("parsed %#v", files)
	}
	file := "intro\r\none\r\ntwo\r\noutro\r\n"
	got := Extract("MultiEdit", ToolInput{Edits: files[0].Edits}, file)
	want := Extracted{Ranges: []LineRange{{StartLine: 2, EndLine: 3, NewTextLines: 2}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
}

func TestExtractWriteCountsCRLFLines(t *testing.T) {
	got := Extract("Write", ToolInput{Content: "a\r\nb\r\nc\r\n"}, "a\r\nb\r\nc\r\n")
	want := Extracted{Ranges: []LineRange{{StartLine: 1, EndLine: 3, NewTextLines: 3}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
}

func TestExtractLFFilesUnchanged(t *testing.T) {
	file := "a\nb\nnew\nc\n"
	got := Extract("Edit", ToolInput{NewString: "new"}, file)
	want := Extracted{Ranges: []LineRange{{StartLine: 3, EndLine: 3, NewTextLines: 1}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
	if got := Extract("Edit", ToolInput{NewString: "missing"}, file); !reflect.DeepEqual(got, Extracted{Unlocated: 1}) {
		t.Fatalf("unlocated edit: got %#v", got)
	}
}

// Mixed-ending files: an exact match must win over the folded fallback, so
// the occurrence whose line ending matches the payload is the one attributed.
func TestExtractPrefersExactMatchInMixedEndingFile(t *testing.T) {
	file := "same\nother\nsame\r\n"
	got := Extract("Edit", ToolInput{NewString: "same\r\n"}, file)
	want := Extracted{Ranges: []LineRange{{StartLine: 3, EndLine: 3, NewTextLines: 1}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}

	file = "a\r\nfoo\nb\r\nfoo\r\n"
	got = Extract("MultiEdit", ToolInput{Edits: []Edit{{NewString: "foo\r\n"}}}, file)
	want = Extracted{Ranges: []LineRange{{StartLine: 4, EndLine: 4, NewTextLines: 1}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
}

func TestExtractFoldedRetryOnlyWhenItLocatesMore(t *testing.T) {
	// Exact pass locates one of two edits; the folded pass locates both and wins.
	file := "x\r\nfoo\r\nbar\r\n"
	got := Extract("MultiEdit", ToolInput{Edits: []Edit{{NewString: "foo\r\n"}, {NewString: "bar\n"}}}, file)
	want := Extracted{Ranges: []LineRange{
		{StartLine: 2, EndLine: 2, NewTextLines: 1},
		{StartLine: 3, EndLine: 3, NewTextLines: 1},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}

	// Nothing to gain from folding: the exact result (with its unlocated
	// count) is returned unchanged.
	got = Extract("MultiEdit", ToolInput{Edits: []Edit{{NewString: "foo\r\n"}, {NewString: "nope\n"}}}, file)
	want = Extracted{Ranges: []LineRange{{StartLine: 2, EndLine: 2, NewTextLines: 1}}, Unlocated: 1}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
}
