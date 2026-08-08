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

	ctx.Stop(info.pid)
	w.tabs = append(w.tabs[:idx], w.tabs[idx+1:]...)
	if w.activeTabIdx >= len(w.tabs) {
		w.activeTabIdx = len(w.tabs) - 1
	}
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

	// Find the lane containing this pane group.
	laneID := m.LaneID
	tabID := m.TabID
	paneCount := 0
	if laneID == "" || tabID == "" {
		for _, t := range w.tabs {
			tabSnap := w.queryTabSnapshot(t.id)
			if tabSnap == nil {
				continue
			}
			for lane, g := range domain.GroupsInTab(tabSnap) {
				if g.ID != m.PaneGroupID {
					continue
				}
				laneID = lane.ID
				tabID = t.id
				paneCount = len(g.Panes)
				// Release aliases.
				for p := range domain.PanesInGroup(g) {
					w.releaseAlias(p.Title)
				}
				break
			}
			if laneID != "" {
				break
			}
		}
	}
	if laneID == "" {
		return &msg.MsgCLIResponse{OK: false, Error: fmt.Sprintf("pane group %q not found", m.PaneGroupID)}
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
		w.releaseAlias(site.Pane.Title)
		if len(site.Group.Panes) <= 1 && len(site.Lane.PaneGroups) <= 1 && len(tabSnap.Lanes) <= 1 {
			// Last pane in last group in last lane: don't delete (would close entire tab)
			if len(w.tabs) <= 1 {
				return &msg.MsgCLIResponse{OK: false, Error: "cannot delete the last pane"}
			}
		}
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
