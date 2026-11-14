package store

import (
	"reflect"
	"testing"
)

func TestLineNumbersRoundTrip(t *testing.T) {
	cases := map[string][]int{
		"":              nil,
		"7":             {7},
		"3-9,14":        {3, 4, 5, 6, 7, 8, 9, 14},
		"1-2,4-5,7":     {1, 2, 4, 5, 7},
		"100-101,10000": {100, 101, 10000},
	}
	for spec, numbers := range cases {
		if got := FormatLineNumbers(numbers); got != spec {
			t.Errorf("FormatLineNumbers(%v) = %q, want %q", numbers, got, spec)
		}
		if got := ParseLineNumbers(spec); !reflect.DeepEqual(got, numbers) {
			t.Errorf("ParseLineNumbers(%q) = %v, want %v", spec, got, numbers)
		}
	}
	// Parts that do not parse are skipped, not fatal.
	if got := ParseLineNumbers("2, x, 9-4, 0, 5-6"); !reflect.DeepEqual(got, []int{2, 5, 6}) {
		t.Errorf("lenient parse: %v", got)
	}
}

func TestCommitLinesRoundTrip(t *testing.T) {
	db := newTestDB(t)
	rows := []CommitLines{
		{SessionID: "s1", Numbers: []int{3, 4, 9}, Norms: []uint64{NormHash("a"), NormHash("b"), NormHash("c")}},
		{SessionID: "s2", Numbers: []int{1}, Norms: []uint64{NormHash("d")}},
		// Nothing to record, and a damaged entry: both left out.
		{SessionID: "s3"},
		{SessionID: "s4", Numbers: []int{1, 2}, Norms: []uint64{1}},
	}
	if err := db.SaveCommitLines("c1", "pkg/a.go", rows); err != nil {
		t.Fatal(err)
	}
	if got := db.CommitLines("c1", "pkg/a.go"); !reflect.DeepEqual(got, rows[:2]) {
		t.Errorf("read back %+v", got)
	}
	if got := db.CommitLines("c1", "pkg/other.go"); len(got) != 0 {
		t.Errorf("another file has rows: %+v", got)
	}
	// Saving again replaces the file's rows.
	if err := db.SaveCommitLines("c1", "pkg/a.go", rows[1:2]); err != nil {
		t.Fatal(err)
	}
	if got := db.CommitLines("c1", "pkg/a.go"); len(got) != 1 || got[0].SessionID != "s2" {
		t.Errorf("after replacing: %+v", got)
	}
	if err := db.DeleteCommitLines("c1"); err != nil {
		t.Fatal(err)
	}
	if got := db.CommitLines("c1", "pkg/a.go"); len(got) != 0 {
		t.Errorf("after deleting: %+v", got)
	}
}

func TestParseCommitAdds(t *testing.T) {
	blobA := "1111111111111111111111111111111111111111"
	blobB := "2222222222222222222222222222222222222222"
	diff := "diff --git a/pkg/a.go b/pkg/a.go\n" +
		"index 0000000000000000000000000000000000000000.." + blobA + " 100644\n" +
		"--- a/pkg/a.go\n" +
		"+++ b/pkg/a.go\n" +
		"@@ -2 +2,2 @@ func a() {\n" +
		"-\told\n" +
		"+\tnew\n" +
		"+++ b/not/a/header\n" + // an added line whose text is "++ b/not/a/header"
		"@@ -9,2 +9,0 @@\n" +
		"-gone\n" +
		"-gone too\n" +
		"@@ -20,0 +19 @@\n" +
		"+last line\n" +
		"\\ No newline at end of file\n" +
		"diff --git a/my file.txt b/my file.txt\n" +
		"new file mode 100644\n" +
		"index 0000000000000000000000000000000000000000.." + blobB + "\n" +
		"--- /dev/null\n" +
		"+++ b/my file.txt\t\n" +
		"@@ -0,0 +1,2 @@\n" +
		"+one\n" +
		"+two\n" +
		"diff --git a/removed.txt b/removed.txt\n" +
		"deleted file mode 100644\n" +
		"index " + blobA + "..0000000000000000000000000000000000000000\n" +
		"--- a/removed.txt\n" +
		"+++ /dev/null\n" +
		"@@ -1 +0,0 @@\n" +
		"-bye\n" +
		"diff --git \"a/quo\\\"ted\" \"b/quo\\\"ted\"\n" +
		"index 0000000000000000000000000000000000000000.." + blobB + "\n" +
		"--- /dev/null\n" +
		"+++ \"b/quo\\\"ted\"\n" +
		"@@ -0,0 +1 @@\n" +
		"+x\n"

	got := parseCommitAdds(diff)
	if len(got) != 2 {
		t.Fatalf("%d files parsed, want 2 (the deleted and the quoted one are left out): %v", len(got), got)
	}
	a := got["pkg/a.go"]
	if a == nil || a.blob != blobA {
		t.Fatalf("pkg/a.go: %+v", a)
	}
	if want := []int{2, 3, 19}; !reflect.DeepEqual(a.numbers, want) {
		t.Errorf("pkg/a.go added lines %v, want %v", a.numbers, want)
	}
	if want := []uint64{NormHash("\tnew"), NormHash("++ b/not/a/header"), NormHash("last line")}; !reflect.DeepEqual(a.norms, want) {
		t.Errorf("pkg/a.go fingerprints differ")
	}
	b := got["my file.txt"]
	if b == nil || b.blob != blobB || !reflect.DeepEqual(b.numbers, []int{1, 2}) {
		t.Errorf("my file.txt: %+v", b)
	}
}
