package actors

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

// B14 (design 008): automatic worktree isolation — `isolation: worktree`
// frontmatter and `##agent spawn[-all] --worktree` provision a per-agent git
// worktree instead of leaving isolation a manual `##worktree new` + `cwd`
// two-step.

func writeIsolationSkill(t *testing.T, dir, frontmatter, body string) string {
	t.Helper()
	path := filepath.Join(dir, "SKILL.md")
	content := "---\n" + frontmatter + "\n---\n" + body + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestSkillFileParsesIsolation: the frontmatter field round-trips, "none"
// normalises to empty, and a typo fails LOUDLY — an agent silently running on
// the shared checkout is exactly what isolation was asked to prevent.
func TestSkillFileParsesIsolation(t *testing.T) {
	dir := t.TempDir()

	def, err := parseSkillFile(writeIsolationSkill(t, dir, "name: fixer\nisolation: worktree", "You fix."), nil)
	if err != nil {
		t.Fatal(err)
	}
	if def.Isolation != "worktree" {
		t.Fatalf("Isolation = %q, want worktree", def.Isolation)
	}

	def, err = parseSkillFile(writeIsolationSkill(t, dir, "name: fixer\nisolation: none", "You fix."), nil)
	if err != nil || def.Isolation != "" {
		t.Fatalf("isolation none => (%q, %v), want normalised empty", def.Isolation, err)
	}

	if _, err = parseSkillFile(writeIsolationSkill(t, dir, "name: fixer\nisolation: worktrees", "You fix."), nil); err == nil {
		t.Fatal("typo'd isolation value must be a loud parse error, not silent shared-checkout")
	}
}

// TestIsolateAgentWorktreeCreatesAndReuses: first spawn provisions
// .rysh/worktrees/agent-<name> on branch agent/<name>; a re-spawn (after a
// skill edit) reuses it instead of failing on the existing branch.
func TestIsolateAgentWorktreeCreatesAndReuses(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root, _ := gitRepoWithWorktree(t, "unrelated") // repo + one unrelated worktree
	w := &WorkspaceActor{cfg: config.Config{WorkingDirectory: root}}

	var out strings.Builder
	branch, path := w.isolateAgentWorktree(&out, "", "fixer", false)
	if branch != "agent/fixer" {
		t.Fatalf("branch = %q, want agent/fixer", branch)
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		t.Fatalf("worktree dir not created at %s: %v", path, err)
	}
	if !strings.Contains(path, filepath.Join(".rysh", "worktrees")) {
		t.Fatalf("worktree not under .rysh/worktrees: %s", path)
	}
	if !strings.Contains(out.String(), "created worktree") {
		t.Fatalf("output missing creation report: %s", out.String())
	}

	out.Reset()
	branch2, path2 := w.isolateAgentWorktree(&out, "", "fixer", false)
	if branch2 != branch || path2 != path {
		t.Fatalf("re-spawn changed the worktree: (%q,%q) vs (%q,%q)", branch2, path2, branch, path)
	}
	if !strings.Contains(out.String(), "reusing worktree") {
		t.Fatalf("re-spawn must reuse, got: %s", out.String())
	}
}

// TestIsolateAgentWorktreeSkipsOutsideGit: isolation degrades loudly to the
// shared checkout outside a git repo — the spawn itself must not fail.
func TestIsolateAgentWorktreeSkipsOutsideGit(t *testing.T) {
	w := &WorkspaceActor{cfg: config.Config{WorkingDirectory: t.TempDir()}}
	var out strings.Builder
	branch, path := w.isolateAgentWorktree(&out, "", "fixer", false)
	if branch != "" || path != "" {
		t.Fatalf("non-git isolation returned (%q,%q), want empty", branch, path)
	}
	if !strings.Contains(out.String(), "SKIPPED") {
		t.Fatalf("skip must be reported loudly, got: %s", out.String())
	}
}

// TestStripWorktreeFlag: flag extraction preserves the remaining args.
func TestStripWorktreeFlag(t *testing.T) {
	rest, found := stripWorktreeFlag([]string{"fixer", "--worktree"})
	if !found || len(rest) != 1 || rest[0] != "fixer" {
		t.Fatalf("got (%v,%v)", rest, found)
	}
	rest, found = stripWorktreeFlag([]string{"./agents"})
	if found || len(rest) != 1 {
		t.Fatalf("got (%v,%v)", rest, found)
	}
}
