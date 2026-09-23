package lineranges

import (
	"strings"
)

// ShellWrite is one file an agent's shell command wrote to. Claude Code in
// auto mode (and agents told to "use the shell") rewrite files with
//
//	cat > path/to/file.go <<'EOF'
//	...
//	EOF
//
// instead of the Edit/Write tools, so without this the edit is invisible to
// attribution. Content is the exact text the command wrote when the parser
// could recover it (heredoc body, here-string, echo/printf literal); it is
// empty — and Known false — for writes whose content comes from another
// program (`sed -i`, `go run gen > out.go`), where only the fact that the
// file was touched is known.
type ShellWrite struct {
	FilePath string
	Content  string
	Append   bool
	Known    bool
}

// ParseShellWrites finds the files a shell command writes to. It understands
// `>` / `>>` redirects (with optional fd prefix, `&>`), `tee [-a]`, heredocs
// (`<<`, `<<-`, quoted or not), here-strings, echo/printf literals, `sed -i`
// targets and a leading `cd dir &&`. Redirects to /dev/*, stderr-only
// redirects, fd duplications and paths containing unexpanded `$` are ignored.
func ParseShellWrites(command string) []ShellWrite {
	lines := strings.Split(strings.ReplaceAll(command, "\r\n", "\n"), "\n")
	var out []ShellWrite
	cwdPrefix := ""

	for i := 0; i < len(lines); i++ {
		toks := tokenize(lines[i])
		if len(toks) == 0 {
			continue
		}
		// Split into pipelines on ; && || &, keeping | inside a pipeline.
		var pipelines [][]token
		var cur []token
		for _, t := range toks {
			if t.op && (t.text == ";" || t.text == "&&" || t.text == "||" || t.text == "&") {
				if len(cur) > 0 {
					pipelines = append(pipelines, cur)
				}
				cur = nil
				continue
			}
			cur = append(cur, t)
		}
		if len(cur) > 0 {
			pipelines = append(pipelines, cur)
		}

		// Heredoc bodies follow the whole line, in operator order.
		var pending []heredocRef
		for _, pl := range pipelines {
			writes, heredocs := parsePipeline(pl, &cwdPrefix)
			base := len(out)
			out = append(out, writes...)
			for _, h := range heredocs {
				if h.feedsWrites {
					for k := range writes {
						if writes[k].Known {
							h.writeIdx = append(h.writeIdx, base+k)
						}
					}
				}
				pending = append(pending, h)
			}
		}
		for _, h := range pending {
			stripTabs := strings.HasPrefix(h.spec, "-")
			delim := strings.TrimPrefix(h.spec, "-")
			var b strings.Builder
			j := i + 1
			for ; j < len(lines); j++ {
				l := lines[j]
				if stripTabs {
					l = strings.TrimLeft(l, "\t")
				}
				if l == delim {
					break
				}
				b.WriteString(l)
				b.WriteByte('\n')
			}
			i = j
			for _, idx := range h.writeIdx {
				out[idx].Content = b.String()
			}
		}
	}

	res := out[:0]
	for _, w := range out {
		if w.FilePath == "" {
			continue
		}
		res = append(res, w)
	}
	return res
}

// heredocRef is a heredoc operator seen on a command line whose body still
// has to be read from the following lines. feedsWrites marks the heredoc
// whose body is the content of that pipeline's writes.
type heredocRef struct {
	spec        string // delimiter, with a "-" prefix for <<-
	feedsWrites bool
	writeIdx    []int
}

type token struct {
	text    string
	quoted  bool // any part of the word was quoted
	expands bool // contains $ or ` outside single quotes: the shell would expand it
	op      bool // shell operator (| || && ; & > >> < << <<- <<< &> ...)
}

// tokenize splits one command line into words and operators, honouring
// single quotes, double quotes and backslash escapes.
func tokenize(line string) []token {
	var toks []token
	var cur strings.Builder
	quoted := false
	expands := false
	inWord := false
	flush := func() {
		if inWord {
			toks = append(toks, token{text: cur.String(), quoted: quoted, expands: expands})
			cur.Reset()
			quoted = false
			expands = false
			inWord = false
		}
	}
	emitOp := func(s string) {
		flush()
		toks = append(toks, token{text: s, op: true})
	}
	n := len(line)
	for i := 0; i < n; i++ {
		c := line[i]
		switch {
		case c == ' ' || c == '\t':
			flush()
		case c == '#' && !inWord:
			return toks
		case c == '\'':
			inWord, quoted = true, true
			j := strings.IndexByte(line[i+1:], '\'')
			if j == -1 {
				cur.WriteString(line[i+1:])
				i = n
			} else {
				cur.WriteString(line[i+1 : i+1+j])
				i = i + 1 + j
			}
		case c == '"':
			inWord, quoted = true, true
			i++
			for ; i < n && line[i] != '"'; i++ {
				if line[i] == '\\' && i+1 < n && strings.IndexByte("\"\\$`", line[i+1]) != -1 {
					i++
				} else if line[i] == '$' || line[i] == '`' {
					expands = true
				}
				cur.WriteByte(line[i])
			}
		case c == '\\' && i+1 < n:
			inWord = true
			i++
			cur.WriteByte(line[i])
		case c == '|':
			if i+1 < n && line[i+1] == '|' {
				emitOp("||")
				i++
			} else {
				emitOp("|")
			}
		case c == ';':
			emitOp(";")
		case c == '&':
			if i+1 < n && line[i+1] == '&' {
				emitOp("&&")
				i++
			} else if i+1 < n && line[i+1] == '>' {
				// &> or &>>
				if i+2 < n && line[i+2] == '>' {
					emitOp("&>>")
					i += 2
				} else {
					emitOp("&>")
					i++
				}
			} else {
				emitOp("&")
			}
		case c == '>' || c == '<':
			// An all-digit word directly before the operator is an fd prefix.
			prefix := ""
			if inWord && !quoted && isDigits(cur.String()) {
				prefix = cur.String()
				cur.Reset()
				inWord = false
			}
			op := string(c)
			if c == '>' {
				if i+1 < n && line[i+1] == '>' {
					op = ">>"
					i++
				}
				if i+1 < n && line[i+1] == '&' {
					// >&N or >&- : fd duplication, no file.
					op += "&"
					i++
					for i+1 < n && (isDigit(line[i+1]) || line[i+1] == '-') {
						i++
					}
				}
			} else {
				if i+2 < n && line[i+1] == '<' && line[i+2] == '<' {
					op = "<<<"
					i += 2
				} else if i+1 < n && line[i+1] == '<' {
					op = "<<"
					i++
					if i+1 < n && line[i+1] == '-' {
						op = "<<-"
						i++
					}
				}
			}
			emitOp(prefix + op)
		default:
			inWord = true
			if c == '$' || c == '`' {
				expands = true
			}
			cur.WriteByte(c)
		}
	}
	flush()
	return toks
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	return true
}

// parsePipeline extracts writes from one pipeline. Heredoc bodies are not
// on this line, so heredocs come back as refs for the caller to fill; when
// the pipeline's content comes from its first heredoc, the writes returned
// are marked Known and the caller copies that body into them.
func parsePipeline(toks []token, cwdPrefix *string) ([]ShellWrite, []heredocRef) {
	type segment struct {
		words    []token
		targets  []ShellWrite
		heredocs []string // delimiter specs, "-" prefix for <<-
		herestr  *string
	}
	var segs []segment
	cur := segment{}
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if t.op && t.text == "|" {
			segs = append(segs, cur)
			cur = segment{}
			continue
		}
		if !t.op {
			cur.words = append(cur.words, t)
			continue
		}
		next := func() (token, bool) {
			if i+1 < len(toks) && !toks[i+1].op {
				i++
				return toks[i], true
			}
			return token{}, false
		}
		switch {
		case strings.HasPrefix(t.text, "<<<"):
			if w, ok := next(); ok {
				s := w.text + "\n"
				cur.herestr = &s
			}
		case strings.HasPrefix(t.text, "<<"):
			if w, ok := next(); ok {
				spec := w.text
				if t.text == "<<-" {
					spec = "-" + spec
				}
				cur.heredocs = append(cur.heredocs, spec)
			}
		case strings.HasPrefix(t.text, "<"):
			next() // input redirect: consume the source, not a write
		default:
			// Output redirect: [fd]> [fd]>> &> &>> or a dup (contains &
			// after >).
			op := t.text
			fd := "1"
			both := false
			if strings.HasPrefix(op, "&>") {
				both = true
				op = strings.TrimPrefix(op, "&")
			} else {
				k := 0
				for k < len(op) && isDigit(op[k]) {
					k++
				}
				if k > 0 {
					fd = op[:k]
					op = op[k:]
				}
			}
			if strings.Contains(op, "&") {
				continue // >&2, 2>&1 …
			}
			w, ok := next()
			if !ok {
				continue
			}
			if !both && fd != "1" {
				continue // stderr or other fd
			}
			cur.targets = append(cur.targets, ShellWrite{FilePath: w.text, Append: strings.HasPrefix(op, ">>")})
		}
	}
	segs = append(segs, cur)

	// `cd dir` as the whole pipeline: later relative paths on this command
	// line live under dir.
	if len(segs) == 1 && len(segs[0].words) == 2 && segs[0].words[0].text == "cd" && len(segs[0].targets) == 0 {
		dir := segs[0].words[1].text
		if !strings.ContainsAny(dir, "$`") {
			if strings.HasPrefix(dir, "/") || *cwdPrefix == "" {
				*cwdPrefix = dir
			} else {
				*cwdPrefix = strings.TrimSuffix(*cwdPrefix, "/") + "/" + dir
			}
		}
		return nil, nil
	}

	// Gather content sources and targets across the pipeline. The first
	// heredoc wins over a here-string or echo/printf literal.
	var heredocs []heredocRef
	content := ""
	haveContent := false
	contentFromHeredoc := false
	var targets []ShellWrite
	for _, s := range segs {
		for _, spec := range s.heredocs {
			heredocs = append(heredocs, heredocRef{spec: spec})
			if !haveContent {
				haveContent = true
				contentFromHeredoc = true
			}
		}
		if s.herestr != nil && !haveContent {
			content, haveContent = *s.herestr, true
		}
		if lit, ok := literalOutput(s.words); ok && !haveContent {
			content, haveContent = lit, true
		}
		if len(s.words) > 0 && s.words[0].text == "tee" {
			appendMode := false
			for _, w := range s.words[1:] {
				if strings.HasPrefix(w.text, "-") && !w.quoted {
					if w.text == "-a" || w.text == "--append" {
						appendMode = true
					}
					continue
				}
				targets = append(targets, ShellWrite{FilePath: w.text, Append: appendMode})
			}
		}
		if len(s.words) > 0 && s.words[0].text == "sed" {
			targets = append(targets, sedInPlaceTargets(s.words[1:])...)
		}
		targets = append(targets, s.targets...)
	}
	if contentFromHeredoc && len(heredocs) > 0 {
		heredocs[0].feedsWrites = true
	}

	var writes []ShellWrite
	for _, w := range targets {
		if !plausibleTarget(w.FilePath) {
			continue
		}
		if *cwdPrefix != "" && !strings.HasPrefix(w.FilePath, "/") {
			w.FilePath = strings.TrimSuffix(*cwdPrefix, "/") + "/" + w.FilePath
		}
		if haveContent {
			w.Known = true
			w.Content = content // empty for a heredoc until the caller fills it
		}
		writes = append(writes, w)
	}
	return writes, heredocs
}

// literalOutput returns what an echo/printf command prints, when that can be
// known without running a shell: echo with plain words, printf with a format
// string containing no % directives (other than %%) and no further args.
func literalOutput(words []token) (string, bool) {
	if len(words) == 0 {
		return "", false
	}
	switch words[0].text {
	case "echo":
		newline := true
		interpret := false
		args := words[1:]
		for len(args) > 0 && !args[0].quoted && strings.HasPrefix(args[0].text, "-") && len(args[0].text) > 1 && strings.Trim(args[0].text, "-neE") == "" {
			if strings.Contains(args[0].text, "n") {
				newline = false
			}
			if strings.Contains(args[0].text, "e") {
				interpret = true
			}
			args = args[1:]
		}
		var parts []string
		for _, a := range args {
			if a.expands || (!a.quoted && strings.ContainsAny(a.text, "*?")) {
				return "", false
			}
			parts = append(parts, a.text)
		}
		s := strings.Join(parts, " ")
		if interpret {
			s = unescapeC(s)
		}
		if newline {
			s += "\n"
		}
		return s, true
	case "printf":
		if len(words) != 2 {
			return "", false
		}
		f := words[1].text
		if strings.Contains(strings.ReplaceAll(f, "%%", ""), "%") {
			return "", false
		}
		if words[1].expands {
			return "", false
		}
		return unescapeC(strings.ReplaceAll(f, "%%", "%")), true
	}
	return "", false
}

func unescapeC(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case '\\':
			b.WriteByte('\\')
		case '"':
			b.WriteByte('"')
		case '\'':
			b.WriteByte('\'')
		default:
			b.WriteByte('\\')
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// sedInPlaceTargets returns the files a `sed -i …` invocation edits, or
// nothing when -i is absent.
func sedInPlaceTargets(args []token) []ShellWrite {
	inPlace := false
	scriptSeen := false
	var files []ShellWrite
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !a.quoted && strings.HasPrefix(a.text, "-") && len(a.text) > 1 {
			switch {
			case a.text == "-i" || a.text == "--in-place":
				inPlace = true
				// BSD sed takes the backup suffix as a separate arg; an
				// empty quoted one (`-i ''`) is the common macOS form.
				if i+1 < len(args) && args[i+1].quoted && args[i+1].text == "" {
					i++
				}
			case strings.HasPrefix(a.text, "-i") || strings.HasPrefix(a.text, "--in-place="):
				inPlace = true
			case a.text == "-e" || a.text == "--expression" || a.text == "-f" || a.text == "--file":
				scriptSeen = true
				i++
			case strings.HasPrefix(a.text, "-e") || strings.HasPrefix(a.text, "--expression="):
				scriptSeen = true
			}
			continue
		}
		if !scriptSeen {
			scriptSeen = true // first positional is the script
			continue
		}
		files = append(files, ShellWrite{FilePath: a.text})
	}
	if !inPlace {
		return nil
	}
	return files
}

func plausibleTarget(p string) bool {
	if p == "" || p == "-" {
		return false
	}
	if strings.HasPrefix(p, "/dev/") || strings.HasPrefix(p, "/proc/") {
		return false
	}
	if strings.ContainsAny(p, "$`*?~") {
		return false
	}
	return true
}
