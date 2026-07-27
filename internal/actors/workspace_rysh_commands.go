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
		return
	}
	tabSnap := w.queryTabSnapshot(tab.id)
	if tabSnap == nil {
		fmt.Fprintf(out, "\n[rysh] could not fetch tab snapshot\n")
		return
	}

	fmt.Fprintf(out, "\n[rysh] lanes in tab %q (%d lanes)\n", tab.title, len(tabSnap.Lanes))
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 60))
	for li, lane := range tabSnap.Lanes {
		totalPanes := 0
		for _, g := range lane.PaneGroups {
			totalPanes += len(g.Panes)
		}
		marker := "  "
		if lane.ActivePaneID == w.activePaneID {
			marker = "> "
		}
		fmt.Fprintf(out, "%s[%d] %-16s id=%.8s  flex=%d  groups=%d  panes=%d\n",
			marker, li+1, lane.Name, lane.ID, lane.Flex, len(lane.PaneGroups), totalPanes)
	}
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 60))
}

func (w *WorkspaceActor) cmdLaneInfo(out *strings.Builder, paneID string) {
	tab := w.currentTab()
	if tab == nil {
		fmt.Fprintf(out, "\n[rysh] no active tab\n")
		return
	}
	if paneID == "" {
		fmt.Fprintf(out, "\n[rysh] no active pane\n")
		return
	}
	tabSnap := w.queryTabSnapshot(tab.id)
	if tabSnap == nil {
		fmt.Fprintf(out, "\n[rysh] could not fetch tab snapshot\n")
		return
	}

	// Find the lane containing the active pane.
	var activeLane *domain.LaneSnapshot
	laneIdx := -1
	for li := range tabSnap.Lanes {
		for _, g := range tabSnap.Lanes[li].PaneGroups {
			for _, p := range g.Panes {
				if p.ID == paneID {
					activeLane = &tabSnap.Lanes[li]
					laneIdx = li
					break
				}
			}
			if activeLane != nil {
				break
			}
		}
		if activeLane != nil {
			break
		}
	}
	if activeLane == nil {
		fmt.Fprintf(out, "\n[rysh] pane %s not found in any lane\n", paneID)
		return
	}

	totalPanes := 0
	for _, g := range activeLane.PaneGroups {
		totalPanes += len(g.Panes)
	}

	fmt.Fprintf(out, "\n[rysh] lane info\n")
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 55))
	fmt.Fprintf(out, "  name        : %s\n", activeLane.Name)
	fmt.Fprintf(out, "  id          : %s\n", activeLane.ID)
	fmt.Fprintf(out, "  tab         : %s\n", tab.title)
	fmt.Fprintf(out, "  position    : %d of %d\n", laneIdx+1, len(tabSnap.Lanes))
	fmt.Fprintf(out, "  flex        : %d\n", activeLane.Flex)
	fmt.Fprintf(out, "  groups      : %d\n", len(activeLane.PaneGroups))
	fmt.Fprintf(out, "  panes       : %d\n", totalPanes)
	fmt.Fprintf(out, "  active pane : %s\n", activeLane.ActivePaneID)
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 55))
}

// ---------------------------------------------------------------------------
// ##public subcommands
// ---------------------------------------------------------------------------

func (w *WorkspaceActor) cmdPublicPanePrint(out *strings.Builder, paneID string) {
	if paneID == "" {
		fmt.Fprintf(out, "\n[rysh] no active pane\n")
		return
	}
	tab := w.currentTab()
	if tab == nil {
		fmt.Fprintf(out, "\n[rysh] no active tab\n")
		return
	}
	snap := tab.actor.PaneSnapshot(paneID)
	if snap == nil {
		fmt.Fprintf(out, "\n[rysh] pane %s not found\n", paneID[:8])
		return
	}
	trimmed := strings.TrimSpace(snap.Output)
	if trimmed == "" {
		fmt.Fprintf(out, "\n[rysh] output for pane %s is empty\n", paneID[:8])
		return
	}
	lines := strings.Split(trimmed, "\n")
	fmt.Fprintf(out, "\n[rysh] output for pane %s (%d lines)\n", paneID[:8], len(lines))
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 60))
	for _, line := range lines {
		fmt.Fprintf(out, ">>> %s\n", line)
	}
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 60))
}

// ---------------------------------------------------------------------------
// ##private subcommands
// ---------------------------------------------------------------------------

func (w *WorkspaceActor) cmdPrivatePanePrint(out *strings.Builder, paneID string) {
	if paneID == "" {
		fmt.Fprintf(out, "\n[rysh] no active pane\n")
		return
	}
	tab := w.currentTab()
	if tab == nil {
		fmt.Fprintf(out, "\n[rysh] no active tab\n")
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
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 60))
	for _, line := range lines {
		fmt.Fprintf(out, ">>> %s\n", line)
	}
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 60))
}

// ---------------------------------------------------------------------------
// ##snap subcommand
// ---------------------------------------------------------------------------

func (w *WorkspaceActor) cmdSnap(out *strings.Builder, paneID, target string) {
	if paneID == "" {
		fmt.Fprintf(out, "\n[rysh] no active pane\n")
		return
	}
	tab := w.currentTab()
	if tab == nil {
		fmt.Fprintf(out, "\n[rysh] no active tab\n")
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
		fmt.Fprintf(out, "  ##snap             copy private buffer to clipboard (default)\n")
		fmt.Fprintf(out, "  ##snap private     copy private buffer to clipboard\n")
		fmt.Fprintf(out, "  ##snap public      copy public (redacted) buffer to clipboard\n\n")
		return
	}

	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		fmt.Fprintf(out, "\n[rysh] %s buffer for pane %s is empty -- nothing to copy\n", label, paneID[:8])
		return
	}

	if err := clipboard.WriteAll(trimmed); err != nil {
		fmt.Fprintf(out, "\n[rysh] failed to copy to clipboard: %v\n", err)
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
		return
	}
	tabSnap := w.queryTabSnapshot(tab.id)
	if tabSnap == nil {
		fmt.Fprintf(out, "\n[rysh] could not fetch tab snapshot\n")
		return
	}

	totalGroups := 0
	for _, lane := range tabSnap.Lanes {
		totalGroups += len(lane.PaneGroups)
	}

	fmt.Fprintf(out, "\n[rysh] pane groups in tab %q (%d groups, %d lanes)\n",
		tab.title, totalGroups, len(tabSnap.Lanes))
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 70))

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
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 70))
}

func (w *WorkspaceActor) cmdPaneGroupInfo(out *strings.Builder, paneID string) {
	tab := w.currentTab()
	if tab == nil {
		fmt.Fprintf(out, "\n[rysh] no active tab\n")
		return
	}
	if paneID == "" {
		fmt.Fprintf(out, "\n[rysh] no active pane\n")
		return
	}
	tabSnap := w.queryTabSnapshot(tab.id)
	if tabSnap == nil {
		fmt.Fprintf(out, "\n[rysh] could not fetch tab snapshot\n")
		return
	}

	var activeGroup *domain.PaneGroupSnapshot
	var activeLane *domain.LaneSnapshot
	laneIdx := -1
	groupIdx := -1
	for li := range tabSnap.Lanes {
		for gi := range tabSnap.Lanes[li].PaneGroups {
			for _, p := range tabSnap.Lanes[li].PaneGroups[gi].Panes {
				if p.ID == paneID {
					activeGroup = &tabSnap.Lanes[li].PaneGroups[gi]
					activeLane = &tabSnap.Lanes[li]
					laneIdx = li
					groupIdx = gi
					break
				}
			}
			if activeGroup != nil {
				break
			}
		}
		if activeGroup != nil {
			break
		}
	}
	if activeGroup == nil {
		fmt.Fprintf(out, "\n[rysh] pane %s not found in any group\n", paneID)
		return
	}

	fmt.Fprintf(out, "\n[rysh] pane group info\n")
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 55))
	fmt.Fprintf(out, "  id          : %s\n", activeGroup.ID)
	fmt.Fprintf(out, "  tab         : %s\n", tab.title)
	fmt.Fprintf(out, "  lane        : %d (flex=%d, id=%.8s)\n", laneIdx+1, activeLane.Flex, activeLane.ID)
	fmt.Fprintf(out, "  group       : %d of %d\n", groupIdx+1, len(activeLane.PaneGroups))
	fmt.Fprintf(out, "  panes       : %d\n", len(activeGroup.Panes))
	fmt.Fprintf(out, "  active pane : %s\n", activeGroup.ActivePaneID)
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 55))
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
		fmt.Fprintf(out, "%s\n", strings.Repeat("-", 55))
	}
}

func (w *WorkspaceActor) cmdPaneGroupLayout(out *strings.Builder, paneID string) {
	tab := w.currentTab()
	if tab == nil {
		fmt.Fprintf(out, "\n[rysh] no active tab\n")
		return
	}
	tabSnap := w.queryTabSnapshot(tab.id)
	if tabSnap == nil {
		fmt.Fprintf(out, "\n[rysh] could not fetch tab snapshot\n")
		return
	}

	totalPanes := 0
	totalGroups := 0
	for _, lane := range tabSnap.Lanes {
		totalGroups += len(lane.PaneGroups)
		for _, g := range lane.PaneGroups {
			totalPanes += len(g.Panes)
		}
	}

	fmt.Fprintf(out, "\n[rysh] layout for tab %q  (%d lanes, %d groups, %d panes)\n",
		tab.title, len(tabSnap.Lanes), totalGroups, totalPanes)
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 60))

	for li, lane := range tabSnap.Lanes {
		fmt.Fprintf(out, "  lane %d (flex=%d) ", li+1, lane.Flex)
		fmt.Fprintf(out, "%s\n", strings.Repeat("-", 35))
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
		return
	}

	fmt.Fprintf(out, "\n[pipeline] placeholders in tab %q (%d lanes)\n", tab.title, len(tabSnap.Lanes))
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 60))
	for li, lane := range tabSnap.Lanes {
		totalPanes := 0
		for _, g := range lane.PaneGroups {
			totalPanes += len(g.Panes)
		}
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
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 60))
}

func (w *WorkspaceActor) ryshHelp(out *strings.Builder) {
	fmt.Fprintf(out, "\navailable ## commands:\n")
	fmt.Fprintf(out, "  ##help                       show this help\n")
	fmt.Fprintf(out, "  ##h, ##history               shell or AI prompt history (based on current mode)\n")
	fmt.Fprintf(out, "  ##native [on|off]            native pass-through shell: bash owns the terminal (Esc Esc exits)\n")
	fmt.Fprintf(out, "  ##new tab                    create a new tab (also: ##rysh new tab)\n")
	fmt.Fprintf(out, "  ##new lane [tab]             create a new lane (default: active tab)\n")
	fmt.Fprintf(out, "  ##new pane [tab] [lane]      create a pane at the bottom of a lane (default: active tab+lane)\n")
	fmt.Fprintf(out, "  ##new grid <N>               stack N panes vertically in the active lane (e.g. ##new grid 4)\n")
	fmt.Fprintf(out, "  ##new grid <L>x<P>           build an L x P grid in the active tab (e.g. ##new grid 3x4)\n")
	fmt.Fprintf(out, "  ##new grid <T>x<L>x<P>       create T tabs x L lanes x P panes (e.g. ##new grid 2x3x4)\n")
	fmt.Fprintf(out, "  ##new stack <N>              add N stacked panes to the active pane group (aliases: ##new pg|panegroup <N>)\n")
	fmt.Fprintf(out, "  ##cmd <scope> [sel] <cmd>    run a bash cmd in every pane of a scope (pane|pg/stack|lane|tab|ws)\n")
	fmt.Fprintf(out, "                               selectors: --ws/--tab/--lane/--pg/--pane <id|name|index>; e.g. ##cmd stack pwd\n")
	fmt.Fprintf(out, "                               (shared panes and panes in a pipeline tab are excluded)\n")
	fmt.Fprintf(out, "  ##llm [list]                 list LLM providers/models from .rysh/llms (session default marked)\n")
	fmt.Fprintf(out, "  ##llm add <p>/<name> [id]    declare a new model in .rysh/llms\n")
	fmt.Fprintf(out, "  ##llm use <provider>/<name>  set the session default LLM model (persists to .rysh/llms)\n")
	fmt.Fprintf(out, "  ##llm info <provider>/<name> show a model's properties\n")
	fmt.Fprintf(out, "  ##llm status                 show the model currently in effect\n")
	fmt.Fprintf(out, "  ##session                    show current session details (alias: ##session info)\n")
	fmt.Fprintf(out, "  ##session list               list all known sessions (current marked with >)\n")
	fmt.Fprintf(out, "  ##session switch <name>      start another session's daemon + how to attach\n")
	fmt.Fprintf(out, "  ##session reload             flush this session's state to KV and refresh its record\n")
	fmt.Fprintf(out, "  ##ws, ##workspace            list workspaces (default: list)\n")
	fmt.Fprintf(out, "  ##ws create <name> <api_key> create a workspace (working_directory ~/, upstream key) + persist it\n")
	fmt.Fprintf(out, "  ##tab                        list all tabs (default: list)\n")
	fmt.Fprintf(out, "  ##tab  list                  list all tabs\n")
	fmt.Fprintf(out, "  ##tab  list-panes [--tab <tab-id>]  list panes of a tab (default: active tab)\n")
	fmt.Fprintf(out, "  ##tab  name <tab-name>       rename the active tab (also: ##rysh tab name, ctrl+t r)\n")
	fmt.Fprintf(out, "  ##tab  name <tab-id> <name>  rename the tab with that id (see ##tab list)\n")
	fmt.Fprintf(out, "  ##tab  pipeline enable       enable pipeline mode for the active tab\n")
	fmt.Fprintf(out, "  ##tab  pipeline disable      disable pipeline mode for the active tab\n")
	fmt.Fprintf(out, "  ##tab  delete <tab-id>       delete the tab with that id (see ##tab list)\n")
	fmt.Fprintf(out, "  ##pane                       show active pane details (default: info)\n")
	fmt.Fprintf(out, "  ##pane info                  show active pane details\n")
	fmt.Fprintf(out, "  ##pane new [--worktree [branch]]  new pane; --worktree runs it in its own git worktree\n")
	fmt.Fprintf(out, "                               (branch pane/<alias>; removed on close if clean, KEPT if dirty)\n")
	fmt.Fprintf(out, "  ##pane list [--tab <tab-id>] list panes of a tab (default: active tab)\n")
	fmt.Fprintf(out, "  ##pane name <name>           set a given-name for the active pane (unique per lane)\n")
	fmt.Fprintf(out, "  ##pane name <pane-id> <name> set a given-name for the pane with that id (see ##pane list)\n")
	fmt.Fprintf(out, "  ##pane listen <id|alias>     listen to another pane's shared output\n")
	fmt.Fprintf(out, "  ##pane unlisten              stop listening\n")
	fmt.Fprintf(out, "  ##pane provider [name [model]] show or override the active pane's LLM provider (persisted; \"default\" clears)\n")
	fmt.Fprintf(out, "  ##pane delete <pane-id>      delete the pane with that id (see ##pane list)\n")
	fmt.Fprintf(out, "  ##pane share start           start sharing pane to remote upstream\n")
	fmt.Fprintf(out, "  ##pane share stop            stop sharing pane\n")
	fmt.Fprintf(out, "  ##pane share status          show sharing status\n")
	fmt.Fprintf(out, "  ##share pane [view|control]   share the active pane to upstream\n")
	fmt.Fprintf(out, "  ##share panegroup [view|ctrl] share the active pane group\n")
	fmt.Fprintf(out, "  ##share lane [view|control]   share the active lane\n")
	fmt.Fprintf(out, "  ##share tab [view|control]    share the active tab\n")
	fmt.Fprintf(out, "  ##share list                  list all active shares\n")
	fmt.Fprintf(out, "  ##share status                show share status for active pane\n")
	fmt.Fprintf(out, "  ##unshare pane                stop sharing the active pane\n")
	fmt.Fprintf(out, "  ##unshare panegroup           stop sharing the active pane group\n")
	fmt.Fprintf(out, "  ##unshare lane                stop sharing the active lane\n")
	fmt.Fprintf(out, "  ##unshare tab                 stop sharing the active tab\n")
	fmt.Fprintf(out, "  ##upstream status             show upstream configuration and status\n")
	fmt.Fprintf(out, "  ##upstream my-shares          list shares published from this session\n")
	fmt.Fprintf(out, "  ##upstream list-remote        list all shares in the workspace (from server)\n")
	fmt.Fprintf(out, "  ##upstream subscribe <id>     subscribe to a remote share\n")
	fmt.Fprintf(out, "  ##upstream unsubscribe        stop subscribing to remote share\n")
	fmt.Fprintf(out, "  ##upstream send <text>        send command to active remote share\n")
	fmt.Fprintf(out, "  ####<command>                 run ##<command> on the shared source pane (control subscriber)\n")
	fmt.Fprintf(out, "  ##lane list                  list lanes in the active tab\n")
	fmt.Fprintf(out, "  ##lane info                  show active lane details\n")
	fmt.Fprintf(out, "  ##lane name <lane-name>      rename the active lane (also: ##rysh lane name)\n")
	fmt.Fprintf(out, "  ##lane delete <lane-id>      delete the lane with that id (see ##lane list)\n")
	fmt.Fprintf(out, "  ##panegroup, ##pg            show active pane group details (default: info)\n")
	fmt.Fprintf(out, "  ##panegroup info             show active pane group details\n")
	fmt.Fprintf(out, "  ##panegroup list             list pane groups in the active tab\n")
	fmt.Fprintf(out, "  ##panegroup layout           show lane layout overview\n")
	fmt.Fprintf(out, "  ##panegroup delete <group-id> delete the pane group with that id (see ##panegroup list)\n")
	fmt.Fprintf(out, "  ##public pane print          print redacted (public) output of the active pane\n")
	fmt.Fprintf(out, "  ##private pane print         print raw (private) output of the active pane\n")
	fmt.Fprintf(out, "  ##snap                       copy private buffer to clipboard (default)\n")
	fmt.Fprintf(out, "  ##snap private               copy private buffer to clipboard\n")
	fmt.Fprintf(out, "  ##snap public                copy public (redacted) buffer to clipboard\n")
	fmt.Fprintf(out, "  ##pipe, ##pipeline           pipeline commands (default: help)\n")
	fmt.Fprintf(out, "  ##pipe list                  list loaded pipelines\n")
	fmt.Fprintf(out, "  ##pipe load <file>           load a pipeline from .rysh/pipelines/<file>\n")
	fmt.Fprintf(out, "  ##pipe unload <name>         remove a loaded pipeline\n")
	fmt.Fprintf(out, "  ##pipe run [name]            run a pipeline (default: first loaded)\n")
	fmt.Fprintf(out, "  ##pipe status                show pipeline execution status\n")
	fmt.Fprintf(out, "  ##pipe clear                 clear pipeline output\n")
	fmt.Fprintf(out, "  ##pipe name <name>           set the pipeline name label\n")
	fmt.Fprintf(out, "  ##pipe placeholder add       add a new pipeline placeholder (lane)\n")
	fmt.Fprintf(out, "  ##pipe placeholder list      list current placeholders\n")
	fmt.Fprintf(out, "  ##rysh web start [--bind <addr>] [--port <n>] [--token <t>|--no-token]  start the web UI server\n")
	fmt.Fprintf(out, "  ##rysh web stop              stop the web UI server\n")
	fmt.Fprintf(out, "  ##rysh web status            show bind address, port + access token\n")
	fmt.Fprintf(out, "  ##rysh web token             print the web access token + open URL\n")
	fmt.Fprintf(out, "  ##mode list                  list enabled + available input modes for the active pane\n")
	fmt.Fprintf(out, "  ##mode new <mode>            enable a mode (shell|prompt(ai)|rysh|chat|external|web)\n")
	fmt.Fprintf(out, "  ##mode new web [--profile N] [url]  enable web mode bound to a browser profile\n")
	fmt.Fprintf(out, "  ##mode delete <mode>         disable a mode (shell cannot be disabled)\n")
	fmt.Fprintf(out, "  ##webai [--pane <id|name>] <prompt>  prompt a pane's web AI (default: this pane; --pane targets another)\n")
	fmt.Fprintf(out, "  ##webai [--pane <id|name>] history [print] [N]  print a pane's last N Ask Rysh turns (default 5)\n")
	fmt.Fprintf(out, "  ##auto web|task|agent|humanoid|code save|run|list|show|delete  reusable prompt automations (web recipes, plain tasks, named agents/humanoids, project code)\n")
	fmt.Fprintf(out, "  ##web headless on|off|status|login    CLI-owned headless Chromium (runs without the desktop app)\n")
	fmt.Fprintf(out, "  ##hop <pane-name|pane-id>    hop output + agent memory to another pane (session fork)\n")
	fmt.Fprintf(out, "  ##hop resume                 resume the AI with the hopped session\n")
	fmt.Fprintf(out, "  ##hop status                 show hop state for active pane\n")
	fmt.Fprintf(out, "  ##hop clear                  clear hopped content\n")
	fmt.Fprintf(out, "  ##grounding                  show grounding state for active pane\n")
	fmt.Fprintf(out, "  ##grounding off|prompt|enforced  override grounding mode (persisted per pane)\n")
	fmt.Fprintf(out, "  ##grounding reset            clear the override, revert to default\n")
	fmt.Fprintf(out, "  ##grounding report           show the last grounding report\n")
	fmt.Fprintf(out, "  ##cron add|list|run|rm|logs  schedule rysh inputs (##auto web, @agent, prompts) in-daemon\n")
	fmt.Fprintf(out, "  ##>...                       event pass-through (forwarded to NATS as-is)\n")
	fmt.Fprintf(out, "  ##agent spawn <name>          load .rysh/agents/<name>/SKILL.md\n")
	fmt.Fprintf(out, "  ##agent spawn <name> <prompt> spawn agent inline\n")
	fmt.Fprintf(out, "  ##agent spawn-all [dir]       spawn all (default: .rysh/agents)\n")
	fmt.Fprintf(out, "  ##agent list                  list all autonomous agents\n")
	fmt.Fprintf(out, "  ##agent show <name>           print an agent's recipe (its skill file)\n")
	fmt.Fprintf(out, "  ##agent delete <name>         delete an agent\n")
	fmt.Fprintf(out, "  ##agent register-output       route agent output to pane chat\n")
	fmt.Fprintf(out, "  ##agent unregister-output      stop routing output to pane\n")
	fmt.Fprintf(out, "  ##agent reload-prompts        reload rysh-cli-agent-prompts/ + override dir (effective next prompt)\n")
	fmt.Fprintf(out, "  ##agent metrics               dump per-tool / LLM / compaction metrics\n")
	fmt.Fprintf(out, "  ##humanoid spawn <name>       load .rysh/humanoids/<name>/SKILL.md (agent + external channels)\n")
	fmt.Fprintf(out, "  ##humanoid spawn-all [dir]    spawn all (default: .rysh/humanoids)\n")
	fmt.Fprintf(out, "  ##humanoid stop <name>        stop a running humanoid (its skill file is kept)\n")
	fmt.Fprintf(out, "  ##humanoid list               running + paused + stopped humanoids, with channel status\n")
	fmt.Fprintf(out, "  ##humanoid show <name>        print a humanoid's recipe (its skill file)\n")
	fmt.Fprintf(out, "  ##humanoid channels <name>    show a humanoid's configured channels\n")
	fmt.Fprintf(out, "  ##humanoid channel start|stop <name> <channel>  connect/disconnect one channel\n")
	fmt.Fprintf(out, "  ##humanoid governance <name> ai|human  autonomous, or draft-and-confirm before each reply\n")
	fmt.Fprintf(out, "  ##humanoid pair list|approve  review and approve inbound contact pairing requests\n")
	fmt.Fprintf(out, "  ##humanoid activate|deactivate <name>  bench or revive a humanoid\n")
	fmt.Fprintf(out, "  ##humanoid register-output|unregister-output  route humanoid output to pane chat\n")
	fmt.Fprintf(out, "  ##image <path>                attach an image to the next prompt in this pane\n")
	fmt.Fprintf(out, "  ##image clear                 clear any pending image\n")
	fmt.Fprintf(out, "  @agent-name <prompt>          send prompt to autonomous agent or humanoid\n")
	fmt.Fprintf(out, "  @@agent-name stop             stop autonomous agent or humanoid\n")
	fmt.Fprintf(out, "  ##secret new <NAME> <VALUE> [--no-persist] [--tab <tab>]  workspace secret (--tab for a tab); persisted to .rysh/secrets/<scope>/<NAME> by default\n")
	fmt.Fprintf(out, "  ##secret list [--tab <tab>]   list the workspace's (or a tab's) secrets\n")
	fmt.Fprintf(out, "  ##secret get <NAME> [--tab <tab>]   resolve a secret as a skill here would (tab→workspace→env)\n")
	fmt.Fprintf(out, "  ##secret delete <NAME> [--tab <tab>]  remove a session + persisted secret (alias: ##secrets)\n")
	fmt.Fprintf(out, "  ##variable new <NAME> <VALUE> [--no-persist] [--tab <tab>]  env variable (.rysh/variables); like ##secret but visible to the LLM (aliases: ##var, ##variables)\n")
	fmt.Fprintf(out, "  ##variable list|get|delete [--tab <tab>]   list/resolve/remove workspace (or tab) variables\n")
	fmt.Fprintf(out, "  ##snat status|on|off|reset|mode|list  SecretNAT/ReSet: reversible secret translation — the LLM provider\n")
	fmt.Fprintf(out, "                                never sees real secrets; tools still get real values (alias: ##rst)\n")
	fmt.Fprintf(out, "  ##proxy [status]              governance proxy: route wrapped agent CLIs through rysh\n")
	fmt.Fprintf(out, "  ##proxy on|off                enable/disable the proxy for this session (default: off)\n")
	fmt.Fprintf(out, "  ##proxy audit                 show the proxied-request audit log\n")
	fmt.Fprintf(out, "  ##cost                        token + dollar spend for this session\n")
	fmt.Fprintf(out, "  ##cost week|7d                spend over the last 7 days\n")
	fmt.Fprintf(out, "  ##cost budget <n>             set a spend ceiling (e.g. ##cost budget 500k)\n")
	fmt.Fprintf(out, "  ##policy                      show the resolved policy (.rysh/policy.yaml + org policy; fail-closed)\n")
	fmt.Fprintf(out, "  ##policy reload               re-read the policy file(s)\n")
	fmt.Fprintf(out, "  ##replay status               session capture state (opt-in per session)\n")
	fmt.Fprintf(out, "  ##replay export [--pane <id>] [-o <file>]  export captured output as asciicast v2\n")
	fmt.Fprintf(out, "  ##replay play [--pane <id>] [--from <dur|ts>] [--speed <n|max>]  replay into a dedicated read-only pane\n")
	fmt.Fprintf(out, "                                (focused replay pane: space pause, ←/→ seek ∓10s, +/- speed, q close)\n")
	fmt.Fprintf(out, "  ##replay play --here          v1 behavior: replay into this pane instead\n")
	fmt.Fprintf(out, "  ##replay stop                 cancel an in-progress replay\n")
	fmt.Fprintf(out, "  ##worktree new <branch>       create a git worktree for isolated agent work\n")
	fmt.Fprintf(out, "  ##worktree list|status        list worktrees / show the active one\n")
	fmt.Fprintf(out, "  ##worktree cwd <branch>       run this pane group in the worktree (new panes start there)\n")
	fmt.Fprintf(out, "  ##worktree merge <branch> [--confirm]  gated merge back (shows a diff first)\n")
	fmt.Fprintf(out, "  ##worktree remove <branch>    remove a worktree (dirty trees are preserved)\n")
	fmt.Fprintf(out, "  ##mcp add <name> http <url>   connect an MCP server (tools→agents)\n")
	fmt.Fprintf(out, "  ##mcp add <name> stdio <cmd>  spawn a stdio MCP server\n")
	fmt.Fprintf(out, "  ##mcp list                    list MCP servers + status\n")
	fmt.Fprintf(out, "  ##mcp tools <name>            list a server's tools\n")
	fmt.Fprintf(out, "  ##mcp remove <name>           disconnect + forget a server\n")
	fmt.Fprintf(out, "  ##forge add <name> <spec>     ingest an API spec + generate artifacts\n")
	fmt.Fprintf(out, "  ##forge list                  list configured integrations\n")
	fmt.Fprintf(out, "  ##forge diff <name> <spec>    operation changes vs the stored spec\n")
	fmt.Fprintf(out, "  ##forge targets               list available generator targets\n")
	fmt.Fprintf(out, "  ##integration list            list Forge API integrations (alias: ##int)\n")
	fmt.Fprintf(out, "  ##integration enable <name>   register a generated integration's tools\n")
	fmt.Fprintf(out, "  ##integration tools <name>    list an integration's tools\n")
	fmt.Fprintf(out, "  (forge artifacts also build from the shell: rysh forge add <name> <spec-file>)\n")
	fmt.Fprintf(out, "\n")
}

// handleApprovalPaneCommand handles the ##pane approval-pane subcommand.
func (w *WorkspaceActor) handleApprovalPaneCommand(out *strings.Builder, paneID string, args []string) {
	if len(args) == 0 {
		fmt.Fprintf(out, "\n[rysh] usage: ##pane approval-pane <name1> [name2] ... | clear | list | enable-attention | disable-attention\n")
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
			return
		}
		tabSnap := w.queryTabSnapshot(tab.id)
		if tabSnap == nil {
			fmt.Fprintf(out, "\n[rysh] could not fetch snapshot\n")
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
				return
			}
			groupID := w.findPaneGroupID(targetPaneID)
			if groupID == "" {
				fmt.Fprintf(out, "\n[rysh] pane group not found for: %s\n", name)
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
