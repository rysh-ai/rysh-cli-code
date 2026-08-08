package tui

import (
	"os"
	"path/filepath"
	"testing"
)

// TestShellCompletions_Path covers argument-position path completion via the
// exported facade (roadmap W7): candidates match the token, directories are
// flagged, dotfiles stay hidden unless typed.
func TestShellCompletions_Path(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"alpha.txt", "alt.md", ".hidden"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "aldir"), 0o755); err != nil {
		t.Fatal(err)
	}

	cands := ShellCompletions(ShellCompletionRequest{
		Token: "al", Line: "cat al", Cwd: dir, IsFirstToken: false,
	})
	got := map[string]bool{}
	for _, c := range cands {
		got[c.Value] = c.IsDir
	}
	if len(got) != 3 {
		t.Fatalf("candidates = %v, want alpha.txt/alt.md/aldir", got)
	}
	if isDir, ok := got["aldir"]; !ok || !isDir {
		t.Errorf("aldir missing or not marked as dir: %v", got)
	}
	if isDir, ok := got["alpha.txt"]; !ok || isDir {
		t.Errorf("alpha.txt missing or wrongly a dir: %v", got)
	}
	if _, ok := got[".hidden"]; ok {
		t.Errorf("dotfile surfaced without a leading-dot token: %v", got)
	}
}

// TestShellCompletions_CommandPosition: a first token with no path separator
// completes command names (builtins guaranteed present).
func TestShellCompletions_CommandPosition(t *testing.T) {
	cands := ShellCompletions(ShellCompletionRequest{
		Token: "expor", Line: "expor", IsFirstToken: true,
	})
	found := false
	for _, c := range cands {
		if c.Value == "export" {
			found = true
		}
		if c.IsDir {
			t.Errorf("command candidate %q marked as dir", c.Value)
		}
	}
	if !found {
		t.Fatalf("command completion for 'expor' missing 'export': %v", cands)
	}
}

// TestShellCompletions_FirstTokenPath: a path-ish first token ("./x") is
// completed as a path, not a command.
func TestShellCompletions_FirstTokenPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	cands := ShellCompletions(ShellCompletionRequest{
		Token: "./ru", Line: "./ru", Cwd: dir, IsFirstToken: true,
	})
	if len(cands) != 1 || cands[0].Value != "./run.sh" {
		t.Fatalf("candidates = %v, want ./run.sh", cands)
	}
}

// TestResolveCompletionCwd prefers the reported cwd and falls back sanely.
func TestResolveCompletionCwd(t *testing.T) {
	if got := resolveCompletionCwd("/explicit", 0); got != "/explicit" {
		t.Errorf("reported cwd ignored: %q", got)
	}
	if got := resolveCompletionCwd("", -1); got == "" {
		t.Errorf("fallback cwd empty")
	}
}
