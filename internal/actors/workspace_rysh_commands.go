package actors

import (
	"fmt"
	"strings"

	"github.com/atotto/clipboard"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// ---------------------------------------------------------------------------
// ##lane subcommands
// ---------------------------------------------------------------------------

func (w *WorkspaceActor) cmdLaneList(out *strings.Builder) {
	tab := w.currentTab()
	if tab == nil {
		fmt.Fprintf(out, "\n[rysh] no active tab\n")
		w.failRysh("no active tab")
		return
	}
	tabSnap := w.queryTabSnapshot(tab.id)
	if tabSnap == nil {
		fmt.Fprintf(out, "\n[rysh] could not fetch tab snapshot\n")
		w.failRysh("could not fetch tab snapshot")
		return
	}

	fmt.Fprintf(out, "\n[rysh] lanes in tab %q (%d lanes)\n", tab.title, len(tabSnap.Lanes))
	ryshWriter(out).Rule()
	for li, lane := range tabSnap.Lanes {
		totalPanes := domain.CountPanesInLane(&lane)
		marker := "  "
		if lane.ActivePaneID == w.activePaneID {
			marker = "> "
		}
		fmt.Fprintf(out, "%s[%d] %-16s id=%.8s  flex=%d  groups=%d  panes=%d\n",
			marker, li+1, lane.Name, lane.ID, lane.Flex, len(lane.PaneGroups), totalPanes)
	}
	ryshWriter(out).Rule()
}

func (w *WorkspaceActor) cmdLaneInfo(out *strings.Builder, paneID string) {
	tab := w.currentTab()
	if tab == nil {
		fmt.Fprintf(out, "\n[rysh] no active tab\n")
		w.failRysh("no active tab")
		return
	}
	if paneID == "" {
		fmt.Fprintf(out, "\n[rysh] no active pane\n")
		w.failRysh("no active pane")
		return
	}
	tabSnap := w.queryTabSnapshot(tab.id)
	if tabSnap == nil {
		fmt.Fprintf(out, "\n[rysh] could not fetch tab snapshot\n")
		w.failRysh("could not fetch tab snapshot")
		return
	}

	// Find the lane containing the active pane.
	var activeLane *domain.LaneSnapshot
	laneIdx := -1
	if site, ok := domain.LocatePaneInTab(tabSnap, paneID); ok {
		activeLane = site.Lane
		laneIdx = site.LaneIndex
	}
	if activeLane == nil {
		fmt.Fprintf(out, "\n[rysh] pane %s not found in any lane\n", paneID)
		w.failRysh("pane %s not found in any lane", paneID)
		return
	}

	totalPanes := domain.CountPanesInLane(activeLane)

	fmt.Fprintf(out, "\n[rysh] lane info\n")
	ryshWriter(out).Rule()
	fmt.Fprintf(out, "  name        : %s\n", activeLane.Name)
	fmt.Fprintf(out, "  id          : %s\n", activeLane.ID)
	fmt.Fprintf(out, "  tab         : %s\n", tab.title)
	fmt.Fprintf(out, "  position    : %d of %d\n", laneIdx+1, len(tabSnap.Lanes))
	fmt.Fprintf(out, "  flex        : %d\n", activeLane.Flex)
	fmt.Fprintf(out, "  groups      : %d\n", len(activeLane.PaneGroups))
	fmt.Fprintf(out, "  panes       : %d\n", totalPanes)
	fmt.Fprintf(out, "  active pane : %s\n", activeLane.ActivePaneID)
	ryshWriter(out).Rule()
}

// ---------------------------------------------------------------------------
// ##public subcommands
// ---------------------------------------------------------------------------

func (w *WorkspaceActor) cmdPublicPanePrint(out *strings.Builder, paneID string) {
	if paneID == "" {
		fmt.Fprintf(out, "\n[rysh] no active pane\n")
		w.failRysh("no active pane")
		return
	}
	tab := w.currentTab()
	if tab == nil {
		fmt.Fprintf(out, "\n[rysh] no active tab\n")
		w.failRysh("no active tab")
		return
	}
	snap := tab.actor.PaneSnapshot(paneID)
	if snap == nil {
		fmt.Fprintf(out, "\n[rysh] pane %s not found\n", paneID[:8])
		w.failRysh("pane %s not found", paneID[:8])
		return
	}
	trimmed := strings.TrimSpace(snap.Output)
	if trimmed == "" {
		fmt.Fprintf(out, "\n[rysh] output for pane %s is empty\n", paneID[:8])
		return
	}
	lines := strings.Split(trimmed, "\n")
	fmt.Fprintf(out, "\n[rysh] output for pane %s (%d lines)\n", paneID[:8], len(lines))
	ryshWriter(out).Rule()
	for _, line := range lines {
		fmt.Fprintf(out, ">>> %s\n", line)
	}
	ryshWriter(out).Rule()
}

// ---------------------------------------------------------------------------
// ##private subcommands
// ---------------------------------------------------------------------------

func (w *WorkspaceActor) cmdPrivatePanePrint(out *strings.Builder, paneID string) {
	if paneID == "" {
		fmt.Fprintf(out, "\n[rysh] no active pane\n")
		w.failRysh("no active pane")
		return
	}
	tab := w.currentTab()
	if tab == nil {
		fmt.Fprintf(out, "\n[rysh] no active tab\n")
		w.failRysh("no active tab")
		return
	}
	rawOut := tab.actor.PanePrivateOutput(paneID)
	trimmed := strings.TrimSpace(rawOut)
	if trimmed == "" {
		fmt.Fprintf(out, "\n[rysh] private output for pane %s is empty\n", paneID[:8])
		return
	}
	lines := strings.Split(trimmed, "\n")
	fmt.Fprintf(out, "\n[rysh] private (raw) output for pane %s (%d lines)\n", paneID[:8], len(lines))
	ryshWriter(out).Rule()
	for _, line := range lines {
		fmt.Fprintf(out, ">>> %s\n", line)
	}
	ryshWriter(out).Rule()
}

// ---------------------------------------------------------------------------
// ##snap subcommand
// ---------------------------------------------------------------------------

func (w *WorkspaceActor) cmdSnap(out *strings.Builder, paneID, target string) {
	if paneID == "" {
		fmt.Fprintf(out, "\n[rysh] no active pane\n")
		w.failRysh("no active pane")
		return
	}
	tab := w.currentTab()
	if tab == nil {
		fmt.Fprintf(out, "\n[rysh] no active tab\n")
		w.failRysh("no active tab")
		return
	}

	var content string
	var label string
	switch target {
	case "private":
		content = tab.actor.PanePrivateOutput(paneID)
		label = "private"
	case "public":
		if snap := tab.actor.PaneSnapshot(paneID); snap != nil {
			content = snap.Output
		}
		label = "public"
	default:
		fmt.Fprintf(out, "\n[rysh] unknown snap target: %q\n", target)
		w.failRysh("unknown snap target: %q", target)
		fmt.Fprintf(out, "  ##snap             copy private buffer to clipboard (default)\n")
		fmt.Fprintf(out, "  ##snap private     copy private buffer to clipboard\n")
		fmt.Fprintf(out, "  ##snap public      copy public (redacted) buffer to clipboard\n\n")
		return
	}

	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		fmt.Fprintf(out, "\n[rysh] %s buffer for pane %s is empty -- nothing to copy\n", label, paneID[:8])
		w.failRysh("%s buffer for pane %s is empty -- nothing to copy", label, paneID[:8])
		return
	}

	if err := clipboard.WriteAll(trimmed); err != nil {
		fmt.Fprintf(out, "\n[rysh] failed to copy to clipboard: %v\n", err)
		w.failRysh("failed to copy to clipboard: %v", err)
		return
	}

	lines := strings.Count(trimmed, "\n") + 1
	fmt.Fprintf(out, "\n[rysh] copied %s buffer (%d lines, %d bytes) to clipboard\n", label, lines, len(trimmed))
}

// ---------------------------------------------------------------------------
// ##panegroup / ##pg subcommands
// ---------------------------------------------------------------------------

func (w *WorkspaceActor) cmdPaneGroupList(out *strings.Builder, paneID string) {
	tab := w.currentTab()
	if tab == nil {
		fmt.Fprintf(out, "\n[rysh] no active tab\n")
		w.failRysh("no active tab")
		return
	}
	tabSnap := w.queryTabSnapshot(tab.id)
	if tabSnap == nil {
		fmt.Fprintf(out, "\n[rysh] could not fetch tab snapshot\n")
		w.failRysh("could not fetch tab snapshot")
		return
	}

	totalGroups := 0
	for _, lane := range tabSnap.Lanes {
		totalGroups += len(lane.PaneGroups)
	}

	fmt.Fprintf(out, "\n[rysh] pane groups in tab %q (%d groups, %d lanes)\n",
		tab.title, totalGroups, len(tabSnap.Lanes))
	ryshWriter(out).Rule()

	for li, lane := range tabSnap.Lanes {
		fmt.Fprintf(out, "  lane %d (flex=%d)\n", li+1, lane.Flex)
		for gi, g := range lane.PaneGroups {
			marker := "    "
			isActiveGroup := false
			for _, p := range g.Panes {
				if p.ID == paneID {
					isActiveGroup = true
					break
				}
			}
			if isActiveGroup {
				marker = "  > "
			}
			fmt.Fprintf(out, "%s[group %d] id=%.8s  panes=%d\n",
				marker, gi+1, g.ID, len(g.Panes))
			for pi, p := range g.Panes {
				pMarker := "        "
				if p.ID == paneID {
					pMarker = "      > "
				}
				fmt.Fprintf(out, "%s[%d] %-16s  status=%-10s  id=%.8s\n",
					pMarker, pi+1, p.Title, p.Status, p.ID)
			}
		}
	}
	ryshWriter(out).Rule()
}

func (w *WorkspaceActor) cmdPaneGroupInfo(out *strings.Builder, paneID string) {
	tab := w.currentTab()
	if tab == nil {
		fmt.Fprintf(out, "\n[rysh] no active tab\n")
		w.failRysh("no active tab")
		return
	}
	if paneID == "" {
		fmt.Fprintf(out, "\n[rysh] no active pane\n")
		w.failRysh("no active pane")
		return
	}
	tabSnap := w.queryTabSnapshot(tab.id)
	if tabSnap == nil {
		fmt.Fprintf(out, "\n[rysh] could not fetch tab snapshot\n")
		w.failRysh("could not fetch tab snapshot")
		return
	}

	var activeGroup *domain.PaneGroupSnapshot
	var activeLane *domain.LaneSnapshot
	laneIdx := -1
	groupIdx := -1
	if site, ok := domain.LocatePaneInTab(tabSnap, paneID); ok {
		activeGroup = site.Group
		activeLane = site.Lane
		laneIdx = site.LaneIndex
		groupIdx = site.GroupIndex
	}
	if activeGroup == nil {
		fmt.Fprintf(out, "\n[rysh] pane %s not found in any group\n", paneID)
		w.failRysh("pane %s not found in any group", paneID)
		return
	}

	fmt.Fprintf(out, "\n[rysh] pane group info\n")
	ryshWriter(out).Rule()
	fmt.Fprintf(out, "  id          : %s\n", activeGroup.ID)
	fmt.Fprintf(out, "  tab         : %s\n", tab.title)
	fmt.Fprintf(out, "  lane        : %d (flex=%d, id=%.8s)\n", laneIdx+1, activeLane.Flex, activeLane.ID)
	fmt.Fprintf(out, "  group       : %d of %d\n", groupIdx+1, len(activeLane.PaneGroups))
	fmt.Fprintf(out, "  panes       : %d\n", len(activeGroup.Panes))
	fmt.Fprintf(out, "  active pane : %s\n", activeGroup.ActivePaneID)
	ryshWriter(out).Rule()
	if len(activeGroup.Panes) > 0 {
		fmt.Fprintf(out, "  panes in this group:\n")
		for pi, p := range activeGroup.Panes {
			pMarker := "    "
			if p.ID == paneID {
				pMarker = "  > "
			}
			fmt.Fprintf(out, "%s[%d] %-16s  status=%-10s  id=%s\n",
				pMarker, pi+1, p.Title, p.Status, p.ID)
		}
		ryshWriter(out).Rule()
	}
}

func (w *WorkspaceActor) cmdPaneGroupLayout(out *strings.Builder, paneID string) {
	tab := w.currentTab()
	if tab == nil {
		fmt.Fprintf(out, "\n[rysh] no active tab\n")
		w.failRysh("no active tab")
		return
	}
	tabSnap := w.queryTabSnapshot(tab.id)
	if tabSnap == nil {
		fmt.Fprintf(out, "\n[rysh] could not fetch tab snapshot\n")
		w.failRysh("could not fetch tab snapshot")
		return
	}

	totalPanes := domain.CountPanesInTab(tabSnap)
	totalGroups := domain.CountGroupsInTab(tabSnap)

	fmt.Fprintf(out, "\n[rysh] layout for tab %q  (%d lanes, %d groups, %d panes)\n",
		tab.title, len(tabSnap.Lanes), totalGroups, totalPanes)
	ryshWriter(out).Rule()

	for li, lane := range tabSnap.Lanes {
		fmt.Fprintf(out, "  lane %d (flex=%d) ", li+1, lane.Flex)
		ryshWriter(out).Rule()
		for gi, g := range lane.PaneGroups {
			marker := "  "
			isActive := false
			for _, p := range g.Panes {
				if p.ID == paneID {
					isActive = true
					break
				}
			}
			if isActive {
				marker = "> "
			}
			paneNames := make([]string, len(g.Panes))
			for pi, p := range g.Panes {
				paneNames[pi] = p.Title
			}
			fmt.Fprintf(out, "    %sgroup %d  panes: %s\n",
				marker, gi+1, strings.Join(paneNames, ", "))
		}
	}

	fmt.Fprintf(out, "\n  > = active pane group\n")
}

// cmdPipelinePlaceholderList shows the pipeline placeholder (lane) structure.
func (w *WorkspaceActor) cmdPipelinePlaceholderList(out *strings.Builder, tab *tabInfo) {
	tabSnap := w.queryTabSnapshot(tab.id)
	if tabSnap == nil {
		fmt.Fprintf(out, "\n[pipeline] could not fetch tab snapshot\n")
		w.failRysh("could not fetch tab snapshot")
		return
	}

	fmt.Fprintf(out, "\n[pipeline] placeholders in tab %q (%d lanes)\n", tab.title, len(tabSnap.Lanes))
	ryshWriter(out).Rule()
	for li, lane := range tabSnap.Lanes {
		totalPanes := domain.CountPanesInLane(&lane)
		fmt.Fprintf(out, "  placeholder %d  %q (flex=%d, groups=%d, panes=%d)\n",
			li+1, lane.Name, lane.Flex, len(lane.PaneGroups), totalPanes)
		for gi, g := range lane.PaneGroups {
			for pi, p := range g.Panes {
				marker := "    "
				if p.ID == w.activePaneID {
					marker = "  > "
				}
				fmt.Fprintf(out, "%s[group %d, pane %d] %-16s  id=%.8s\n",
					marker, gi+1, pi+1, p.Title, p.ID)
			}
		}
	}
	ryshWriter(out).Rule()
}

// handleApprovalPaneCommand handles the ##pane approval-pane subcommand.
func (w *WorkspaceActor) handleApprovalPaneCommand(out *strings.Builder, paneID string, args []string) {
	if len(args) == 0 {
		ryshWriter(out).UsageLine("##pane approval-pane <name1> [name2] ... | clear | list | enable-attention | disable-attention")
		w.failRyshUsage("usage: %s", "##pane approval-pane <name1> [name2] ... | clear | list | enable-attention | disable-attention")
		return
	}

	switch args[0] {
	case "clear":
		_ = w.pub.Send(msg.T("pane", paneID, "inbox"), &msg.MsgPaneSetApprovalPaneGroups{PaneGroupIDs: nil})
		_ = w.pub.Send(msg.T("pane", paneID, "llm_prompt_execution", "inbox"), &msg.MsgPaneSetApprovalPaneGroups{PaneGroupIDs: nil})
		fmt.Fprintf(out, "\n[rysh] approval pane groups cleared. Approvals will show in this pane.\n")

	case "list":
		// Get the pane's snapshot to read its configured approval groups.
		tab := w.currentTab()
		if tab == nil {
			fmt.Fprintf(out, "\n[rysh] no active tab\n")
			w.failRysh("no active tab")
			return
		}
		tabSnap := w.queryTabSnapshot(tab.id)
		if tabSnap == nil {
			fmt.Fprintf(out, "\n[rysh] could not fetch snapshot\n")
			w.failRysh("could not fetch snapshot")
			return
		}
		for _, lane := range tabSnap.Lanes {
			for _, g := range lane.PaneGroups {
				for _, ps := range g.Panes {
					if ps.ID == paneID {
						if len(ps.ApprovalPaneGroups) == 0 {
							fmt.Fprintf(out, "\n[rysh] no approval pane groups configured (approvals show in this pane)\n")
						} else {
							fmt.Fprintf(out, "\n[rysh] approval pane groups: %v\n", ps.ApprovalPaneGroups)
						}
						return
					}
				}
			}
		}
		fmt.Fprintf(out, "\n[rysh] pane not found in snapshot\n")
		w.failRysh("pane not found in snapshot")

	case "enable-attention":
		_ = w.pub.Send(msg.T("pane", paneID, "inbox"),
			&msg.MsgAttentionEnable{PaneID: paneID})
		fmt.Fprintf(out, "\n[rysh] approval attention enable requested\n")

	case "disable-attention":
		_ = w.pub.Send(msg.T("pane", paneID, "inbox"),
			&msg.MsgAttentionDisable{PaneID: paneID})
		fmt.Fprintf(out, "\n[rysh] approval attention disabled\n")

	default:
		// Resolve pane names to pane group IDs.
		var groupIDs []string
		seen := make(map[string]bool)
		for _, name := range args {
			targetPaneID := w.resolvePaneID(name)
			if targetPaneID == "" {
				fmt.Fprintf(out, "\n[rysh] pane not found: %s\n", name)
				w.failRysh("pane not found: %s", name)
				return
			}
			groupID := w.findPaneGroupID(targetPaneID)
			if groupID == "" {
				fmt.Fprintf(out, "\n[rysh] pane group not found for: %s\n", name)
				w.failRysh("pane group not found for: %s", name)
				return
			}
			if !seen[groupID] {
				groupIDs = append(groupIDs, groupID)
				seen[groupID] = true
			}
		}

		// Send to PaneActor.
		_ = w.pub.Send(msg.T("pane", paneID, "inbox"), &msg.MsgPaneSetApprovalPaneGroups{PaneGroupIDs: groupIDs})
		// Also update LLMPromptExecutionActor.
		_ = w.pub.Send(msg.T("pane", paneID, "llm_prompt_execution", "inbox"), &msg.MsgPaneSetApprovalPaneGroups{PaneGroupIDs: groupIDs})

		fmt.Fprintf(out, "\n[rysh] approval panes set to groups: %v\n", groupIDs)
	}
}
