package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func setupRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	run(t, dir, "init", "-b", "main")
	run(t, dir, "config", "user.email", "t@example.com")
	run(t, dir, "config", "user.name", "T")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "add", ".")
	run(t, dir, "commit", "-m", "init")
	return dir
}

func TestIsGitRepo(t *testing.T) {
	repo := setupRepo(t)
	if !IsGitRepo(repo) {
		t.Fatal("repo not detected as git")
	}
	if IsGitRepo(t.TempDir()) {
		t.Fatal("empty dir detected as git")
	}
}

func TestAddListRemove(t *testing.T) {
	repo := setupRepo(t)
	root, err := RepoRoot(repo)
	if err != nil {
		t.Fatal(err)
	}

	wtPath := Dir(root, "agent/feature-x")
	if err := Add(root, wtPath, "agent/feature-x"); err != nil {
		t.Fatalf("add: %v", err)
	}

	infos, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, in := range infos {
		if in.Branch == "agent/feature-x" {
			found = true
		}
	}
	if !found {
		t.Fatalf("new worktree not in list: %+v", infos)
	}

	// A fresh worktree is clean.
	clean, err := IsClean(wtPath)
	if err != nil || !clean {
		t.Fatalf("fresh worktree clean=%v err=%v", clean, err)
	}

	// Remove the clean worktree.
	if err := Remove(root, wtPath, false); err != nil {
		t.Fatalf("remove: %v", err)
	}
}

func TestDirtyDetection(t *testing.T) {
	repo := setupRepo(t)
	root, _ := RepoRoot(repo)
	wtPath := Dir(root, "dirty")
	if err := Add(root, wtPath, "dirty"); err != nil {
		t.Fatal(err)
	}
	// Untracked file ⇒ not clean (status --porcelain catches it).
	if err := os.WriteFile(filepath.Join(wtPath, "new.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	clean, err := IsClean(wtPath)
	if err != nil {
		t.Fatal(err)
	}
	if clean {
		t.Fatal("dirty worktree reported clean")
	}
}

func TestSanitizeBranch(t *testing.T) {
	if got := SanitizeBranch("agent/feature x"); got != "agent-feature-x" {
		t.Fatalf("got %q", got)
	}
}

// TestPrune: a worktree whose directory was deleted out from under git (the
// crashed-agent orphan case, design 008) is dropped from the list by Prune,
// while intact worktrees survive.
func TestPrune(t *testing.T) {
	root := setupRepo(t)
	keep := Dir(root, "agent/keeper")
	gone := Dir(root, "agent/goner")
	if err := Add(root, keep, "agent/keeper"); err != nil {
		t.Fatal(err)
	}
	if err := Add(root, gone, "agent/goner"); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}

	if err := Prune(root); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	infos, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, i := range infos {
		paths = append(paths, i.Path)
	}
	joined := strings.Join(paths, "\n")
	if strings.Contains(joined, "goner") {
		t.Fatalf("pruned worktree still listed:\n%s", joined)
	}
	if !strings.Contains(joined, "keeper") {
		t.Fatalf("intact worktree lost by prune:\n%s", joined)
	}
}

// DeleteBranch drops the throwaway branch a per-run worktree was created on
// (design 009), so repeated headless runs never accumulate branches.
func TestDeleteBranch(t *testing.T) {
	repo := setupRepo(t)
	wtPath := Dir(repo, "run-123")
	if err := Add(repo, wtPath, "run-123"); err != nil {
		t.Fatalf("add: %v", err)
	}
	// A branch checked out in a worktree cannot be deleted; remove it first —
	// the order the run cleanup uses.
	if err := Remove(repo, wtPath, true); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := DeleteBranch(repo, "run-123"); err != nil {
		t.Fatalf("delete branch: %v", err)
	}
	out, err := exec.Command("git", "-C", repo, "branch", "--list", "run-123").Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("branch still exists: %q", out)
	}
}
