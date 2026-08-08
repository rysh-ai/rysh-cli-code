package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/script"
)

// TestShippedExamplesAreValid keeps the examples honest. They are the first
// thing anyone copies, so a .rysh file we ship that does not satisfy its own
// contract is worse than no example at all.
//
// Both halves are checked: it must transpile, and it must be valid bash with
// the ## lines inert — which is the property that breaks silently the moment
// someone writes a ## line as the only statement of an if or for body.
func TestShippedExamplesAreValid(t *testing.T) {
	matches, err := filepath.Glob(filepath.Join("..", "..", "examples", "scripts", "*.rysh"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Skip("no example scripts found")
	}

	haveBash := true
	if _, err := exec.LookPath("bash"); err != nil {
		haveBash = false
	}

	for _, path := range matches {
		t.Run(filepath.Base(path), func(t *testing.T) {
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			result, err := script.Transpile(string(src))
			if err != nil {
				t.Fatalf("does not transpile: %v", err)
			}
			if len(result.RyshLines) == 0 {
				t.Error("example contains no ## commands — it is not demonstrating anything")
			}
			if !haveBash {
				t.Skip("bash not available for the polyglot half")
			}
			if err := runBashParse(string(src)); err != nil {
				t.Errorf("example is not valid bash (polyglot property broken): %v", err)
			}
			if err := runBashParse(result.Bash); err != nil {
				t.Errorf("transpiled example is not valid bash: %v", err)
			}
		})
	}
}

// TestShippedExamplesRunUnderPlainBash goes one step further than parsing: the
// examples must actually RUN under bash with the rysh work skipped, which is
// what "the same file works both ways" claims.
func TestShippedExamplesRunUnderPlainBash(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	matches, _ := filepath.Glob(filepath.Join("..", "..", "examples", "scripts", "*.rysh"))
	for _, path := range matches {
		abs, err := filepath.Abs(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Run(filepath.Base(path), func(t *testing.T) {
			cmd := exec.Command("bash", abs)
			// Run somewhere disposable: an example is allowed to write files,
			// and none of them should land in the source tree.
			cmd.Dir = t.TempDir()
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Errorf("example failed under plain bash: %v\n%s", err, out)
			}
		})
	}
}

func runBashParse(src string) error {
	cmd := exec.Command("bash", "-n")
	cmd.Stdin = strings.NewReader(src)
	if out, err := cmd.CombinedOutput(); err != nil {
		return &parseErr{strings.TrimSpace(string(out))}
	}
	return nil
}

type parseErr struct{ msg string }

func (e *parseErr) Error() string { return e.msg }

// TestCountLines pins the line count --check reports.
func TestCountLines(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int
	}{
		{"", 0},
		{"a\n", 1},
		{"a\nb\n", 2},
		{"a\nb", 1},
	} {
		if got := countLines(c.in); got != c.want {
			t.Errorf("countLines(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}
