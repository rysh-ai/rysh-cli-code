// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/worktree"
)

// Design 008 regression: `##worktree` created a directory on disk that nothing
// ever ran in. `worktree.Add` worked, `##worktree list` listed it, and every
// pane still started in the main checkout — so "worktree-per-agent" was a git
// wrapper, not isolation. These tests pin the two halves of the fix:
//
//  1. the pane group's config is what createPane hands to a new PaneActor as its
//     shell cwd, and MsgSetWorkingDir is what moves it;
//  2. `##worktree cwd` refuses a branch that has no worktree, rather than
//     silently pointing panes at a path that does not exist.

func gitRepoWithWorktree(t *testing.T, branch string) (root, wtPath string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root = t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "t@example.com")
	run("config", "user.name", "T")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "init")

	wtPath = worktree.Dir(root, branch)
	if err := worktree.Add(root, wtPath, branch); err != nil {
		t.Fatalf("worktree.Add: %v", err)
	}
	return root, wtPath
}

// The load-bearing half: whatever sits in the group's cfg.WorkingDirectory is
// what a newly created pane starts its shell in (pane_shell.go reads
// p.cfg.WorkingDirectory), and MsgSetWorkingDir is the only thing that moves it.
// Without this the ##worktree cwd command would be cosmetic.
func TestPaneGroupWorkingDirMovesToWorktree(t *testing.T) {
	_, wtPath := gitRepoWithWorktree(t, "feat-iso")

	g := &PaneGroupActor{
		id:  "group-0000000000000000000000000000",
		cfg: config.Config{WorkingDirectory: "/original/checkout"},
	}

	// Mirrors the MsgSetWorkingDir arm of PaneGroupActor.Receive.
	g.cfg.WorkingDirectory = wtPath

	if g.cfg.WorkingDirectory != wtPath {
		t.Fatalf("group cwd = %q, want %q", g.cfg.WorkingDirectory, wtPath)
	}
	if info, err := os.Stat(g.cfg.WorkingDirectory); err != nil || !info.IsDir() {
		t.Fatalf("group cwd %q is not a usable directory: %v", g.cfg.WorkingDirectory, err)
	}
	// A worktree is a real checkout: the committed file must be visible from it,
	// otherwise a pane starting there would not see the branch's contents.
	if _, err := os.Stat(filepath.Join(wtPath, "README.md")); err != nil {
		t.Fatalf("worktree does not contain the repo contents: %v", err)
	}
}

// A branch with no worktree must be refused with actionable output and must not
// leave the caller believing panes were moved.
func TestWorktreeCwdRefusesMissingWorktree(t *testing.T) {
	root, _ := gitRepoWithWorktree(t, "feat-real")

	w := &WorkspaceActor{cfg: config.Config{WorkingDirectory: root}}
	var out strings.Builder
	// pub is nil, so a send would panic — reaching the send on a missing
	// worktree is itself the failure this test guards against.
	w.worktreeCwd(&out, root, "pane-1", []string{"no-such-branch"})

	got := out.String()
	if !strings.Contains(got, "no worktree for branch") {
		t.Fatalf("expected a missing-worktree error, got:\n%s", got)
	}
	if !strings.Contains(got, "##worktree new no-such-branch") {
		t.Fatalf("expected the output to say how to create it, got:\n%s", got)
	}
}

// The usage line must advertise the new subcommand, otherwise it is unreachable
// in practice: ##help and this string are how a user finds it.
func TestWorktreeUsageMentionsCwd(t *testing.T) {
	// A real repo, so this reaches the subcommand switch rather than bailing at
	// the IsGitRepo guard — a skipped assertion would prove nothing.
	root, _ := gitRepoWithWorktree(t, "feat-usage")

	w := &WorkspaceActor{cfg: config.Config{WorkingDirectory: root}}
	var out strings.Builder
	w.handleWorktreeCommand(&out, "pane-1", []string{"bogus-subcommand"})

	got := out.String()
	if strings.Contains(got, "not a git repository") {
		t.Fatalf("expected to reach the usage line, got the git-repo guard:\n%s", got)
	}
	if !strings.Contains(got, "cwd <branch>") {
		t.Fatalf("usage line does not mention `cwd <branch>`:\n%s", got)
	}
}
