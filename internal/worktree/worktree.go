// Package worktree wraps git-worktree operations for per-agent isolation
// (design 008): give a pane/agent its own git worktree so parallel agents never
// collide on the same repo — "N agents, N branches, one tab, zero collisions."
//
// This package is the tested git seam. The command surface (##worktree) and the
// eventual pane-cwd integration live in internal/actors.
package worktree

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// Info describes one worktree from `git worktree list`.
type Info struct {
	Path   string
	Branch string
	Head   string
}

func git(dir string, args ...string) (string, error) {
	full := append([]string{"-C", dir}, args...)
	out, err := exec.Command("git", full...).CombinedOutput()
	if err != nil {
		return strings.TrimSpace(string(out)), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// IsGitRepo reports whether dir is inside a git work tree.
func IsGitRepo(dir string) bool {
	out, err := git(dir, "rev-parse", "--is-inside-work-tree")
	return err == nil && out == "true"
}

// RepoRoot returns the top-level directory of the repo containing dir.
func RepoRoot(dir string) (string, error) {
	return git(dir, "rev-parse", "--show-toplevel")
}

// SanitizeBranch turns a branch name into a filesystem-safe directory segment.
func SanitizeBranch(branch string) string {
	s := strings.ReplaceAll(branch, "/", "-")
	s = strings.ReplaceAll(s, " ", "-")
	return s
}

// Dir returns the conventional worktree path for a branch under the repo's
// .rysh/worktrees/ (mirrors the monorepo's own worktrees/ convention).
func Dir(repoRoot, branch string) string {
	return filepath.Join(repoRoot, ".rysh", "worktrees", SanitizeBranch(branch))
}

// Add creates a worktree at path on a new branch. If the branch already exists,
// it is checked out instead of created.
func Add(repoRoot, path, branch string) error {
	// Does the branch already exist?
	if _, err := git(repoRoot, "rev-parse", "--verify", "refs/heads/"+branch); err == nil {
		_, err := git(repoRoot, "worktree", "add", path, branch)
		return err
	}
	_, err := git(repoRoot, "worktree", "add", path, "-b", branch)
	return err
}

// Prune drops stale administrative entries — worktrees whose directory was
// deleted out from under git (crashed agent, manual rm -rf). It never touches
// a directory that still exists, so it is safe to run unconditionally at
// session start (design 008's orphan sweep).
func Prune(repoRoot string) error {
	_, err := git(repoRoot, "worktree", "prune")
	return err
}

// Remove removes a worktree. force removes even if dirty.
func Remove(repoRoot, path string, force bool) error {
	args := []string{"worktree", "remove", path}
	if force {
		args = append(args, "--force")
	}
	_, err := git(repoRoot, args...)
	return err
}

// DeleteBranch force-deletes a local branch. Used by the headless CI runner
// (design 009) to drop the throwaway branch its per-run worktree was created
// on, so repeated `rysh run --worktree` invocations never accumulate branches.
func DeleteBranch(repoRoot, branch string) error {
	_, err := git(repoRoot, "branch", "-D", branch)
	return err
}

// List returns the repo's worktrees (porcelain-parsed).
func List(repoRoot string) ([]Info, error) {
	out, err := git(repoRoot, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var infos []Info
	var cur Info
	flush := func() {
		if cur.Path != "" {
			infos = append(infos, cur)
		}
		cur = Info{}
	}
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			flush()
			cur.Path = strings.TrimPrefix(line, "worktree ")
		case strings.HasPrefix(line, "HEAD "):
			cur.Head = strings.TrimPrefix(line, "HEAD ")
		case strings.HasPrefix(line, "branch "):
			cur.Branch = strings.TrimPrefix(strings.TrimPrefix(line, "branch "), "refs/heads/")
		case line == "detached":
			cur.Branch = "(detached)"
		}
	}
	flush()
	return infos, nil
}

// IsClean reports whether the worktree at path has no uncommitted changes.
func IsClean(path string) (bool, error) {
	out, err := git(path, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return out == "", nil
}

// DiffStat returns `git diff --stat` for the worktree at path.
func DiffStat(path string) (string, error) {
	return git(path, "diff", "--stat")
}

// Merge merges branch into the current branch at repoRoot, returning git output.
// Fails (non-nil error) on conflicts — callers surface it fail-closed.
func Merge(repoRoot, branch string) (string, error) {
	return git(repoRoot, "merge", "--no-edit", branch)
}

// MergePreview returns a diff --stat of what merging branch would bring in.
func MergePreview(repoRoot, branch string) (string, error) {
	return git(repoRoot, "diff", "--stat", "HEAD..."+branch)
}
