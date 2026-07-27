package actors

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/asynkron/protoactor-go/actor"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// cmdSelectors pins specific entities for a ##cmd broadcast. An empty field
// means "use the active entity at that level".
type cmdSelectors struct {
	ws   string
	tab  string
	lane string
	pg   string
	pane string
}

// parseCmdArgs parses the arguments of `##cmd`:
//
//	##cmd <scope> [--ws v] [--tab v] [--lane v] [--pg v] [--pane v] [--] <bash command...>
//
// <scope> is one of pane | panegroup|pg|stack | lane | tab | workspace|ws.
// Flag parsing stops at the first non-flag token (or an explicit "--");
// everything after that is the bash command. Scope aliases are normalized
// ("pg" and "stack" → "panegroup", "ws" → "workspace"). "stack" is offered
// because a pane group is exactly a stack of panes.
func parseCmdArgs(args []string) (scope string, sel cmdSelectors, command string, err error) {
	if len(args) == 0 {
		return "", sel, "", fmt.Errorf("usage: ##cmd <pane|pg|lane|tab|ws> [selectors] <command>")
	}
	scope = strings.ToLower(args[0])
	switch scope {
	case "pane", "panegroup", "pg", "stack", "lane", "tab", "workspace", "ws":
	default:
		return "", sel, "", fmt.Errorf("unknown scope %q (expected pane, pg/stack, lane, tab, or ws)", args[0])
	}

	rest := args[1:]
	i := 0
	for i < len(rest) {
		tok := rest[i]
		if tok == "--" {
			i++
			break
		}
		if !strings.HasPrefix(tok, "--") {
			break // first non-flag token: the command starts here
		}
		if i+1 >= len(rest) {
			return "", sel, "", fmt.Errorf("flag %s requires a value", tok)
		}
		val := rest[i+1]
		switch tok[2:] {
		case "ws", "workspace":
			sel.ws = val
		case "tab":
			sel.tab = val
		case "lane":
			sel.lane = val
		case "pg", "panegroup":
			sel.pg = val
		case "pane":
			sel.pane = val
		default:
			return "", sel, "", fmt.Errorf("unknown flag %s", tok)
		}
		i += 2
	}

	command = strings.TrimSpace(strings.Join(rest[i:], " "))
	if command == "" {
		return "", sel, "", fmt.Errorf("no command given; usage: ##cmd <scope> [selectors] <command>")
	}

	switch scope {
	case "pg", "stack":
		scope = "panegroup"
	case "ws":
		scope = "workspace"
	}
	return scope, sel, command, nil
}

// ---------------------------------------------------------------------------
// Snapshot-based scope resolution (pure helpers, unit-tested)
// ---------------------------------------------------------------------------

func resolveTabInSnapshot(snap *domain.WorkspaceSnapshot, arg string) *domain.TabSnapshot {
	if len(snap.Tabs) == 0 {
		return nil
	}
	if arg == "" {
		for i := range snap.Tabs {
			if snap.Tabs[i].ID == snap.ActiveTabID {
				return &snap.Tabs[i]
			}
		}
		return &snap.Tabs[0]
	}
	for i := range snap.Tabs {
		if snap.Tabs[i].ID == arg {
			return &snap.Tabs[i]
		}
	}
	if n, e := strconv.Atoi(arg); e == nil && n >= 1 && n <= len(snap.Tabs) {
		return &snap.Tabs[n-1]
	}
	for i := range snap.Tabs {
		if snap.Tabs[i].Title == arg {
			return &snap.Tabs[i]
		}
	}
	return nil
}

func laneContainsPane(lane *domain.LaneSnapshot, paneID string) bool {
	for gi := range lane.PaneGroups {
		if groupContainsPane(&lane.PaneGroups[gi], paneID) {
			return true
		}
	}
	return false
}

func resolveLaneInTabSnapshot(tab *domain.TabSnapshot, arg string) *domain.LaneSnapshot {
	if len(tab.Lanes) == 0 {
		return nil
	}
	if arg == "" {
		if tab.ActivePaneID != "" {
			for i := range tab.Lanes {
				if laneContainsPane(&tab.Lanes[i], tab.ActivePaneID) {
					return &tab.Lanes[i]
				}
			}
		}
		return &tab.Lanes[0]
	}
	for i := range tab.Lanes {
		if tab.Lanes[i].ID == arg {
			return &tab.Lanes[i]
		}
	}
	if n, e := strconv.Atoi(arg); e == nil && n >= 1 && n <= len(tab.Lanes) {
		return &tab.Lanes[n-1]
	}
	for i := range tab.Lanes {
		if tab.Lanes[i].Name == arg {
			return &tab.Lanes[i]
		}
	}
	return nil
}

func groupContainsPane(g *domain.PaneGroupSnapshot, paneID string) bool {
	for i := range g.Panes {
		if g.Panes[i].ID == paneID {
			return true
		}
	}
	return false
}

func resolveGroupInLaneSnapshot(lane *domain.LaneSnapshot, arg string) *domain.PaneGroupSnapshot {
	if len(lane.PaneGroups) == 0 {
		return nil
	}
	if arg == "" {
		if lane.ActivePaneID != "" {
			for i := range lane.PaneGroups {
				if groupContainsPane(&lane.PaneGroups[i], lane.ActivePaneID) {
					return &lane.PaneGroups[i]
				}
			}
		}
		return &lane.PaneGroups[0]
	}
	for i := range lane.PaneGroups {
		if lane.PaneGroups[i].ID == arg {
			return &lane.PaneGroups[i]
		}
	}
	if n, e := strconv.Atoi(arg); e == nil && n >= 1 && n <= len(lane.PaneGroups) {
		return &lane.PaneGroups[n-1]
	}
	return nil
}

func resolvePaneInGroupSnapshot(g *domain.PaneGroupSnapshot, arg string) *domain.PaneSnapshot {
	if len(g.Panes) == 0 {
		return nil
	}
	if arg == "" {
		if g.ActivePaneID != "" {
			for i := range g.Panes {
				if g.Panes[i].ID == g.ActivePaneID {
					return &g.Panes[i]
				}
			}
		}
		return &g.Panes[0]
	}
	for i := range g.Panes {
		p := &g.Panes[i]
		if p.ID == arg || p.Title == arg || p.GivenName == arg {
			return p
		}
	}
	return nil
}

// paneIDsInGroup returns the IDs of every pane in the group, excluding panes
// that are currently shared upstream with remote users (Sharing == true). The
// number of excluded panes is returned as skipped.
func paneIDsInGroup(g *domain.PaneGroupSnapshot) (ids []string, skipped int) {
	for i := range g.Panes {
		if g.Panes[i].Sharing {
			skipped++
			continue
		}
		ids = append(ids, g.Panes[i].ID)
	}
	return ids, skipped
}

func paneIDsInLane(l *domain.LaneSnapshot) (ids []string, skipped int) {
	for i := range l.PaneGroups {
		gi, gs := paneIDsInGroup(&l.PaneGroups[i])
		ids = append(ids, gi...)
		skipped += gs
	}
	return ids, skipped
}

func paneIDsInTab(t *domain.TabSnapshot) (ids []string, skipped int) {
	for i := range t.Lanes {
		li, ls := paneIDsInLane(&t.Lanes[i])
		ids = append(ids, li...)
		skipped += ls
	}
	return ids, skipped
}

func paneIDsInWorkspace(snap *domain.WorkspaceSnapshot) (ids []string, skipped int) {
	for i := range snap.Tabs {
		ti, ts := paneIDsInTab(&snap.Tabs[i])
		ids = append(ids, ti...)
		skipped += ts
	}
	return ids, skipped
}

// countPanesInGroup/Lane/Tab count every pane (including shared ones). Used to
// report how many panes were excluded because their tab is a pipeline.
func countPanesInGroup(g *domain.PaneGroupSnapshot) int { return len(g.Panes) }

func countPanesInLane(l *domain.LaneSnapshot) int {
	n := 0
	for i := range l.PaneGroups {
		n += countPanesInGroup(&l.PaneGroups[i])
	}
	return n
}

func countPanesInTab(t *domain.TabSnapshot) int {
	n := 0
	for i := range t.Lanes {
		n += countPanesInLane(&t.Lanes[i])
	}
	return n
}

// collectScopePaneIDs resolves the target pane IDs for a scope within snap,
// applying selector overrides (empty selector → active entity). Two classes of
// panes are excluded from the broadcast:
//   - panes shared upstream with remote users (Sharing == true) → skippedShared
//   - panes whose tab is a pipeline (TabSnapshot.PipelineEnabled) → skippedPipeline
//
// Pipeline membership is a tab-level property, so a pipeline-enabled tab excludes
// every pane in the requested scope. It also returns a human-readable label.
func collectScopePaneIDs(snap *domain.WorkspaceSnapshot, scope string, sel cmdSelectors) (ids []string, skippedShared, skippedPipeline int, label string, err error) {
	if scope == "workspace" {
		for i := range snap.Tabs {
			t := &snap.Tabs[i]
			if t.PipelineEnabled {
				skippedPipeline += countPanesInTab(t)
				continue
			}
			ti, ts := paneIDsInTab(t)
			ids = append(ids, ti...)
			skippedShared += ts
		}
		return ids, skippedShared, skippedPipeline, "the workspace", nil
	}
	tab := resolveTabInSnapshot(snap, sel.tab)
	if tab == nil {
		return nil, 0, 0, "", fmt.Errorf("tab not found: %q", sel.tab)
	}
	tabLabel := fmt.Sprintf("tab %q", tab.Title)
	if scope == "tab" {
		if tab.PipelineEnabled {
			return nil, 0, countPanesInTab(tab), tabLabel, nil
		}
		ids, skippedShared = paneIDsInTab(tab)
		return ids, skippedShared, 0, tabLabel, nil
	}
	lane := resolveLaneInTabSnapshot(tab, sel.lane)
	if lane == nil {
		return nil, 0, 0, "", fmt.Errorf("lane not found: %q", sel.lane)
	}
	laneLabel := fmt.Sprintf("lane %.8s of %s", lane.ID, tabLabel)
	if lane.Name != "" {
		laneLabel = fmt.Sprintf("lane %q of %s", lane.Name, tabLabel)
	}
	if scope == "lane" {
		if tab.PipelineEnabled {
			return nil, 0, countPanesInLane(lane), laneLabel, nil
		}
		ids, skippedShared = paneIDsInLane(lane)
		return ids, skippedShared, 0, laneLabel, nil
	}
	group := resolveGroupInLaneSnapshot(lane, sel.pg)
	if group == nil {
		return nil, 0, 0, "", fmt.Errorf("pane group not found: %q", sel.pg)
	}
	groupLabel := fmt.Sprintf("pane group %.8s of %s", group.ID, laneLabel)
	if scope == "panegroup" {
		if tab.PipelineEnabled {
			return nil, 0, countPanesInGroup(group), groupLabel, nil
		}
		ids, skippedShared = paneIDsInGroup(group)
		return ids, skippedShared, 0, groupLabel, nil
	}
	// scope == "pane"
	pane := resolvePaneInGroupSnapshot(group, sel.pane)
	if pane == nil {
		return nil, 0, 0, "", fmt.Errorf("pane not found: %q", sel.pane)
	}
	paneLabel := fmt.Sprintf("pane %.8s", pane.ID)
	if tab.PipelineEnabled {
		// A pane in a pipeline tab never receives a broadcast command.
		return nil, 0, 1, paneLabel, nil
	}
	if pane.Sharing {
		// A shared pane never receives a broadcast command.
		return nil, 1, 0, paneLabel, nil
	}
	return []string{pane.ID}, 0, 0, paneLabel, nil
}

// ---------------------------------------------------------------------------
// WorkspaceActor methods
// ---------------------------------------------------------------------------

// anchorSnapshotToPane re-points the snapshot's "active" markers — the defaults
// used when a ##cmd selector at a given level is omitted — at the pane that
// issued the command. Explicit selectors resolve by id/name/index and are
// unaffected. This keeps ##cmd operating on the tab/lane/group the user actually
// issued it from, even when w.activeTabIdx (→ snap.ActiveTabID) has drifted out
// of sync with the focused pane. No-op when paneID is empty (e.g. a cross-
// workspace broadcast, where the originating pane lives in another workspace).
func anchorSnapshotToPane(snap *domain.WorkspaceSnapshot, paneID string) {
	if paneID == "" {
		return
	}
	for ti := range snap.Tabs {
		t := &snap.Tabs[ti]
		for li := range t.Lanes {
			l := &t.Lanes[li]
			for gi := range l.PaneGroups {
				g := &l.PaneGroups[gi]
				if groupContainsPane(g, paneID) {
					snap.ActiveTabID = t.ID
					t.ActivePaneID = paneID
					l.ActivePaneID = paneID
					g.ActivePaneID = paneID
					return
				}
			}
		}
	}
}

// broadcastCmd resolves the scope against this workspace's own snapshot and
// sends the bash command to every target pane. Panes shared upstream with remote
// users, and panes whose tab is a pipeline, are excluded (reported via
// skippedShared / skippedPipeline). Returns the count of panes the command was
// sent to, the two skip counts, a label, or an error. Used both for the active
// workspace (Phase 1) and, via the Farm, for a sibling workspace (Phase 2).
//
// anchorPaneID is the pane that issued the command; when set and the scope uses
// the active-entity default, resolution is anchored to that pane's tab/lane/group
// rather than the (possibly stale) active-tab index. Empty for cross-workspace
// broadcasts.
func (w *WorkspaceActor) broadcastCmd(scope string, sel cmdSelectors, command, anchorPaneID string) (count, skippedShared, skippedPipeline int, label string, err error) {
	snap := w.collectSnapshot(false)
	anchorSnapshotToPane(&snap, anchorPaneID)
	ids, skippedShared, skippedPipeline, label, err := collectScopePaneIDs(&snap, scope, sel)
	if err != nil {
		return 0, 0, 0, "", err
	}
	for _, id := range ids {
		_ = w.pub.Send(msg.T("pane", id, "inbox"), &msg.MsgPaneExecShell{Command: command})
	}
	return len(ids), skippedShared, skippedPipeline, label, nil
}

// resolveCmdWorkspace interprets the --ws selector against the session's
// workspace list. Returns (targetName, isSibling, err):
//   - empty / the active workspace's own name / matching index → ("", false, nil): local.
//   - a known sibling (by name or 1-based index) → (name, true, nil): route via Farm.
//   - anything else → error.
func (w *WorkspaceActor) resolveCmdWorkspace(arg string) (string, bool, error) {
	if arg == "" {
		return "", false, nil
	}
	match := ""
	for _, n := range w.workspaceNames {
		if n == arg {
			match = n
			break
		}
	}
	if match == "" {
		if n, e := strconv.Atoi(arg); e == nil && n >= 1 && n <= len(w.workspaceNames) {
			match = w.workspaceNames[n-1]
		}
	}
	if match == "" {
		return "", false, fmt.Errorf("unknown workspace: %q", arg)
	}
	if match == w.workspaceName {
		return "", false, nil
	}
	return match, true, nil
}

// handleCmdBroadcast implements `##cmd <scope> [selectors] <bash command>`.
func (w *WorkspaceActor) handleCmdBroadcast(ctx actor.Context, out *strings.Builder, originPaneID string, args []string) {
	scope, sel, command, err := parseCmdArgs(args)
	if err != nil {
		fmt.Fprintf(out, "\n[rysh] %v\n", err)
		w.cmdUsage(out)
		return
	}

	// Cross-workspace: route to a sibling workspace (same session) via the Farm.
	target, isSibling, rerr := w.resolveCmdWorkspace(sel.ws)
	if rerr != nil {
		fmt.Fprintf(out, "\n[rysh] %v\n", rerr)
		return
	}
	if isSibling {
		sel.ws = "" // consumed; sibling resolves the rest against its own snapshot
		ctx.Send(ctx.Parent(), &farmBroadcastCmdMsg{
			targetWorkspace: target,
			scope:           scope,
			sel:             sel,
			command:         command,
		})
		fmt.Fprintf(out, "\n[rysh] ##cmd: dispatched %q to workspace %q\n", command, target)
		return
	}

	// Local (active) workspace. Anchor active-entity defaults to the originating
	// pane so the command targets the tab/lane/group the user is actually in,
	// not the (possibly stale) active tab.
	count, skippedShared, skippedPipeline, label, err := w.broadcastCmd(scope, sel, command, originPaneID)
	if err != nil {
		fmt.Fprintf(out, "\n[rysh] %v\n", err)
		return
	}
	fmt.Fprintf(out, "\n[rysh] ##cmd: ran %q in %d pane(s) of %s", command, count, label)
	if skip := skipNote(skippedShared, skippedPipeline); skip != "" {
		fmt.Fprintf(out, " %s", skip)
	}
	fmt.Fprintf(out, "\n")
}

// skipNote formats the "(skipped …)" suffix for the ##cmd summary, listing only
// the nonzero exclusion reasons.
func skipNote(shared, pipeline int) string {
	var parts []string
	if shared > 0 {
		parts = append(parts, fmt.Sprintf("%d shared", shared))
	}
	if pipeline > 0 {
		parts = append(parts, fmt.Sprintf("%d pipeline", pipeline))
	}
	if len(parts) == 0 {
		return ""
	}
	return "(skipped " + strings.Join(parts, ", ") + " pane(s))"
}

// cmdUsage prints the usage block for ##cmd.
func (w *WorkspaceActor) cmdUsage(out *strings.Builder) {
	fmt.Fprintf(out, "  ##cmd <scope> [selectors] <bash command>\n")
	fmt.Fprintf(out, "    scope:     pane | pg/stack | lane | tab | ws\n")
	fmt.Fprintf(out, "    selectors: --ws --tab --lane --pg --pane <id|name|index>\n")
	fmt.Fprintf(out, "    examples:  ##cmd stack pwd   |   ##cmd tab --tab 2 git status   |   ##cmd ws --ws build make\n\n")
}
