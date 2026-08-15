// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPathMatch(t *testing.T) {
	cases := []struct {
		pat, s string
		want   bool
	}{
		{"src/**/*.go", "src/a/b/main.go", true},
		{"src/**/*.go", "src/main.go", true}, // ** matches zero segments
		{"src/*.go", "src/a/main.go", false}, // * doesn't cross /
		{"*.md", "README.md", true},
		{"*.md", "docs/README.md", false},
		{"src/**", "secret.env", false},
	}
	for _, c := range cases {
		if got := pathMatch(c.pat, c.s); got != c.want {
			t.Errorf("pathMatch(%q,%q)=%v want %v", c.pat, c.s, got, c.want)
		}
	}
}

func TestCmdMatch(t *testing.T) {
	cases := []struct {
		pat, s string
		want   bool
	}{
		{"go test *", "go test ./...", true}, // * crosses / for commands
		{"rm -rf *", "rm -rf /tmp", true},
		{"git push*", "git push --force", true},
		{"go build *", "go test ./...", false},
	}
	for _, c := range cases {
		if got := cmdMatch(c.pat, c.s); got != c.want {
			t.Errorf("cmdMatch(%q,%q)=%v want %v", c.pat, c.s, got, c.want)
		}
	}
}

func TestEvaluate_AllPass(t *testing.T) {
	e := Expect{
		FilesChanged:      []string{"src/**/*.go"},
		CommandsAllowed:   []string{"go test *", "go build *"},
		CommandsForbidden: []string{"rm -rf *"},
		OutputMatches:     []string{"PASS"},
		MaxTokens:         100000,
	}
	r := Result{
		FilesChanged: []string{"src/a/main.go"},
		Commands:     []string{"go test ./...", "go build ./..."},
		Output:       "... ok PASS ...",
		TokensUsed:   42000,
	}
	as := Evaluate(e, r)
	if !Passed(as) {
		for _, a := range as {
			if !a.Pass {
				t.Errorf("failed: %s — %s", a.Name, a.Detail)
			}
		}
	}
}

func TestEvaluate_Failures(t *testing.T) {
	e := Expect{
		FilesChanged:      []string{"src/**"},
		CommandsForbidden: []string{"git push*"},
		OutputMatches:     []string{"NEVER_APPEARS"},
		MaxTokens:         1000,
	}
	r := Result{
		FilesChanged: []string{"secret.env"},       // out of scope
		Commands:     []string{"git push --force"}, // forbidden
		Output:       "nope",                       // regex won't match
		TokensUsed:   5000,                         // over budget
	}
	as := Evaluate(e, r)
	if Passed(as) {
		t.Fatal("expected failures")
	}
	fails := map[string]bool{}
	for _, a := range as {
		if !a.Pass {
			fails[a.Name] = true
		}
	}
	for _, want := range []string{"files_changed in scope", "commands_forbidden", "max_tokens"} {
		if !fails[want] {
			t.Errorf("expected %q to fail", want)
		}
	}
}

func TestLoadCase(t *testing.T) {
	dir := t.TempDir()
	task := "---\nuntil: tests pass\n---\nFix the failing test in foo.go.\n"
	if err := os.WriteFile(filepath.Join(dir, "task.md"), []byte(task), 0o644); err != nil {
		t.Fatal(err)
	}
	exp := "files_changed: [\"**/*.go\"]\nmax_tokens: 200000\noutput_matches: [\"done\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "expect.yaml"), []byte(exp), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCase(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Until != "tests pass" {
		t.Fatalf("until = %q", c.Until)
	}
	if c.Prompt != "Fix the failing test in foo.go." {
		t.Fatalf("prompt = %q", c.Prompt)
	}
	if c.Expect.MaxTokens != 200000 || len(c.Expect.FilesChanged) != 1 {
		t.Fatalf("expect = %+v", c.Expect)
	}
}
