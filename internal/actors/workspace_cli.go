// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"

	"github.com/asynkron/protoactor-go/actor"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func (w *WorkspaceActor) handleCLIDeleteTab(ctx actor.Context, m *msg.MsgCLIDeleteTab) *msg.MsgCLIResponse {
	if m.TabID == "" {
		return &msg.MsgCLIResponse{OK: false, Error: "tab_id is required"}
	}
	if len(w.tabs) <= 1 {
		return &msg.MsgCLIResponse{OK: false, Error: "cannot delete the last tab"}
	}
	idx := -1
	for i, t := range w.tabs {
		if t.id == m.TabID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return &msg.MsgCLIResponse{OK: false, Error: fmt.Sprintf("tab %q not found", m.TabID)}
	}
	info := w.tabs[idx]

	// Release all pane aliases belonging to this tab.
	tabSnap := w.queryTabSnapshot(info.id)
	for p := range domain.PanesInTab(tabSnap) {
		w.releaseAlias(p.Title)
	}

	// KEEP THE HUMAN ON THE TAB THEY WERE ON. Remembering the active tab by ID
	// across the removal is the whole fix: the index-only version was correct
	// for the tab that shifted into place and wrong for everyone else, so
	// deleting a tab to the LEFT of the active one silently moved the view one
	// tab along. With `E-44` this stopped being cosmetic — a fleet tearing its
	// own tab down would yank whoever was watching another fleet, which is the
	// focus theft design 025 §4.1 hazard 2 exists to prevent.
	activeID := ""
	if w.activeTabIdx >= 0 && w.activeTabIdx < len(w.tabs) {
		activeID = w.tabs[w.activeTabIdx].id
	}

	ctx.Stop(info.pid)
	w.tabs = append(w.tabs[:idx], w.tabs[idx+1:]...)

	w.activeTabIdx = reindexActiveTab(w.tabs, idx, activeID, info.id)
	// Decrement resource counters.
	if tabSnap != nil {
		w.decrementTabResources(tabSnap)
	}
	w.syncActivePane()
	w.persistToKV()
	// Worktree cleanup-on-close (design 008) for every group in the tab.
	w.reportWorktreeRelease(w.releaseWorktreesInTabSnap(tabSnap))
	return &msg.MsgCLIResponse{OK: true}
}

// reindexActiveTab decides which tab is active after one is removed.
//
// PURE, and split out so the rule is testable against the real thing rather
// than re-stated in a test that would pass either way. tabs is the list AFTER
// the removal; removedIdx is where the deleted tab used to be.
//
// The rule: follow the watcher's OWN tab to its new index. Falling back to the
// index alone was the defect — correct only for the tab that shifted into the
// hole, and silently one-off for every tab to the right of it.
func reindexActiveTab(tabs []*tabInfo, removedIdx int, activeID, removedID string) int {
	if len(tabs) == 0 {
		return 0
	}
	if activeID != "" && activeID != removedID {
		for i, t := range tabs {
			if t.id == activeID {
				return i
			}
		}
	}
	// The active tab WAS the deleted one: take whatever replaced it, or the
	// last tab if it was at the end.
	if removedIdx < len(tabs) {
		return removedIdx
	}
	return len(tabs) - 1
}

func (w *WorkspaceActor) handleCLICreateLane(m *msg.MsgCLICreateLane) *msg.MsgCLIResponse {
	// New lane: adds 1 pane.
	if err := w.checkLimits(1); err != nil {
		return &msg.MsgCLIResponse{OK: false, Error: err.Error()}
	}
	tab := w.findTabByID(m.TabID)
	if tab == nil {
		if m.TabID == "" {
			tab = w.currentTab()
		}
		if tab == nil {
			return &msg.MsgCLIResponse{OK: false, Error: "tab not found"}
		}
	}
	alias := w.generateUniqueAlias()
	_ = w.pub.Send(msg.T("tab", tab.id, "inbox"), &msg.MsgTabCreatePane{Title: alias})
	w.resCounts.panes++
	w.syncActivePane()
	w.persistToKV()
	return &msg.MsgCLIResponse{OK: true}
}

func (w *WorkspaceActor) handleCLIDeleteLane(m *msg.MsgCLIDeleteLane) *msg.MsgCLIResponse {
	if m.LaneID == "" {
		return &msg.MsgCLIResponse{OK: false, Error: "lane_id is required"}
	}
	tab := w.findTabByID(m.TabID)
	if tab == nil {
		// Search all tabs for this lane.
		for _, t := range w.tabs {
			if domain.FindLaneByID(w.queryTabSnapshot(t.id), m.LaneID) != nil {
				tab = t
				break
			}
		}
	}
	if tab == nil {
		return &msg.MsgCLIResponse{OK: false, Error: fmt.Sprintf("lane %q not found", m.LaneID)}
	}

	// Release aliases for all panes in this lane and count resources for decrement.
	worktreeReport := ""
	if lane := domain.FindLaneByID(w.queryTabSnapshot(tab.id), m.LaneID); lane != nil {
		for p := range domain.PanesInLane(lane) {
			w.releaseAlias(p.Title)
		}
		w.resCounts.panes -= domain.CountPanesInLane(lane)
		// Worktree cleanup-on-close (design 008) for the lane's groups.
		worktreeReport = w.releaseWorktreesInLaneSnap(lane)
	}

	_ = w.pub.Send(msg.T("tab", tab.id, "inbox"), &msg.MsgTabDeleteLane{LaneID: m.LaneID})
	w.syncActivePane()
	w.persistToKV()
	w.reportWorktreeRelease(worktreeReport)
	return &msg.MsgCLIResponse{OK: true}
}

func (w *WorkspaceActor) handleCLICreatePaneGroup(m *msg.MsgCLICreatePaneGroup) *msg.MsgCLIResponse {
	// New pane group: adds 1 pane.
	if err := w.checkLimits(1); err != nil {
		return &msg.MsgCLIResponse{OK: false, Error: err.Error()}
	}
	tab := w.findTabByID(m.TabID)
	if tab == nil {
		if m.TabID == "" {
			tab = w.currentTab()
		}
		if tab == nil {
			return &msg.MsgCLIResponse{OK: false, Error: "tab not found"}
		}
	}
	alias := w.generateUniqueAlias()
	if m.LaneID != "" {
		_ = w.pub.Send(msg.T("tab", tab.id, "inbox"),
			&msg.MsgTabCreatePaneGroupInLane{LaneID: m.LaneID, Title: alias})
	} else {
		_ = w.pub.Send(msg.T("tab", tab.id, "inbox"),
			&msg.MsgTabCreatePaneDown{Title: alias})
	}
	w.resCounts.panes++
	w.syncActivePane()
	w.persistToKV()
	return &msg.MsgCLIResponse{OK: true}
}

func (w *WorkspaceActor) handleCLIDeletePaneGroup(m *msg.MsgCLIDeletePaneGroup) *msg.MsgCLIResponse {
	if m.PaneGroupID == "" {
		return &msg.MsgCLIResponse{OK: false, Error: "pane_group_id is required"}
	}

	// ALWAYS resolve the group, even when --tab/--lane were supplied. Treating
	// those hints as permission to skip the search is F-27: an id naming no
	// group was published to the lane, dropped there, and reported as deleted.
	snaps := make([]*domain.TabSnapshot, 0, len(w.tabs))
	for _, t := range w.tabs {
		if s := w.queryTabSnapshot(t.id); s != nil {
			snaps = append(snaps, s)
		}
	}
	site, found := locatePaneGroup(snaps, m.PaneGroupID)
	if !found {
		return &msg.MsgCLIResponse{OK: false, Error: fmt.Sprintf(
			"pane group %q not found — nothing was deleted (a PANE id is not a group id; "+
				"`##pane list` shows both)", m.PaneGroupID)}
	}
	laneID, tabID, paneCount := site.LaneID, site.TabID, len(site.Group.Panes)
	for p := range domain.PanesInGroup(site.Group) {
		w.releaseAlias(p.Title)
	}

	_ = w.pub.Send(msg.T("lane", laneID, "inbox"),
		&msg.MsgLaneDeletePaneGroup{PaneGroupID: m.PaneGroupID})
	// Decrement resource counters.
	w.resCounts.panes -= paneCount
	_ = tabID // suppress unused (used only for search)
	w.syncActivePane()
	w.persistToKV()
	// Worktree cleanup-on-close (design 008): the group is gone; release its
	// worktree (clean -> removed, dirty -> kept + diff --stat report).
	w.reportWorktreeRelease(w.releaseGroupWorktree(m.PaneGroupID))
	return &msg.MsgCLIResponse{OK: true}
}

func (w *WorkspaceActor) handleCLICreatePane(m *msg.MsgCLICreatePane) *msg.MsgCLIResponse {
	// New pane (split right): adds 1 pane.
	if err := w.checkLimits(1); err != nil {
		return &msg.MsgCLIResponse{OK: false, Error: err.Error()}
	}
	tab := w.findTabByID(m.TabID)
	if tab == nil {
		if m.TabID == "" {
			tab = w.currentTab()
		}
		if tab == nil {
			return &msg.MsgCLIResponse{OK: false, Error: "tab not found"}
		}
	}
	alias := w.generateUniqueAlias()
	_ = w.pub.Send(msg.T("tab", tab.id, "inbox"), &msg.MsgTabCreatePane{Title: alias})
	w.resCounts.panes++
	w.syncActivePane()
	w.persistToKV()
	return &msg.MsgCLIResponse{OK: true}
}

func (w *WorkspaceActor) handleCLIDeletePane(ctx actor.Context, m *msg.MsgCLIDeletePane) *msg.MsgCLIResponse {
	if m.PaneID == "" {
		return &msg.MsgCLIResponse{OK: false, Error: "pane_id is required"}
	}

	// Find the pane group containing this pane.
	for _, t := range w.tabs {
		tabSnap := w.queryTabSnapshot(t.id)
		if tabSnap == nil {
			continue
		}
		site, ok := domain.LocatePaneInTab(tabSnap, m.PaneID)
		if !ok {
			continue
		}
		// Decide BEFORE releasing the alias or publishing: the group silently
		// drops a delete that would empty it, so answering OK first is how
		// F-24 reported `deleted` over a pane that never moved.
		if refusal := paneDeleteRefusal(site, tabSnap, len(w.tabs)); refusal != "" {
			return &msg.MsgCLIResponse{OK: false, Error: refusal}
		}
		w.releaseAlias(site.Pane.Title)
		_ = w.pub.Send(msg.T("pane-group", site.Group.ID, "inbox"),
			&msg.MsgPaneGroupDeletePane{PaneID: m.PaneID})
		w.resCounts.panes--
		w.syncActivePane()
		w.persistToKV()
		return &msg.MsgCLIResponse{OK: true}
	}
	return &msg.MsgCLIResponse{OK: false, Error: fmt.Sprintf("pane %q not found", m.PaneID)}
}

func (w *WorkspaceActor) handleCLICreateStackedPane(m *msg.MsgCLICreateStackedPane) *msg.MsgCLIResponse {
	// Stacked pane: adds 1 pane only.
	if err := w.checkLimits(1); err != nil {
		return &msg.MsgCLIResponse{OK: false, Error: err.Error()}
	}
	tab := w.findTabByID(m.TabID)
	if tab == nil {
		if m.TabID == "" {
			tab = w.currentTab()
		}
		if tab == nil {
			return &msg.MsgCLIResponse{OK: false, Error: "tab not found"}
		}
	}
	alias := w.generateUniqueAlias()
	if m.LaneID != "" && m.PaneGroupID != "" {
		_ = w.pub.Send(msg.T("tab", tab.id, "inbox"),
			&msg.MsgTabCreateStackedPaneInLane{
				LaneID:      m.LaneID,
				PaneGroupID: m.PaneGroupID,
				Title:       alias,
			})
	} else {
		_ = w.pub.Send(msg.T("tab", tab.id, "inbox"),
			&msg.MsgTabCreateStackedPane{Title: alias})
	}
	w.resCounts.panes++
	w.syncActivePane()
	w.persistToKV()
	return &msg.MsgCLIResponse{OK: true}
}

func (w *WorkspaceActor) handleCLIPipelineEnable(m *msg.MsgCLIPipelineEnable) *msg.MsgCLIResponse {
	tab := w.findTabByID(m.TabID)
	if tab == nil {
		if m.TabID == "" {
			tab = w.currentTab()
		}
		if tab == nil {
			return &msg.MsgCLIResponse{OK: false, Error: "tab not found"}
		}
	}
	_ = w.pub.Send(msg.T("tab", tab.id, "inbox"), &msg.MsgTabPipelineEnable{})
	w.persistToKV()
	return &msg.MsgCLIResponse{OK: true}
}

func (w *WorkspaceActor) handleCLIPipelineDisable(m *msg.MsgCLIPipelineDisable) *msg.MsgCLIResponse {
	tab := w.findTabByID(m.TabID)
	if tab == nil {
		if m.TabID == "" {
			tab = w.currentTab()
		}
		if tab == nil {
			return &msg.MsgCLIResponse{OK: false, Error: "tab not found"}
		}
	}
	_ = w.pub.Send(msg.T("tab", tab.id, "inbox"), &msg.MsgTabPipelineDisable{})
	w.persistToKV()
	return &msg.MsgCLIResponse{OK: true}
}
