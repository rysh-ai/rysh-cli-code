// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"os"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/worktree"
)

// baseDir returns the directory to resolve the git repo from.
func (w *WorkspaceActor) baseDir() string {
	if d := strings.TrimSpace(w.cfg.WorkingDirectory); d != "" {
		return d
	}
	return "."
}

// handleWorktreeCommand implements ##worktree (design 008):
//
//	##worktree new <branch>       create .rysh/worktrees/<branch> on a new branch
//	##worktree list | status      list worktrees (status adds clean/dirty)
//	##worktree remove <branch>    remove if clean (dirty ⇒ refused + diff --stat)
//	##worktree cwd <branch>       point the active pane group at the worktree, so
//	                              panes added to it start their shell inside
//	##worktree merge <branch> [--confirm]   preview, then merge on --confirm
func (w *WorkspaceActor) handleWorktreeCommand(out *strings.Builder, paneID string, args []string) {
	if !worktree.IsGitRepo(w.baseDir()) {
		fmt.Fprintf(out, "worktree: %s is not a git repository\n", w.baseDir())
		w.failRysh("worktree: %s is not a git repository", w.baseDir())
		return
	}
	root, err := worktree.RepoRoot(w.baseDir())
	if err != nil {
		w.failRysh("%v", err)
		fmt.Fprintf(out, "worktree: %v\n", err)
		return
	}
	sub := ""
	if len(args) > 0 {
		sub = strings.ToLower(strings.TrimSpace(args[0]))
	}
	switch sub {
	case "new", "add":
		w.worktreeNew(out, root, args[1:])
	case "list", "status":
		w.worktreeList(out, root, sub == "status")
	case "remove", "rm":
		w.worktreeRemove(out, root, args[1:])
	case "merge":
		w.worktreeMerge(out, root, args[1:])
	case "cwd", "use":
		w.worktreeCwd(out, root, paneID, args[1:])
	default:
		fmt.Fprintf(out, "usage: ##worktree new <branch> | list | status | cwd <branch> | remove <branch> | merge <branch> [--confirm]\n")
		w.failRyshUsage("unknown ##worktree subcommand: %q", sub)
	}
}

// worktreeCwd points the ACTIVE PANE GROUP at a worktree, so panes added to that
// group from now on start their shell inside it (design 008).
//
// This is the step that makes `##worktree` isolation rather than a `git
// worktree` wrapper: creating a worktree never touched any pane's cwd, so
// "worktree-per-agent" was a directory on disk that nothing ran in.
//
// Deliberately scoped to one group rather than broadcast workspace-wide (as
// `##session cwd` does): the point is that ONE agent works in the worktree while
// the rest of the workspace stays on the main checkout. Already-running panes
// keep their shells — a live shell's cwd is the user's, not ours to move.
func (w *WorkspaceActor) worktreeCwd(out *strings.Builder, root, paneID string, args []string) {
	if len(args) == 0 {
		fmt.Fprintf(out, "usage: ##worktree cwd <branch>\n")
		return
	}
	branch := args[0]
	path := worktree.Dir(root, branch)
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		fmt.Fprintf(out, "worktree: no worktree for branch %q at %s\n", branch, path)
		fmt.Fprintf(out, "  create it first:  ##worktree new %s\n", branch)
		return
	}

	groupID := w.groupIDForPane(paneID)
	if groupID == "" {
		fmt.Fprintf(out, "worktree: could not resolve the active pane group\n")
		w.failRysh("worktree: could not resolve the active pane group")
		return
	}

	_ = w.pub.Send(msg.T("pane-group", groupID, "inbox"), &msg.MsgSetWorkingDir{Dir: path})
	// The group is now a recorded user of the worktree: closing it triggers
	// cleanup-on-close (clean -> removed, dirty -> kept). See
	// workspace_pane_worktree.go.
	w.trackGroupWorktree(groupID, path, branch)

	fmt.Fprintf(out, "worktree cwd set for this pane group: %s  (branch %s)\n", path, branch)
	fmt.Fprintf(out, "  new panes in this group start there (e.g. ##new stack, ctrl+p s)\n")
	fmt.Fprintf(out, "  running panes keep their current shell; cd %s to move one by hand\n", path)
}

// groupIDForPane resolves the pane group that owns paneID, via the tab snapshot.
func (w *WorkspaceActor) groupIDForPane(paneID string) string {
	if paneID == "" {
		return ""
	}
	tab := w.currentTab()
	if tab == nil {
		return ""
	}
	tabSnap := w.queryTabSnapshot(tab.id)
	if tabSnap == nil {
		return ""
	}
	if g := domain.GroupOfPane(tabSnap, paneID); g != nil {
		return g.ID
	}
	return ""
}

func (w *WorkspaceActor) worktreeNew(out *strings.Builder, root string, args []string) {
	if len(args) == 0 {
		fmt.Fprintf(out, "usage: ##worktree new <branch>\n")
		return
	}
	branch := args[0]
	path := worktree.Dir(root, branch)
	if err := worktree.Add(root, path, branch); err != nil {
		fmt.Fprintf(out, "worktree: create failed: %v\n", err)
		w.failRysh("worktree: create failed: %v", err)
		return
	}
	fmt.Fprintf(out, "worktree created: %s  (branch %s)\n", path, branch)
	fmt.Fprintf(out, "  run this pane group there:  ##worktree cwd %s\n", branch)
	fmt.Fprintf(out, "  or move one shell by hand:  cd %s\n", path)
}

func (w *WorkspaceActor) worktreeList(out *strings.Builder, root string, status bool) {
	infos, err := worktree.List(root)
	if err != nil {
		w.failRysh("%v", err)
		fmt.Fprintf(out, "worktree: %v\n", err)
		return
	}
	fmt.Fprintf(out, "worktrees (%d):\n", len(infos))
	for _, in := range infos {
		line := fmt.Sprintf("  %-40s %s", in.Path, in.Branch)
		if status {
			if clean, err := worktree.IsClean(in.Path); err == nil {
				if clean {
					line += "  [clean]"
				} else {
					line += "  [dirty]"
				}
			}
		}
		fmt.Fprintf(out, "%s\n", line)
	}
}

func (w *WorkspaceActor) worktreeRemove(out *strings.Builder, root string, args []string) {
	if len(args) == 0 {
		fmt.Fprintf(out, "usage: ##worktree remove <branch>\n")
		return
	}
	path := worktree.Dir(root, args[0])
	clean, err := worktree.IsClean(path)
	if err != nil {
		w.failRysh("%v", err)
		fmt.Fprintf(out, "worktree: %v\n", err)
		return
	}
	if !clean {
		fmt.Fprintf(out, "worktree %s is DIRTY — not removed (uncommitted work preserved)\n", path)
		if stat, err := worktree.DiffStat(path); err == nil && stat != "" {
			fmt.Fprintf(out, "%s\n", stat)
		}
		fmt.Fprintf(out, "  commit/merge first, or remove manually: git worktree remove --force %s\n", path)
		return
	}
	if err := worktree.Remove(root, path, false); err != nil {
		fmt.Fprintf(out, "worktree: remove failed: %v\n", err)
		w.failRysh("worktree: remove failed: %v", err)
		return
	}
	fmt.Fprintf(out, "worktree removed: %s\n", path)
}

func (w *WorkspaceActor) worktreeMerge(out *strings.Builder, root string, args []string) {
	confirm := false
	branch := ""
	for _, a := range args {
		if a == "--confirm" {
			confirm = true
		} else if branch == "" {
			branch = a
		}
	}
	if branch == "" {
		fmt.Fprintf(out, "usage: ##worktree merge <branch> [--confirm]\n")
		return
	}
	if !confirm {
		preview, err := worktree.MergePreview(root, branch)
		if err != nil {
			fmt.Fprintf(out, "worktree: preview failed: %v\n", err)
			w.failRysh("worktree: preview failed: %v", err)
			return
		}
		if strings.TrimSpace(preview) == "" {
			preview = "(no differences)"
		}
		fmt.Fprintf(out, "merge preview (%s → current):\n%s\n", branch, preview)
		fmt.Fprintf(out, "  re-run with --confirm to merge\n")
		return
	}
	res, err := worktree.Merge(root, branch)
	if err != nil {
		fmt.Fprintf(out, "worktree: merge failed (fail-closed — resolve conflicts manually):\n%s\n%v\n", res, err)
		w.failRysh("worktree: merge failed (fail-closed — resolve conflicts manually):\n%s\n%v", res, err)
		return
	}
	fmt.Fprintf(out, "merged %s:\n%s\n", branch, res)
}
