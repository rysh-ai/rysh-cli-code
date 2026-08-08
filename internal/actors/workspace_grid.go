package actors

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/google/uuid"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// handleNewInstance implements `##new <type> [args]` (and its alias
// `##rysh new <type> [args]`):
//
//	new tab                  create a new tab (takes no arguments)
//	new lane [tab]           create a new lane; tab defaults to the active tab
//	new pane [tab] [lane]    create a pane (a pane group holding one pane) at
//	                         the bottom of a lane; tab and lane default to the
//	                         active tab and active lane
//
// focusPaneID is the pane that issued the command; focus is kept there instead
// of jumping to the newly created pane.
func (w *WorkspaceActor) handleNewInstance(ctx actor.Context, out *strings.Builder, focusPaneID string, args []string) {
	if len(args) == 0 {
		w.newUsage(out)
		return
	}

	switch args[0] {
	case "tab":
		w.createTab(ctx)
		w.restoreFocusAfterCreate(focusPaneID)
		w.persistToKV()
		fmt.Fprintf(out, "\n[rysh] new tab created\n")

	case "lane":
		tabArg := ""
		if len(args) > 1 {
			tabArg = args[1]
		}
		// No explicit tab arg -> act on the tab that issued the command (robust
		// against an activeTabIdx that has drifted from the originating pane).
		tab := w.resolveTabArg(tabArg)
		if tabArg == "" {
			tab = w.resolveOriginTab(focusPaneID)
		}
		if tab == nil {
			fmt.Fprintf(out, "\n[rysh] tab not found: %q\n", tabArg)
			w.failRysh("tab not found: %q", tabArg)
			return
		}
		if err := w.checkLimits(1); err != nil {
			w.failRysh("%v", err)
			fmt.Fprintf(out, "\n[rysh] %v\n", err)
			return
		}
		alias := w.generateUniqueAlias()
		_ = w.pub.Send(msg.T("tab", tab.id, "inbox"), &msg.MsgTabCreatePane{Title: alias})
		w.resCounts.panes++
		w.restoreFocusAfterCreate(focusPaneID)
		w.persistToKV()
		fmt.Fprintf(out, "\n[rysh] new lane created in tab %q\n", tab.title)

	case "pane":
		tabArg, laneArg := "", ""
		if len(args) > 1 {
			tabArg = args[1]
		}
		if len(args) > 2 {
			laneArg = args[2]
		}
		// No explicit tab arg -> act on the tab that issued the command.
		tab := w.resolveTabArg(tabArg)
		if tabArg == "" {
			tab = w.resolveOriginTab(focusPaneID)
		}
		if tab == nil {
			fmt.Fprintf(out, "\n[rysh] tab not found: %q\n", tabArg)
			w.failRysh("tab not found: %q", tabArg)
			return
		}
		laneID := w.resolveLaneInTab(tab, laneArg)
		if laneID == "" {
			fmt.Fprintf(out, "\n[rysh] lane not found: %q\n", laneArg)
			w.failRysh("lane not found: %q", laneArg)
			return
		}
		if err := w.checkLimits(1); err != nil {
			w.failRysh("%v", err)
			fmt.Fprintf(out, "\n[rysh] %v\n", err)
			return
		}
		alias := w.generateUniqueAlias()
		_ = w.pub.Send(msg.T("tab", tab.id, "inbox"),
			&msg.MsgTabCreatePaneGroupInLane{LaneID: laneID, Title: alias})
		w.resCounts.panes++
		w.restoreFocusAfterCreate(focusPaneID)
		w.persistToKV()
		fmt.Fprintf(out, "\n[rysh] new pane created at the bottom of lane %.8s in tab %q\n", laneID, tab.title)

	case "grid":
		// Accept both "##new grid 3x4" (single token) and "##new grid 3 4"
		// (space-separated tokens). Join the remaining tokens with a space;
		// gridDims treats 'x'/'X' and whitespace equivalently as separators.
		//
		// Dimension count selects the target: a 1-D spec (<n>) stacks n panes
		// in the active lane, a 2-D spec (<lanes>x<panes>) builds the grid in
		// the active tab, and a 3-D spec (<tabs>x<lanes>x<panes>) creates new
		// tabs. The optional "--here" flag (also "-here"/"here") forbids the
		// new-tabs form.
		here := false
		specTokens := make([]string, 0, len(args)-1)
		for _, a := range args[1:] {
			switch a {
			case "--here", "-here", "here":
				here = true
			default:
				specTokens = append(specTokens, a)
			}
		}
		spec := strings.Join(specTokens, " ")
		w.handleNewGrid(ctx, out, focusPaneID, spec, here)

	case "stack", "pg", "panegroup":
		// ##new stack <n> (aliases: ##new pg <n>, ##new panegroup <n>):
		// add n stacked panes to the active pane group.
		if len(args) < 2 {
			ryshWriter(out).UsageLine(fmt.Sprintf("##new %s <n>   e.g. ##new stack 4", args[0]))
			w.failRyshUsage("usage: %s", fmt.Sprintf("##new %s <n>   e.g. ##new stack 4", args[0]))
			return
		}
		n, convErr := strconv.Atoi(args[1])
		if convErr != nil || n < 1 {
			fmt.Fprintf(out, "\n[rysh] invalid count %q: must be a positive integer\n", args[1])
			w.failRysh("invalid count %q: must be a positive integer", args[1])
			return
		}
		w.handleNewStack(ctx, out, focusPaneID, n)

	default:
		fmt.Fprintf(out, "\n[rysh] unknown instance type: %q\n", args[0])
		w.failRysh("unknown instance type: %q", args[0])
		w.newUsage(out)
	}
}

// handleNewGrid dispatches `##new grid <spec>` by dimension count:
//
//	<lanes>x<panes>          (2-D) → build the grid in the active tab
//	<tabs>x<lanes>x<panes>   (3-D) → create new tabs, each holding the grid
//
// A 2-D spec is unambiguous (it lacks the leading tab count), so it implies the
// active-tab form without needing a flag. The --here flag explicitly forces the
// active-tab form and therefore requires a 2-D spec.
func (w *WorkspaceActor) handleNewGrid(ctx actor.Context, out *strings.Builder, focusPaneID, spec string, here bool) {
	dims, err := gridDims(spec)
	if err != nil {
		fmt.Fprintf(out, "\n[rysh] invalid grid spec %q: %v\n", spec, err)
		w.failRysh("invalid grid spec %q: %v", spec, err)
		w.gridUsage(out)
		return
	}

	switch classifyGrid(dims, here) {
	case gridSameLane:
		w.handleNewGridSameLane(ctx, out, focusPaneID, dims[0])
	case gridActiveTab:
		w.handleNewGridHere(ctx, out, focusPaneID, dims[0], dims[1])
	case gridNewTabs:
		w.handleNewGridTabs(ctx, out, focusPaneID, dims[0], dims[1], dims[2])
	default:
		if here {
			fmt.Fprintf(out, "\n[rysh] --here builds a grid in the active tab; use <n> or <lanes>x<panes>, not a 3-D spec\n")
		} else {
			fmt.Fprintf(out, "\n[rysh] grid spec %q must be <n>, <lanes>x<panes>, or <tabs>x<lanes>x<panes>\n", spec)
		}
		w.gridUsage(out)
		w.failRyshUsage("bad grid spec %q", spec)
	}
}

// gridKind classifies a parsed grid spec by its target.
type gridKind int

const (
	gridInvalid   gridKind = iota
	gridSameLane           // 1-D <n>: stack n panes vertically in the active lane
	gridActiveTab          // 2-D <lanes>x<panes>: build in the active tab
	gridNewTabs            // 3-D <tabs>x<lanes>x<panes>: create new tabs
)

// classifyGrid maps a parsed grid spec to its target by dimension count:
//
//	1-D <n>                    → stack n panes in the active lane
//	2-D <lanes>x<panes>        → build a grid in the active tab
//	3-D <tabs>x<lanes>x<panes> → create new tabs
//
// Each arity is unambiguous, so no flag is required. The --here flag only
// forbids the new-tabs form (it forces "build here"); it is a no-op for the
// already-local 1-D and 2-D forms.
func classifyGrid(dims []int, here bool) gridKind {
	switch len(dims) {
	case 1:
		return gridSameLane
	case 2:
		return gridActiveTab
	case 3:
		if here {
			return gridInvalid
		}
		return gridNewTabs
	default:
		return gridInvalid
	}
}

// handleNewGridTabs implements the 3-D form `##new grid <tabs>x<lanes>x<panes>`
// (e.g. 2x3x4): create tabs tabs, each with lanes lanes, each lane holding panes
// panes (each pane is its own pane group). The whole structure is seeded inline
// in each actor's *actor.Started handler, so it is built reliably without racing
// the freshly-spawned actors' NATS bridges. Focus is kept on the originating
// pane.
func (w *WorkspaceActor) handleNewGridTabs(ctx actor.Context, out *strings.Builder, focusPaneID string, tabs, lanes, panes int) {
	total := tabs * lanes * panes
	const maxGridPanes = 200
	if total > maxGridPanes {
		fmt.Fprintf(out, "\n[rysh] grid too large: %dx%dx%d = %d panes (max %d)\n", tabs, lanes, panes, total, maxGridPanes)
		return
	}
	if err := w.checkLimits(total); err != nil {
		w.failRysh("%v", err)
		fmt.Fprintf(out, "\n[rysh] %v\n", err)
		return
	}

	for ti := 0; ti < tabs; ti++ {
		laneTitles := make([][]string, lanes)
		for li := 0; li < lanes; li++ {
			titles := make([]string, panes)
			for pi := 0; pi < panes; pi++ {
				titles[pi] = w.generateUniqueAlias()
			}
			laneTitles[li] = titles
		}
		w.createTabGrid(ctx, laneTitles)
	}

	w.resCounts.panes += total
	w.restoreFocusAfterCreate(focusPaneID)
	w.persistToKV()
	fmt.Fprintf(out, "\n[rysh] grid created: %d tab(s) x %d lane(s) x %d pane(s) = %d panes\n", tabs, lanes, panes, total)
}

// handleNewGridHere implements the 2-D form `##new grid <lanes>x<panes>` (e.g.
// 3x4): build a grid of lanes lanes, each holding panes panes, inside the active
// tab instead of creating new tabs. Existing lanes are preserved (the grid is
// appended), and the tab's lanes are equalized so the result reads as a clean
// grid. Focus is kept on the originating pane.
func (w *WorkspaceActor) handleNewGridHere(ctx actor.Context, out *strings.Builder, focusPaneID string, lanes, panes int) {
	// Build in the tab that issued the command, not w.currentTab(): activeTabIdx
	// can drift out of sync with the originating pane, which would otherwise drop
	// the grid into a tab the user is not on.
	tab := w.resolveOriginTab(focusPaneID)
	if tab == nil {
		fmt.Fprintf(out, "\n[rysh] no active tab\n")
		w.failRysh("no active tab")
		return
	}

	total := lanes * panes
	const maxGridPanes = 200
	if total > maxGridPanes {
		fmt.Fprintf(out, "\n[rysh] grid too large: %dx%d = %d panes (max %d)\n", lanes, panes, total, maxGridPanes)
		return
	}
	if err := w.checkLimits(total); err != nil {
		w.failRysh("%v", err)
		fmt.Fprintf(out, "\n[rysh] %v\n", err)
		return
	}

	laneTitles := make([][]string, lanes)
	for li := 0; li < lanes; li++ {
		titles := make([]string, panes)
		for pi := 0; pi < panes; pi++ {
			titles[pi] = w.generateUniqueAlias()
		}
		laneTitles[li] = titles
	}
	_ = w.pub.Send(msg.T("tab", tab.id, "inbox"), &msg.MsgTabCreateGrid{LaneTitles: laneTitles})

	w.resCounts.panes += total
	w.restoreFocusAfterCreate(focusPaneID)
	w.persistToKV()
	fmt.Fprintf(out, "\n[rysh] grid created in tab %q: %d lane(s) x %d pane(s) = %d panes\n", tab.title, lanes, panes, total)
}

// handleNewGridSameLane implements the 1-D form `##new grid <n>` (e.g. 4):
// stack n panes vertically in the active lane (each its own pane group),
// preserving any panes already there and equalizing the lane's group heights.
// Focus is kept on the originating pane.
func (w *WorkspaceActor) handleNewGridSameLane(ctx actor.Context, out *strings.Builder, focusPaneID string, count int) {
	// Resolve the target tab from the originating pane so the stack lands in the
	// tab the user is on even if activeTabIdx has drifted (see resolveOriginTab).
	tab := w.resolveOriginTab(focusPaneID)
	if tab == nil {
		fmt.Fprintf(out, "\n[rysh] no active tab\n")
		w.failRysh("no active tab")
		return
	}
	laneID := w.resolveLaneInTab(tab, "")
	if laneID == "" {
		fmt.Fprintf(out, "\n[rysh] no active lane in tab %q\n", tab.title)
		w.failRysh("no active lane in tab %q", tab.title)
		return
	}

	const maxGridPanes = 200
	if count > maxGridPanes {
		fmt.Fprintf(out, "\n[rysh] grid too large: %d panes (max %d)\n", count, maxGridPanes)
		return
	}
	if err := w.checkLimits(count); err != nil {
		w.failRysh("%v", err)
		fmt.Fprintf(out, "\n[rysh] %v\n", err)
		return
	}

	titles := make([]string, count)
	for i := range titles {
		titles[i] = w.generateUniqueAlias()
	}
	_ = w.pub.Send(msg.T("tab", tab.id, "inbox"),
		&msg.MsgTabCreateGroupsInLane{LaneID: laneID, Titles: titles})

	w.resCounts.panes += count
	w.restoreFocusAfterCreate(focusPaneID)
	w.persistToKV()
	fmt.Fprintf(out, "\n[rysh] %d pane(s) stacked vertically in the active lane of tab %q\n", count, tab.title)
}

// handleNewStack implements `##new stack <n>` (aliases `##new pg <n>` and
// `##new panegroup <n>`): add n stacked panes to the active pane group. Stacked
// panes share a single slot in the layout (a deck of cards) -- only the front
// pane is visible and accepts input. Focus is kept on the originating pane.
func (w *WorkspaceActor) handleNewStack(ctx actor.Context, out *strings.Builder, focusPaneID string, count int) {
	// Resolve the target tab from the originating pane so the stacked panes land
	// in the tab the user is on even if activeTabIdx has drifted.
	tab := w.resolveOriginTab(focusPaneID)
	if tab == nil {
		fmt.Fprintf(out, "\n[rysh] no active tab\n")
		w.failRysh("no active tab")
		return
	}

	const maxStackPanes = 200
	if count > maxStackPanes {
		fmt.Fprintf(out, "\n[rysh] stack too large: %d panes (max %d)\n", count, maxStackPanes)
		return
	}
	if err := w.checkLimits(count); err != nil {
		w.failRysh("%v", err)
		fmt.Fprintf(out, "\n[rysh] %v\n", err)
		return
	}

	// Each MsgTabCreateStackedPane adds one pane to the active lane's active
	// group; processed in order, all n land in the same group.
	for i := 0; i < count; i++ {
		alias := w.generateUniqueAlias()
		_ = w.pub.Send(msg.T("tab", tab.id, "inbox"), &msg.MsgTabCreateStackedPane{Title: alias})
	}

	w.resCounts.panes += count
	w.restoreFocusAfterCreate(focusPaneID)
	w.persistToKV()
	fmt.Fprintf(out, "\n[rysh] %d pane(s) added to the active pane group of tab %q\n", count, tab.title)
}

// gridDims normalizes a grid spec ('x'/'X' and whitespace are both treated as
// separators) and parses each token as a positive integer. It does not enforce
// a dimension count; callers validate len(dims).
func gridDims(spec string) ([]int, error) {
	normalized := strings.Map(func(r rune) rune {
		if r == 'x' || r == 'X' {
			return ' '
		}
		return r
	}, spec)
	parts := strings.Fields(normalized)
	dims := make([]int, len(parts))
	for i, p := range parts {
		n, convErr := strconv.Atoi(p)
		if convErr != nil || n < 1 {
			return nil, fmt.Errorf("each dimension must be a positive integer")
		}
		dims[i] = n
	}
	return dims, nil
}

// gridUsage prints the shared usage block for the `##new grid` command,
// covering the 1-D (active-lane), 2-D (active-tab) and 3-D (new-tabs) forms.
func (w *WorkspaceActor) gridUsage(out *strings.Builder) {
	fmt.Fprintf(out, "  usage: ##new grid <n>                        stack n panes in the active lane (e.g. 4)\n")
	fmt.Fprintf(out, "         ##new grid <lanes>x<panes>            grid in the active tab (e.g. 3x4 or 3 4)\n")
	fmt.Fprintf(out, "         ##new grid <tabs>x<lanes>x<panes>     create new tabs (e.g. 2x3x4 or 2 3 4)\n")
	fmt.Fprintf(out, "         (--here forces the active-tab form: ##new grid 3x4 --here)\n\n")
}

// createTabGrid spawns a new tab pre-seeded with one lane per entry in
// laneTitles, each lane holding one pane group per title. The new tab becomes
// the active tab. Resource accounting and focus are handled by the caller.
func (w *WorkspaceActor) createTabGrid(ctx actor.Context, laneTitles [][]string) *tabInfo {
	tabID := uuid.NewString()
	title := fmt.Sprintf("tab-%d", len(w.tabs)+1)

	ta := NewTabActor(tabID, title, w.cfg, w.pub, w.nc, w.agSetup, w.paneKV, w.childSecretResolver(), nil)
	ta.initialLaneTitles = laneTitles
	pid := ctx.Spawn(actor.PropsFromProducer(func() actor.Actor { return ta }))

	info := &tabInfo{id: tabID, title: title, actor: ta, pid: pid}
	w.tabs = append(w.tabs, info)
	w.activeTabIdx = len(w.tabs) - 1
	return info
}

// newUsage prints the usage block for the ##new / ##rysh new commands.
func (w *WorkspaceActor) newUsage(out *strings.Builder) {
	ryshWriter(out).Usage(
		"##new tab                    create a new tab",
		"##new lane [tab]             create a new lane (default: active tab)",
		"##new pane [tab] [lane]      create a pane at the bottom of a lane",
		"                             (defaults: active tab, active lane)",
		"##new grid <N>               stack N panes vertically in the active lane",
		"                             e.g. ##new grid 4",
		"##new grid <L>x<P>           build an L lanes x P panes grid in the active tab",
		"                             e.g. ##new grid 3x4  (or ##new grid 3 4)",
		"##new grid <T>x<L>x<P>       create T tabs, each with L lanes, each lane with P panes",
		"                             e.g. ##new grid 2x3x4  (or ##new grid 2 3 4)",
		"##new stack <N>              add N stacked panes to the active pane group",
		"                             e.g. ##new stack 4  (aliases: ##new pg <N>, ##new panegroup <N>)",
		"tab/lane args accept an id, name, or 1-based index",
		"(aliases: ##rysh new tab|lane|pane|grid|stack|pg|panegroup)",
	)
}

// restoreFocusAfterCreate keeps focus on the pane that issued a ##new command
// instead of letting it jump to the newly created pane. focusPaneByID resets
// the active tab, lane, and group consistently and sends an ordered focus
// message to the originating pane's tab (and lane), so the focus restore lands
// after the create is processed. Falls back to syncActivePane when there is no
// originating pane.
func (w *WorkspaceActor) restoreFocusAfterCreate(focusPaneID string) {
	if focusPaneID == "" {
		w.syncActivePane()
		return
	}
	w.focusPaneByID(focusPaneID)
}
