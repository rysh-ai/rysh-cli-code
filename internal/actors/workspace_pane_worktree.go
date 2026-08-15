// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/google/uuid"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/worktree"
)

// Design 008, pane-side triggers: `##pane new --worktree [branch]` spawns a
// pane whose shell runs inside its own git worktree, and closing the last pane
// group that uses a rysh-provisioned worktree cleans the worktree up — removed
// when clean, KEPT when dirty (path + `git diff --stat` are printed, so
// uncommitted agent work is never discarded silently).
//
// Ownership model: the workspace records which pane group runs in which
// rysh-managed worktree (groupWorktrees, populated by ##pane new --worktree,
// ##worktree cwd and ##agent spawn --worktree). A worktree is auto-removed only
// when the LAST recorded pane-group user closes, no live agent claims it
// (agent registry, design 008 KV round-trip), and `git status --porcelain` is
// empty. Every other outcome keeps the worktree — fail-safe is always "keep";
// `##worktree remove` stays the manual override.
//
// Session shutdown deliberately does NOT clean up: worktrees must survive a
// detach/restart (the agent registry round-trips its worktree records through
// KV for exactly that reason, and so does groupWorktrees — see "Ownership
// persistence" below). Only user-initiated pane/group/lane/tab closes release
// worktrees.

// groupWorktreeRef records the rysh-managed worktree a pane group runs in.
type groupWorktreeRef struct {
	path   string
	branch string
}

// handlePaneNewCommand implements `##pane new [--worktree [branch]]`:
//
//	##pane new                        alias of ##new pane
//	##pane new --worktree             new pane in its own worktree, branch pane/<alias>
//	##pane new --worktree <branch>    same, on an explicit branch (reused if it exists)
//
// The worktree variant creates (or reuses) the worktree first, then creates a
// NEW pane group in the active lane that is BORN with the worktree as its
// working directory (MsgTabCreatePaneGroupInLane.WorkingDir). Carrying the cwd
// on the create message — rather than retargeting afterwards with
// MsgSetWorkingDir, as ##worktree cwd does for existing groups — avoids the
// race where the group's initial pane starts its shell before the retarget
// lands. The caller's own group stays on the shared checkout: one pane works
// in the worktree, the rest of the workspace is untouched.
//
// Default branch: pane/<pane-alias> (the same generated alias becomes the pane
// title), mirroring the agent path's agent/<name> convention — branch, pane
// title and worktree directory all share one human-readable handle.
func (w *WorkspaceActor) handlePaneNewCommand(ctx actor.Context, out *strings.Builder, paneID string, args []string) {
	rest, withClaude := stripClaudeFlag(args)
	if withClaude {
		w.handlePaneNewClaude(ctx, out, paneID, rest)
		return
	}
	rest, withWorktree := stripWorktreeFlag(args)
	if !withWorktree {
		// Plain `##pane new` is an alias of `##new pane`.
		w.handleNewInstance(ctx, out, paneID, append([]string{"pane"}, rest...))
		return
	}

	// Unlike agent spawn (where the agent is the point and isolation degrades
	// loudly), here the worktree IS the point: fail closed, create no pane.
	if !worktree.IsGitRepo(w.baseDir()) {
		fmt.Fprintf(out, "\n[rysh] ##pane new --worktree: %s is not a git repository\n", w.baseDir())
		w.failRysh("##pane new --worktree: %s is not a git repository", w.baseDir())
		return
	}
	root, err := worktree.RepoRoot(w.baseDir())
	if err != nil {
		w.failRysh("%v", err)
		fmt.Fprintf(out, "\n[rysh] ##pane new --worktree: %v\n", err)
		return
	}
	if err := w.checkLimits(1); err != nil {
		w.failRysh("%v", err)
		fmt.Fprintf(out, "\n[rysh] %v\n", err)
		return
	}
	tab := w.resolveOriginTab(paneID)
	if tab == nil {
		fmt.Fprintf(out, "\n[rysh] no active tab\n")
		w.failRysh("no active tab")
		return
	}
	laneID := w.resolveLaneInTab(tab, "")
	if laneID == "" {
		fmt.Fprintf(out, "\n[rysh] no active lane\n")
		w.failRysh("no active lane")
		return
	}

	alias := w.generateUniqueAlias()
	branchArg := ""
	if len(rest) > 0 {
		branchArg = rest[0]
	}
	branch, path := w.provisionPaneWorktree(out, root, branchArg, alias)
	if branch == "" {
		return // provisionPaneWorktree already reported the failure
	}

	groupID := uuid.NewString()
	_ = w.pub.Send(msg.T("tab", tab.id, "inbox"), &msg.MsgTabCreatePaneGroupInLane{
		LaneID:     laneID,
		Title:      alias,
		GroupID:    groupID,
		WorkingDir: path,
	})
	w.trackGroupWorktree(groupID, path, branch)

	w.resCounts.panes++
	w.restoreFocusAfterCreate(paneID)
	w.persistToKV()

	fmt.Fprintf(out, "\n[rysh] new pane %q created in lane %.8s — shell starts in the worktree\n", alias, laneID)
	fmt.Fprintf(out, "  merge back when done:  ##worktree merge %s\n", branch)
	fmt.Fprintf(out, "  closing the pane removes the worktree if clean; dirty worktrees are kept\n")
}

// provisionPaneWorktree creates (or reuses) the worktree for a `##pane new
// --worktree` pane. branchArg overrides the default pane/<alias> branch.
// Returns ("", "") after reporting when provisioning fails. Mirrors
// isolateAgentWorktree's create/reuse messages.
func (w *WorkspaceActor) provisionPaneWorktree(out *strings.Builder, root, branchArg, alias string) (string, string) {
	branch := branchArg
	if branch == "" {
		branch = "pane/" + worktree.SanitizeBranch(alias)
	}
	path := worktree.Dir(root, branch)
	if info, statErr := os.Stat(path); statErr != nil || !info.IsDir() {
		if err := worktree.Add(root, path, branch); err != nil {
			fmt.Fprintf(out, "\n[rysh] ##pane new --worktree: create failed: %v\n", err)
			w.failRysh("##pane new --worktree: create failed: %v", err)
			return "", ""
		}
		fmt.Fprintf(out, "\n[rysh] created worktree %s (branch %s)\n", path, branch)
	} else {
		fmt.Fprintf(out, "\n[rysh] reusing worktree %s (branch %s)\n", path, branch)
	}
	return branch, path
}

// ---------------------------------------------------------------------------
// Ownership tracking + cleanup-on-close
// ---------------------------------------------------------------------------

// trackGroupWorktree records that a pane group runs inside a rysh-managed
// worktree, making the group a "user" for cleanup-on-close purposes.
func (w *WorkspaceActor) trackGroupWorktree(groupID, path, branch string) {
	if groupID == "" || path == "" {
		return
	}
	if w.groupWorktrees == nil {
		w.groupWorktrees = make(map[string]groupWorktreeRef)
	}
	w.groupWorktrees[groupID] = groupWorktreeRef{path: path, branch: branch}
	w.persistGroupWorktrees()
}

// ---------------------------------------------------------------------------
// Ownership persistence (E-17)
// ---------------------------------------------------------------------------
//
// groupWorktrees used to live only in memory, so a daemon restart forgot which
// pane group owned which worktree: the close of an owning group found no
// record, returned "", and orphaned the worktree until the user found it and
// ran `##worktree remove` by hand. The agent half of design 008 already
// round-trips its worktree record through KV (agentKV.WorktreePath /
// restoreFromKV in agent_registry.go); this is the pane-group equivalent,
// keyed and written like the workspace's other side-document (cron.jobs).
//
// The fail-safe direction is unchanged and load-bearing: this code can only
// ADD records that trackGroupWorktree itself wrote. Every degraded path — no
// KV, an unreadable key, bytes that are not the expected document, a record
// whose directory has since vanished — ends with the record ABSENT, and an
// absent record makes releaseGroupWorktree a silent no-op ("keep"). There is
// no path here that can produce a removal the in-memory code would not have
// made.

// groupWorktreeKV is the serialisable form of one ownership record. Kept
// separate from groupWorktreeRef so the persisted shape is explicit (the ref's
// fields are unexported and would serialise to nothing).
type groupWorktreeKV struct {
	Path   string `json:"path"`
	Branch string `json:"branch,omitempty"`
}

// groupWorktreeKVKey is the workspace-KV key holding the ownership map.
// Workspaces share one bucket, so the key is workspace-scoped exactly as
// kvKey() is — group ids are per workspace, and one workspace's records must
// not clobber another's. Sanitized for the NATS KV key charset.
func (w *WorkspaceActor) groupWorktreeKVKey() string {
	if w.workspaceName != "" {
		return "worktrees_groups_" + sanitizeKVKey(w.workspaceName)
	}
	return "worktrees_groups"
}

// persistGroupWorktrees writes the ownership map to KV. Called on every
// mutation (track + the delete in releaseGroupWorktree): the map is tiny and
// changes only when a pane group takes or gives up a worktree, so it is
// written directly rather than through the debounced layout persist — an
// ownership record that is one crash behind is exactly the bug being fixed.
func (w *WorkspaceActor) persistGroupWorktrees() {
	if w.wKV == nil {
		return
	}
	entries := make(map[string]groupWorktreeKV, len(w.groupWorktrees))
	for groupID, ref := range w.groupWorktrees {
		entries[groupID] = groupWorktreeKV{Path: ref.path, Branch: ref.branch}
	}
	data, err := json.Marshal(entries)
	if err != nil {
		return
	}
	if _, err := w.wKV.Put(w.groupWorktreeKVKey(), data); err != nil {
		// Surfaced, not swallowed: a failed write means the next restart falls
		// back to keep + manual remove, which is safe but worth seeing.
		slog.Warn("workspace: group-worktree ownership persist failed",
			"key", w.groupWorktreeKVKey(), "err", err)
	}
}

// restoreGroupWorktreesFromKV re-populates the ownership map at workspace
// start, so a pane group that survives the restart still cleans up its
// worktree when it closes. Records whose directory no longer exists are
// pruned (and the pruning written back) — a vanished worktree is nothing to
// own, and nothing is removed on its behalf.
func (w *WorkspaceActor) restoreGroupWorktreesFromKV() {
	if w.wKV == nil {
		return
	}
	entry, err := w.wKV.Get(w.groupWorktreeKVKey())
	if err != nil {
		return // never written, or unreadable -> no records -> close keeps
	}
	var entries map[string]groupWorktreeKV
	if err := json.Unmarshal(entry.Value(), &entries); err != nil {
		slog.Warn("workspace: group-worktree ownership record unreadable — "+
			"worktrees will be kept on close until re-tracked",
			"key", w.groupWorktreeKVKey(), "err", err)
		return
	}

	restored := 0
	for groupID, gkv := range entries {
		if groupID == "" || gkv.Path == "" {
			continue
		}
		if info, serr := os.Stat(gkv.Path); serr != nil || !info.IsDir() {
			continue // stale record: directory gone -> drop it, remove nothing
		}
		if w.groupWorktrees == nil {
			w.groupWorktrees = make(map[string]groupWorktreeRef)
		}
		w.groupWorktrees[groupID] = groupWorktreeRef{path: gkv.Path, branch: gkv.Branch}
		restored++
	}
	if restored != len(entries) {
		w.persistGroupWorktrees() // make the pruning durable
	}
	slog.Info("workspace: restored pane-group worktree ownership",
		"restored", restored, "pruned", len(entries)-restored)
}

// releaseGroupWorktree is the cleanup-on-close hook: called when the pane
// group groupID is being torn down. If the group was the last recorded user of
// its worktree, a CLEAN worktree is removed (+ prune) and a DIRTY one is kept
// with the path and `git diff --stat` reported. Returns the report to render
// ("" when there is nothing to say). Never removes on any doubt.
func (w *WorkspaceActor) releaseGroupWorktree(groupID string) string {
	ref, ok := w.groupWorktrees[groupID]
	if !ok {
		return ""
	}
	delete(w.groupWorktrees, groupID)
	// Durable immediately: the group is gone whatever the outcome below, so a
	// restart must not see it as a live user of the worktree.
	w.persistGroupWorktrees()

	// Another pane group still runs in this worktree — not ours to touch.
	for other, oref := range w.groupWorktrees {
		if oref.path == ref.path {
			return fmt.Sprintf("[rysh] worktree kept: %s (still used by pane group %.8s)\n", ref.path, other)
		}
	}

	// A live agent still claims the worktree (registry record, design 008).
	if agentPaths, ok := w.agentWorktreePaths(); !ok {
		return fmt.Sprintf("[rysh] worktree kept: %s (could not verify agent users — remove manually with ##worktree remove %s)\n", ref.path, ref.branch)
	} else {
		for _, p := range agentPaths {
			if p == ref.path {
				return fmt.Sprintf("[rysh] worktree kept: %s (in use by a spawned agent)\n", ref.path)
			}
		}
	}

	// Directory already gone (manual rm, crash): just prune the stale record.
	if _, err := os.Stat(ref.path); err != nil {
		if root, rerr := worktree.RepoRoot(w.baseDir()); rerr == nil {
			_ = worktree.Prune(root)
		}
		return ""
	}

	clean, err := worktree.IsClean(ref.path)
	if err != nil {
		return fmt.Sprintf("[rysh] worktree kept: %s (status check failed: %v)\n", ref.path, err)
	}
	if !clean {
		// Same rendering as ##worktree remove's dirty refusal
		// (workspace_worktree.go): path + diff --stat + how to proceed.
		var b strings.Builder
		fmt.Fprintf(&b, "[rysh] worktree %s is DIRTY — kept (uncommitted work preserved)\n", ref.path)
		if stat, serr := worktree.DiffStat(ref.path); serr == nil && stat != "" {
			fmt.Fprintf(&b, "%s\n", stat)
		}
		fmt.Fprintf(&b, "  merge back: ##worktree merge %s   remove: ##worktree remove %s\n", ref.branch, ref.branch)
		return b.String()
	}

	root, err := worktree.RepoRoot(w.baseDir())
	if err != nil {
		return fmt.Sprintf("[rysh] worktree kept: %s (%v)\n", ref.path, err)
	}
	if err := worktree.Remove(root, ref.path, false); err != nil {
		return fmt.Sprintf("[rysh] worktree kept: %s (remove failed: %v)\n", ref.path, err)
	}
	_ = worktree.Prune(root)
	return fmt.Sprintf("[rysh] worktree removed (clean): %s (branch %s kept)\n", ref.path, ref.branch)
}

// releaseWorktreesInLaneSnap releases every tracked group in a lane snapshot.
func (w *WorkspaceActor) releaseWorktreesInLaneSnap(lane *domain.LaneSnapshot) string {
	var b strings.Builder
	for _, g := range lane.PaneGroups {
		b.WriteString(w.releaseGroupWorktree(g.ID))
	}
	return b.String()
}

// releaseWorktreesInTabSnap releases every tracked group in a tab snapshot.
func (w *WorkspaceActor) releaseWorktreesInTabSnap(tabSnap *domain.TabSnapshot) string {
	if tabSnap == nil {
		return ""
	}
	var b strings.Builder
	for li := range tabSnap.Lanes {
		b.WriteString(w.releaseWorktreesInLaneSnap(&tabSnap.Lanes[li]))
	}
	return b.String()
}

// reportWorktreeRelease renders a cleanup report to the active pane via the
// established pane rysh-output channel. Called after the close has settled
// (syncActivePane), so the report lands in the surviving focused pane.
func (w *WorkspaceActor) reportWorktreeRelease(report string) {
	if report == "" || w.pub == nil || w.activePaneID == "" {
		return
	}
	_ = w.pub.SendPaneRyshOutput(w.activePaneID, "\n"+report)
}

// agentWorktreePaths lists the worktree paths currently claimed by spawned
// agents. ok=false means the registry could not be consulted — callers must
// fail safe (keep the worktree). No registry spawned means no agent users.
func (w *WorkspaceActor) agentWorktreePaths() ([]string, bool) {
	if w.actorSystem == nil || w.agentRegistryPID == nil {
		return nil, true
	}
	infos, err := w.queryAgents()
	if err != nil {
		return nil, false
	}
	var paths []string
	for _, a := range infos {
		if a.WorktreePath != "" {
			paths = append(paths, a.WorktreePath)
		}
	}
	return paths, true
}

// groupClosedByPaneClose mirrors TabActor.closePaneInLane's decision for a
// keyboard pane close (MsgClosePane): given the tab snapshot, it returns the
// ID of the pane group that will be torn down, or "" when only a pane (or
// nothing) closes. The active group is the one holding the tab's active pane.
//
//  1. group has >1 stacked panes  -> only the top pane closes -> ""
//  2. lane has >1 groups          -> the group closes         -> group ID
//  3. tab has >1 lanes            -> the lane (and its only group) closes -> group ID
//  4. last group of last lane     -> closePaneInLane no-ops   -> ""
//
// Pure function over the snapshot so the mapping is testable without actors.
func groupClosedByPaneClose(tabSnap *domain.TabSnapshot) string {
	if tabSnap == nil {
		return ""
	}
	for li := range tabSnap.Lanes {
		lane := &tabSnap.Lanes[li]
		for gi := range lane.PaneGroups {
			g := &lane.PaneGroups[gi]
			for _, p := range g.Panes {
				if p.ID != tabSnap.ActivePaneID {
					continue
				}
				if len(g.Panes) > 1 {
					return ""
				}
				if len(lane.PaneGroups) > 1 {
					return g.ID
				}
				if len(tabSnap.Lanes) > 1 {
					return g.ID
				}
				return ""
			}
		}
	}
	return ""
}
