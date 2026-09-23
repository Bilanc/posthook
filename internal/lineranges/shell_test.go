package lineranges

import (
	"reflect"
	"testing"
)

func TestParseShellWritesHeredoc(t *testing.T) {
	cmd := "cat > demo/ratelimit.go <<'EOF'\npackage demo\n\nfunc a() {}\nEOF"
	got := ParseShellWrites(cmd)
	want := []ShellWrite{{FilePath: "demo/ratelimit.go", Content: "package demo\n\nfunc a() {}\n", Known: true}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
}

func TestParseShellWritesHeredocVariants(t *testing.T) {
	cases := map[string]string{
		"unquoted delimiter":          "cat > f.txt <<EOF\nhello\nEOF",
		"double-quoted delimiter":     "cat > f.txt <<\"EOF\"\nhello\nEOF",
		"redirect before heredoc":     "cat <<'EOF' > f.txt\nhello\nEOF",
		"no space after redirect":     "cat >f.txt <<'EOF'\nhello\nEOF",
		"fd 1 prefix":                 "cat 1> f.txt <<'EOF'\nhello\nEOF",
		"tee":                         "cat <<'EOF' | tee f.txt\nhello\nEOF",
		"tee with stderr dup":         "cat <<'EOF' 2>&1 | tee f.txt >/dev/null\nhello\nEOF",
		"cd prefix stripped later":    "cat > f.txt <<'EOF' && echo done\nhello\nEOF",
		"trailing commands after":     "cat > f.txt <<'EOF'\nhello\nEOF\ngofmt -l f.txt",
		"tab-stripping <<-":           "cat > f.txt <<-'EOF'\n\thello\n\tEOF",
		"different delimiter word":    "cat > f.txt <<'PYEOF'\nhello\nPYEOF",
		"leading spaces on cat line":  "  cat > f.txt <<'EOF'\nhello\nEOF",
		"other heredoc first in line": "python3 - <<'PY' && cat > f.txt <<'EOF'\nprint(1)\nPY\nhello\nEOF",
	}
	for name, cmd := range cases {
		t.Run(name, func(t *testing.T) {
			got := ParseShellWrites(cmd)
			if len(got) != 1 {
				t.Fatalf("want exactly one write, got %#v", got)
			}
			if got[0].FilePath != "f.txt" || !got[0].Known || got[0].Content != "hello\n" {
				t.Fatalf("got %#v", got[0])
			}
		})
	}
}

func TestParseShellWritesAppendAndLiterals(t *testing.T) {
	cases := []struct {
		cmd  string
		want ShellWrite
	}{
		{"cat >> notes.md <<'EOF'\nmore\nEOF", ShellWrite{FilePath: "notes.md", Content: "more\n", Append: true, Known: true}},
		{"echo 'hello world' > f.txt", ShellWrite{FilePath: "f.txt", Content: "hello world\n", Known: true}},
		{"echo hello world >> f.txt", ShellWrite{FilePath: "f.txt", Content: "hello world\n", Append: true, Known: true}},
		{"echo -n hi > f.txt", ShellWrite{FilePath: "f.txt", Content: "hi", Known: true}},
		{"printf 'a\\nb\\n' > f.txt", ShellWrite{FilePath: "f.txt", Content: "a\nb\n", Known: true}},
		{"printf 'x %s\\n' y > f.txt", ShellWrite{FilePath: "f.txt"}},
		{"cat > f.txt <<< \"one line\"", ShellWrite{FilePath: "f.txt", Content: "one line\n", Known: true}},
		{"echo hi | tee -a f.txt", ShellWrite{FilePath: "f.txt", Content: "hi\n", Append: true, Known: true}},
		{"go run ./gen > out.go", ShellWrite{FilePath: "out.go"}},
		{"cd demo && cat > f.txt <<'EOF'\nhello\nEOF", ShellWrite{FilePath: "demo/f.txt", Content: "hello\n", Known: true}},
		{"sed -i '' 's/a/b/' pkg/x.go", ShellWrite{FilePath: "pkg/x.go"}},
		{"sed -i 's/a/b/' pkg/x.go pkg/y.go", ShellWrite{FilePath: "pkg/x.go"}},
		{"sed -i.bak -e 's/a/b/' pkg/x.go", ShellWrite{FilePath: "pkg/x.go"}},
	}
	for _, c := range cases {
		got := ParseShellWrites(c.cmd)
		if len(got) == 0 || got[0] != c.want {
			t.Errorf("%q\n got %#v\nwant %#v", c.cmd, got, c.want)
		}
	}
}

func TestParseShellWritesIgnoresNonFiles(t *testing.T) {
	cases := []string{
		"go test ./... > /dev/null 2>&1",
		"ls 2> err.txt",
		"make >&2",
		"grep -r foo . | head",
		"cat > \"$OUT\" <<'EOF'\nx\nEOF",
		"cat > ~/notes.txt <<'EOF'\nx\nEOF",
		"python3 - <<'PY'\nopen('f.txt','w').write('x')\nPY",
		"sed 's/a/b/' f.txt",
		"cat f.txt",
		"# cat > f.txt <<'EOF'",
	}
	for _, cmd := range cases {
		if got := ParseShellWrites(cmd); len(got) != 0 {
			t.Errorf("%q should produce no writes, got %#v", cmd, got)
		}
	}
	// A dollar in the echo literal defeats content recovery but the write
	// itself is still known to have happened.
	got := ParseShellWrites("echo \"$HOME\" > f.txt")
	if len(got) != 1 || got[0].Known || got[0].FilePath != "f.txt" {
		t.Errorf("expanded literal should be a touched-only write, got %#v", got)
	}
}

func TestParseShellWritesMultipleHeredocsAndTargets(t *testing.T) {
	cmd := "cat > a.txt <<'EOF'\nA\nEOF\ncat > b.txt <<'EOF'\nB\nB2\nEOF"
	got := ParseShellWrites(cmd)
	want := []ShellWrite{
		{FilePath: "a.txt", Content: "A\n", Known: true},
		{FilePath: "b.txt", Content: "B\nB2\n", Known: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
}

func TestExtractShellWriteLocatesLastOccurrence(t *testing.T) {
	post := "x\nhello\ny\nhello\n"
	out := Extract("ShellWrite", ToolInput{Content: "hello\n"}, post)
	if len(out.Ranges) != 1 || out.Ranges[0].StartLine != 4 || out.Ranges[0].EndLine != 4 {
		t.Fatalf("got %#v", out)
	}
	whole := Extract("ShellWrite", ToolInput{Content: post}, post)
	if len(whole.Ranges) != 1 || whole.Ranges[0].StartLine != 1 || whole.Ranges[0].EndLine != 4 {
		t.Fatalf("whole-file write: got %#v", whole)
	}
	if miss := Extract("ShellWrite", ToolInput{Content: "nope\n"}, post); miss.Unlocated != 1 {
		t.Fatalf("unlocated: got %#v", miss)
	}
}
