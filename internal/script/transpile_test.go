package script

import (
	"os/exec"
	"strings"
	"testing"
)

// TestTranspile_Golden pins the rewrite for every shape the scanner has to get
// right. Each case is source -> expected bash, compared line for line.
func TestTranspile_Golden(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "bare command",
			src:  "##pane info",
			want: `__rysh "##pane info"`,
		},
		{
			name: "indent is preserved",
			src:  "\t  ##pane info",
			want: "\t  __rysh \"##pane info\"",
		},
		{
			// The one-space escape hatch: a comment that looks like a command.
			name: "space after ## is a comment",
			src:  "## pane info",
			want: "## pane info",
		},
		{
			name: "three hashes is a comment",
			src:  "### pane info",
			want: "### pane info",
		},
		{
			name: "not in statement position",
			src:  `echo ##pane info`,
			want: `echo ##pane info`,
		},
		{
			name: "colon form",
			src:  "  : ##pane info",
			want: `  __rysh "##pane info"`,
		},
		{
			name: "colon without a space is not the colon form",
			src:  ":##pane info",
			want: ":##pane info",
		},
		{
			name: "or-else tail",
			src:  "##agent run x.md || exit 1",
			want: `__rysh "##agent run x.md" || exit 1`,
		},
		{
			name: "and-then tail",
			src:  "##tab list && echo ok",
			want: `__rysh "##tab list" && echo ok`,
		},
		{
			name: "pipe tail",
			src:  "##pane list | grep -c idle",
			want: `__rysh "##pane list" | grep -c idle`,
		},
		{
			name: "redirect tail",
			src:  "##pane info > state.txt",
			want: `__rysh "##pane info" > state.txt`,
		},
		{
			name: "append redirect tail",
			src:  "##pane info >> log.txt",
			want: `__rysh "##pane info" >> log.txt`,
		},
		{
			name: "semicolon tail",
			src:  "##tab list ; echo done",
			want: `__rysh "##tab list" ; echo done`,
		},
		{
			name: "background tail",
			src:  "##agent run x.md &",
			want: `__rysh "##agent run x.md" &`,
		},
		{
			name: "trailing comment",
			src:  "##pane info   # snapshot",
			want: `__rysh "##pane info" # snapshot`,
		},
		{
			// An operator inside quotes belongs to the rysh command, not bash.
			name: "operator inside double quotes is not a split point",
			src:  `##cmd pane echo "a | b"`,
			want: `__rysh "##cmd pane echo "a | b""`,
		},
		{
			name: "operator inside single quotes is not a split point",
			src:  `##cmd pane echo 'a && b'`,
			want: `__rysh "##cmd pane echo 'a && b'"`,
		},
		{
			name: "escaped operator is not a split point",
			src:  `##cmd pane echo a\|b`,
			want: `__rysh "##cmd pane echo a\|b"`,
		},
		{
			name: "variables are left for bash to expand",
			src:  `##llm select $MODEL`,
			want: `__rysh "##llm select $MODEL"`,
		},
		{
			// A '#' inside ${...} is part of the expansion, not a comment.
			// Splitting there emitted `__rysh "##new grid ${"` and bash died
			// on the unterminated quote.
			name: "array length expansion",
			src:  `##new grid ${#REPOS[@]}`,
			want: `__rysh "##new grid ${#REPOS[@]}"`,
		},
		{
			name: "prefix-strip expansion",
			src:  `##pane name ${V##*/}`,
			want: `__rysh "##pane name ${V##*/}"`,
		},
		{
			name: "default-value expansion",
			src:  `##pane name ${NAME:-fallback}`,
			want: `__rysh "##pane name ${NAME:-fallback}"`,
		},
		{
			// A pipe inside an expansion is not a pipe into bash.
			name: "pipe inside a command substitution",
			src:  `##pane name $(echo a | tr a b)`,
			want: `__rysh "##pane name $(echo a | tr a b)"`,
		},
		{
			// ...but a pipe AFTER the expansion still splits.
			name: "operator after an expansion still splits",
			src:  `##pane list ${FILTER} | grep -c idle`,
			want: `__rysh "##pane list ${FILTER}" | grep -c idle`,
		},
		{
			name: "nested expansion",
			src:  `##new grid ${#A[@]}${B:-}`,
			want: `__rysh "##new grid ${#A[@]}${B:-}"`,
		},
		{
			// ##prompt is a script builtin: it compiles to the blocking
			// `rysh prompt` verb, not to a dispatch-table call.
			name: "prompt builtin",
			src:  `##prompt refactor the parser`,
			want: `__rysh_prompt "refactor the parser"`,
		},
		{
			name: "prompt builtin keeps its tail",
			src:  `##prompt fix the build || exit 1`,
			want: `__rysh_prompt "fix the build" || exit 1`,
		},
		{
			// No text: not the builtin. It falls through to the table, whose
			// entry explains what to use instead.
			name: "bare prompt is not the builtin",
			src:  `##prompt`,
			want: `__rysh "##prompt"`,
		},
		{
			name: "promptfoo is not the prompt builtin",
			src:  `##promptfoo bar`,
			want: `__rysh "##promptfoo bar"`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Transpile(c.src)
			if err != nil {
				t.Fatalf("Transpile(%q) failed: %v", c.src, err)
			}
			if got.Bash != c.want {
				t.Errorf("Transpile(%q)\n got: %q\nwant: %q", c.src, got.Bash, c.want)
			}
		})
	}
}

// TestTranspile_HeredocsAreData is the scanner's most important guarantee.
// Rewriting a heredoc body would turn text the script means to WRITE into a
// command it RUNS — the worst thing this package could do.
func TestTranspile_HeredocsAreData(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{"plain", "cat <<EOF\n##pane info\nEOF\n"},
		{"quoted delimiter", "cat <<'EOF'\n##pane info\nEOF\n"},
		{"double-quoted delimiter", "cat <<\"EOF\"\n##pane info\nEOF\n"},
		{"tab-stripped", "cat <<-EOF\n\t##pane info\n\tEOF\n"},
		{"two on one line", "cat <<A <<B\n##pane info\nA\n##tab list\nB\n"},
		{"body resumes code after terminator", "cat <<EOF\n##pane info\nEOF\n##tab list\n"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Transpile(c.src)
			if err != nil {
				t.Fatalf("Transpile failed: %v", err)
			}
			// Every ## line inside a heredoc body must survive verbatim.
			if strings.Count(got.Bash, "##pane info") == 0 {
				t.Errorf("heredoc body was rewritten:\n%s", got.Bash)
			}
			for _, line := range strings.Split(got.Bash, "\n") {
				if strings.HasPrefix(line, RyshFunc+" \"##pane info\"") {
					t.Errorf("a heredoc body line was transpiled:\n%s", got.Bash)
				}
			}
		})
	}

	// ...and the line after the terminator must go back to being code.
	got, err := Transpile("cat <<EOF\n##pane info\nEOF\n##tab list\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Bash, `__rysh "##tab list"`) {
		t.Errorf("code after the heredoc terminator was not transpiled:\n%s", got.Bash)
	}
}

// TestTranspile_MultiLineQuotesAreData covers the other way a ## line can be
// text rather than a statement.
func TestTranspile_MultiLineQuotesAreData(t *testing.T) {
	src := "X='start\n##pane info\nend'\n##tab list\n"
	got, err := Transpile(src)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.Bash, `__rysh "##pane info"`) {
		t.Errorf("a line inside a multi-line string was transpiled:\n%s", got.Bash)
	}
	if !strings.Contains(got.Bash, `__rysh "##tab list"`) {
		t.Errorf("code after the string closed was not transpiled:\n%s", got.Bash)
	}
}

// TestTranspile_ContinuationIsData covers a backslash-continued command whose
// next line begins with ##.
func TestTranspile_ContinuationIsData(t *testing.T) {
	src := "echo one \\\n##pane info\n"
	got, err := Transpile(src)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.Bash, RyshFunc) {
		t.Errorf("a continuation line was transpiled:\n%s", got.Bash)
	}
}

// TestTranspile_LineFidelity is the invariant the whole debugging story rests
// on: bash's diagnostics only name the right line if input and output have the
// same number of lines, in the same order.
func TestTranspile_LineFidelity(t *testing.T) {
	srcs := []string{
		"##pane info\necho hi\n##tab list\n",
		"cat <<EOF\n##pane info\nEOF\n",
		"##pane info",             // no trailing newline
		"",                        // empty file
		"\n\n\n",                  // blank lines only
		"##a\n##b\n##c\n##d\n##e", // all rysh
	}
	for _, src := range srcs {
		got, err := Transpile(src)
		if err != nil {
			t.Fatalf("Transpile(%q) failed: %v", src, err)
		}
		in := strings.Count(src, "\n")
		out := strings.Count(got.Bash, "\n")
		if in != out {
			t.Errorf("line count changed for %q: %d newlines in, %d out", src, in, out)
		}
		if len(strings.Split(src, "\n")) != len(strings.Split(got.Bash, "\n")) {
			t.Errorf("line count changed for %q", src)
		}
	}
}

// TestTranspile_RyshLinesReported checks the reported line numbers, which
// --check prints and which a future editor integration would use.
func TestTranspile_RyshLinesReported(t *testing.T) {
	got, err := Transpile("echo a\n##pane info\necho b\n: ##tab list\n")
	if err != nil {
		t.Fatal(err)
	}
	want := []int{2, 4}
	if len(got.RyshLines) != len(want) {
		t.Fatalf("RyshLines = %v, want %v", got.RyshLines, want)
	}
	for i := range want {
		if got.RyshLines[i] != want[i] {
			t.Errorf("RyshLines = %v, want %v", got.RyshLines, want)
		}
	}
}

// TestTranspile_UnbalancedQuoteRejected checks that a body which would produce
// a bash syntax error is caught here instead, with the source line named.
func TestTranspile_UnbalancedQuoteRejected(t *testing.T) {
	for _, src := range []string{
		"echo ok\n##pane rename \"my pane\n",
		"##cmd pane echo 'unterminated\n",
	} {
		_, err := Transpile(src)
		if err == nil {
			t.Fatalf("Transpile(%q) succeeded; want an error naming the line", src)
		}
		var e *Error
		if !asScriptError(err, &e) {
			t.Fatalf("error %v is not a *script.Error", err)
		}
		if e.Line != 2 && e.Line != 1 {
			t.Errorf("error names line %d, want the offending line", e.Line)
		}
	}
}

func asScriptError(err error, target **Error) bool {
	e, ok := err.(*Error)
	if ok {
		*target = e
	}
	return ok
}

// TestTranspile_OutputIsValidBash runs bash's own parser over the result. A
// golden test only proves we emit what we intended; this proves what we
// intended is legal.
func TestTranspile_OutputIsValidBash(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	src := `set -euo pipefail
##new tab
if [[ "${RYSH_OUT:-}" == *idle* ]]; then
  : ##agent run reviewer.md || exit 1
fi
for r in a b; do
  : ##cmd lane git -C "$r" status
done
cat <<EOF
##pane info
EOF
##pane list | grep -c idle
`
	got, err := Transpile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := bashParses(got.Bash); err != nil {
		t.Errorf("transpiled output is not valid bash: %v\n%s", err, got.Bash)
	}

	// The polyglot half: the SOURCE must be valid bash too, with the ## lines
	// inert. This is the property that makes a .rysh file honest, and it is
	// checked by asking bash rather than by reasoning about it.
	if err := bashParses(src); err != nil {
		t.Errorf("source is not valid bash (polyglot property broken): %v", err)
	}
}

// TestTranspile_SoleBlockBodyNeedsColonForm documents, by test, the one place
// the bare ## form is not valid bash — and that the colon form fixes it.
func TestTranspile_SoleBlockBodyNeedsColonForm(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	bare := "if true; then\n##pane info\nfi\n"
	if err := bashParses(bare); err == nil {
		t.Error("expected plain bash to reject a ## line as an if-body's only statement")
	}
	colon := "if true; then\n: ##pane info\nfi\n"
	if err := bashParses(colon); err != nil {
		t.Errorf("colon form should be valid bash, got: %v", err)
	}
	// Both still transpile to the same command.
	a, _ := Transpile(bare)
	b, _ := Transpile(colon)
	if !strings.Contains(a.Bash, `__rysh "##pane info"`) || !strings.Contains(b.Bash, `__rysh "##pane info"`) {
		t.Errorf("the two forms should transpile alike:\n%s\n---\n%s", a.Bash, b.Bash)
	}
}

func bashParses(src string) error {
	cmd := exec.Command("bash", "-n")
	cmd.Stdin = strings.NewReader(src)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return &Error{Msg: strings.TrimSpace(string(out))}
	}
	return nil
}
