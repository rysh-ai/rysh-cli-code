// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/asynkron/protoactor-go/actor"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// ---------------------------------------------------------------------------
// ##move — relocate a pane, a stack or a lane
// ---------------------------------------------------------------------------
//
// Everything here is a rearrangement of LIVE things. Nothing is created,
// destroyed or restarted: the pane that moves keeps its PTY, its scrollback and
// whatever agent is mid-turn inside it (see pane_move.go for how, and for the
// two things a move cannot carry).
//
// The grammar is `##move <subject> [<ref>] <destination> [<ref>] [flags]`. The
// subject defaults to the pane the command was issued from, so an agent can say
// `##move pane to-lane build` about itself, and a human can name any pane in the
// session instead.
//
// Composite subjects are composed, not special-cased: moving a stack is moving
// each of its panes into one destination stack, and moving a lane is moving each
// of its stacks. Emptied stacks and lanes are dropped on the way out by the
// actors that own them, so the layout closes up behind the move.

// moveDest is a destination keyword and its optional argument.
type moveDest struct {
	kind string
	ref  string
}

// movePos is where in the destination the subject lands.
type movePos struct {
	// mode is "" (append — the default), "first", "index", "before" or "after".
	mode string
	// index is 1-based, matching every listing a user reads a position off.
	index int
	ref   string
}

// moveRequest is a parsed ##move command line.
type moveRequest struct {
	subject    string
	subjectRef string
	dest       moveDest
	tabArg     string
	pos        movePos
}

// moveDestKinds maps every destination keyword (and its aliases) to a canonical
// kind. A token in this set ends the subject and starts the destination, which
// is what lets the subject reference be optional without a delimiter.
var moveDestKinds = map[string]string{
	"to-lane":         "lane",
	"to-stack":        "stack",
	"to-stacked-pane": "stack",
	"to-pg":           "stack",
	"to-panegroup":    "stack",
	"to-group":        "stack",
	"to-tab":          "tab",
	"to-new-lane":     "new-lane",
	"to-new-tab":      "new-tab",
	"here":            "here",
	"out":             "out",
	"unstack":         "out",
	"up":              "up",
	"down":            "down",
	"left":            "left",
	"right":           "right",
}

// moveSubjects maps a subject word (and its aliases) to a canonical subject.
var moveSubjects = map[string]string{
	"pane": "pane", "p": "pane",
	"stack": "stack", "pg": "stack", "panegroup": "stack", "group": "stack",
	"lane": "lane",
	"tab":  "tab",
}

// handleMoveCommand implements `##move` (alias `##mv`).
func (w *WorkspaceActor) handleMoveCommand(ctx actor.Context, out *strings.Builder, focusPaneID string, args []string) error {
	// `##move help` asks for the usage and gets it, successfully. A BARE
	// `##move` printed the same block and also exited 0, which is the shape
	// design 021 went hunting: a command that did nothing at all reporting that
	// it worked. A script that typos the verb has to notice.
	if len(args) == 1 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help") {
		w.moveUsage(out)
		return nil
	}
	if len(args) == 0 {
		w.moveUsage(out)
		return fmt.Errorf("##move needs a subject and a destination")
	}

	req, err := parseMoveRequest(args)
	if err != nil {
		w.moveUsage(out)
		fmt.Fprintf(out, "\n[rysh] %v\n", err)
		return err
	}

	// A move must not walk the human's cursor to whatever an agent rearranged,
	// for restoreFocusAfterCreate's reason: focus is the human's.
	defer func() {
		w.restoreFocusAfterCreate(focusPaneID)
		w.invalidateSnapshotCaches()
		w.persistToKV()
		w.notifyLayoutDirty()
	}()

	switch req.subject {
	case "pane":
		return w.movePaneSubject(ctx, out, focusPaneID, req)
	case "stack":
		return w.moveStackSubject(ctx, out, focusPaneID, req)
	case "lane":
		return w.moveLaneSubject(ctx, out, focusPaneID, req)
	case "tab":
		return w.moveTabSubject(ctx, out, focusPaneID, req)
	}
	err = fmt.Errorf("unknown ##move subject: %q", req.subject)
	fmt.Fprintf(out, "\n[rysh] %v\n", err)
	return err
}

// ---------------------------------------------------------------------------
// Parsing
// ---------------------------------------------------------------------------

func parseMoveRequest(args []string) (moveRequest, error) {
	var req moveRequest
	rest, tabArg, pos, err := parseMoveFlags(args)
	if err != nil {
		return req, err
	}
	req.tabArg = tabArg
	req.pos = pos

	if len(rest) == 0 {
		return req, fmt.Errorf("##move needs a subject (pane, stack, lane or tab)")
	}
	subject, ok := moveSubjects[strings.ToLower(rest[0])]
	if !ok {
		return req, fmt.Errorf("unknown ##move subject: %q", rest[0])
	}
	req.subject = subject
	rest = rest[1:]

	// Everything before the destination keyword names the subject. At most one
	// token: a pane/lane/stack reference is a single id, name or index.
	for len(rest) > 0 {
		if kind, ok := moveDestKinds[strings.ToLower(rest[0])]; ok {
			req.dest.kind = kind
			rest = rest[1:]
			if len(rest) > 0 {
				req.dest.ref = rest[0]
				rest = rest[1:]
			}
			break
		}
		if req.subjectRef != "" {
			return req, fmt.Errorf("unexpected argument %q — expected a destination (to-lane, to-stack, to-tab, up, down, left, right, out)", rest[0])
		}
		req.subjectRef = rest[0]
		rest = rest[1:]
	}
	if req.dest.kind == "" {
		return req, fmt.Errorf("##move %s needs a destination", req.subject)
	}
	if len(rest) > 0 {
		return req, fmt.Errorf("unexpected argument after the destination: %q", rest[0])
	}
	return req, nil
}

// parseMoveFlags pulls the flags out of an argument list, leaving the
// positional words. --tab follows the spelling every other command accepts.
func parseMoveFlags(args []string) (rest []string, tabArg string, pos movePos, err error) {
	need := func(i int, flag string) (string, error) {
		if i+1 >= len(args) {
			return "", fmt.Errorf("%s needs a value", flag)
		}
		return args[i+1], nil
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--tab" || a == "-t":
			if tabArg, err = need(i, a); err != nil {
				return nil, "", pos, err
			}
			i++
		case strings.HasPrefix(a, "--tab="):
			tabArg = strings.TrimPrefix(a, "--tab=")
		case a == "--before" || a == "--after":
			v, e := need(i, a)
			if e != nil {
				return nil, "", pos, e
			}
			pos = movePos{mode: strings.TrimPrefix(a, "--"), ref: v}
			i++
		case strings.HasPrefix(a, "--before=") || strings.HasPrefix(a, "--after="):
			parts := strings.SplitN(strings.TrimPrefix(a, "--"), "=", 2)
			pos = movePos{mode: parts[0], ref: parts[1]}
		case a == "--index":
			v, e := need(i, a)
			if e != nil {
				return nil, "", pos, e
			}
			n, e := strconv.Atoi(v)
			if e != nil || n < 1 {
				return nil, "", pos, fmt.Errorf("--index takes a 1-based position, got %q", v)
			}
			pos = movePos{mode: "index", index: n}
			i++
		case strings.HasPrefix(a, "--index="):
			n, e := strconv.Atoi(strings.TrimPrefix(a, "--index="))
			if e != nil || n < 1 {
				return nil, "", pos, fmt.Errorf("--index takes a 1-based position, got %q", a)
			}
			pos = movePos{mode: "index", index: n}
		case a == "--first" || a == "--top":
			pos = movePos{mode: "first"}
		case a == "--last" || a == "--bottom":
			pos = movePos{mode: ""}
		case strings.HasPrefix(a, "-") && a != "-":
			return nil, "", pos, fmt.Errorf("unknown flag: %q", a)
		default:
			rest = append(rest, a)
		}
	}
	return rest, tabArg, pos, nil
}

// ---------------------------------------------------------------------------
// Subject: pane
// ---------------------------------------------------------------------------

func (w *WorkspaceActor) movePaneSubject(ctx actor.Context, out *strings.Builder, focusPaneID string, req moveRequest) error {
	paneID := w.resolveMovePane(req.subjectRef, focusPaneID)
	if paneID == "" {
		return moveFail(out, "pane not found: %q", orSelf(req.subjectRef))
	}
	srcTab, srcLane, srcGroup := w.locatePane(paneID)
	if srcTab == nil || srcLane == nil || srcGroup == nil {
		return moveFail(out, "cannot locate pane %s in the layout", shortID(paneID))
	}

	switch req.dest.kind {
	case "up", "down", "left", "right":
		dir := moveDirection(req.dest.kind)
		if req.dest.kind == "left" || req.dest.kind == "right" {
			// A pane's horizontal neighbour is another LANE, not another slot
			// in its stack, so this is a lane-to-lane transfer rather than a
			// reorder.
			return w.movePaneToNeighbourLane(ctx, out, paneID, srcTab, srcLane, req.dest.kind)
		}
		reply, ok := requestMove[*tabMoveReply](ctx, srcTab.pid, &tabMovePaneInStackRequest{paneID: paneID, dir: dir})
		if !ok || reply == nil || !reply.ok {
			return moveFail(out, "pane %s is already at the %s of its stack", shortID(paneID), edgeWord(req.dest.kind))
		}
		fmt.Fprintf(out, "\n[rysh] moved pane %s %s within its stack\n", w.paneLabel(paneID), req.dest.kind)
		return nil

	case "out":
		if len(srcGroup.Panes) <= 1 {
			return moveFail(out, "pane %s is not stacked — nothing to move it out of", shortID(paneID))
		}
		plan := paneMovePlan{
			destTab: srcTab,
			laneID:  srcLane.ID,
			groupAt: groupIndexInLane(srcLane, srcGroup.ID) + 1,
			index:   -1,
		}
		if _, _, err := w.transferPane(ctx, paneID, plan); err != nil {
			return moveFail(out, "%v", err)
		}
		fmt.Fprintf(out, "\n[rysh] moved pane %s out of its stack, into its own stack in lane %s\n",
			w.paneLabel(paneID), laneLabel(srcLane))
		return nil
	}

	plan, desc, err := w.resolvePaneDestination(ctx, req, focusPaneID, srcTab, srcLane, srcGroup)
	if err != nil {
		return moveFail(out, "%v", err)
	}
	if err := w.guardTabNotEmptied(srcTab, plan.destTab, 1); err != nil {
		return moveFail(out, "%v", err)
	}
	if _, _, err := w.transferPane(ctx, paneID, plan); err != nil {
		return moveFail(out, "%v", err)
	}
	fmt.Fprintf(out, "\n[rysh] moved pane %s -> %s\n", w.paneLabel(paneID), desc)
	return nil
}

// movePaneToNeighbourLane moves a pane one lane left or right within its tab,
// as its own stack. There is no neighbour past the edge, and creating one there
// is a different command (to-new-lane), so the edge refuses.
func (w *WorkspaceActor) movePaneToNeighbourLane(ctx actor.Context, out *strings.Builder, paneID string, tab *tabInfo, srcLane *domain.LaneSnapshot, dir string) error {
	snap := w.queryTabSnapshot(tab.id)
	if snap == nil {
		return moveFail(out, "tab %q did not answer", tab.title)
	}
	idx := laneIndexInTab(snap, srcLane.ID)
	target := idx - 1
	if dir == "right" {
		target = idx + 1
	}
	if idx < 0 || target < 0 || target >= len(snap.Lanes) {
		return moveFail(out, "no lane to the %s of %s", dir, laneLabel(srcLane))
	}
	dest := snap.Lanes[target]
	if _, _, err := w.transferPane(ctx, paneID, paneMovePlan{
		destTab: tab, laneID: dest.ID, groupAt: -1, index: -1,
	}); err != nil {
		return moveFail(out, "%v", err)
	}
	fmt.Fprintf(out, "\n[rysh] moved pane %s -> lane %s\n", w.paneLabel(paneID), laneLabel(&dest))
	return nil
}

// resolvePaneDestination turns a parsed destination into a concrete plan plus
// the sentence fragment the result line uses to describe it.
func (w *WorkspaceActor) resolvePaneDestination(
	ctx actor.Context,
	req moveRequest,
	focusPaneID string,
	srcTab *tabInfo,
	srcLane *domain.LaneSnapshot,
	srcGroup *domain.PaneGroupSnapshot,
) (paneMovePlan, string, error) {
	plan := paneMovePlan{index: -1, groupAt: -1, laneAt: -1}

	switch req.dest.kind {
	case "lane":
		tab, err := w.moveDestTab(req.tabArg, srcTab)
		if err != nil {
			return plan, "", err
		}
		snap := w.queryTabSnapshot(tab.id)
		lane := domain.ResolveLane(snap, req.dest.ref)
		if lane == nil {
			return plan, "", fmt.Errorf("lane not found in tab %q: %q (see ##lane list)", tab.title, req.dest.ref)
		}
		plan.destTab = tab
		plan.laneID = lane.ID
		plan.groupAt = w.resolveGroupPosition(lane, req.pos)
		return plan, fmt.Sprintf("lane %s (tab %q)", laneLabel(lane), tab.title), nil

	case "stack":
		tab, lane, group, err := w.resolveStackRef(req.dest.ref, srcTab, srcLane, srcGroup)
		if err != nil {
			return plan, "", err
		}
		plan.destTab = tab
		plan.laneID = lane.ID
		plan.groupID = group.ID
		plan.index = resolvePanePosition(group, req.pos)
		return plan, fmt.Sprintf("stack %s in lane %s (tab %q)", shortID(group.ID), laneLabel(lane), tab.title), nil

	case "tab":
		tab := w.resolveTabArg(firstNonEmpty(req.dest.ref, req.tabArg))
		if tab == nil {
			return plan, "", fmt.Errorf("tab not found: %q (see ##tab list)", firstNonEmpty(req.dest.ref, req.tabArg))
		}
		lane := domain.ResolveLane(w.queryTabSnapshot(tab.id), "")
		if lane == nil {
			return plan, "", fmt.Errorf("tab %q has no lane to move into", tab.title)
		}
		plan.destTab = tab
		plan.laneID = lane.ID
		return plan, fmt.Sprintf("tab %q (lane %s)", tab.title, laneLabel(lane)), nil

	case "new-lane":
		tab, err := w.moveDestTab(req.tabArg, srcTab)
		if err != nil {
			return plan, "", err
		}
		plan.destTab = tab
		plan.laneAt = w.resolveLanePosition(w.queryTabSnapshot(tab.id), req.pos)
		return plan, fmt.Sprintf("a new lane in tab %q", tab.title), nil

	case "new-tab":
		tab := w.createEmptyTab(ctx)
		if tab == nil {
			return plan, "", fmt.Errorf("could not create a tab")
		}
		plan.destTab = tab
		return plan, fmt.Sprintf("a new tab %q", tab.title), nil

	case "here":
		hostTab, hostLane, hostGroup := w.locatePane(focusPaneID)
		if hostTab == nil || hostGroup == nil {
			return plan, "", fmt.Errorf("`here` needs an issuing pane, and this command has none")
		}
		plan.destTab = hostTab
		plan.laneID = hostLane.ID
		plan.groupID = hostGroup.ID
		plan.index = resolvePanePosition(hostGroup, req.pos)
		return plan, fmt.Sprintf("this stack, in lane %s (tab %q)", laneLabel(hostLane), hostTab.title), nil
	}
	return plan, "", fmt.Errorf("unsupported destination for a pane: %q", req.dest.kind)
}

// ---------------------------------------------------------------------------
// Subject: stack
// ---------------------------------------------------------------------------

func (w *WorkspaceActor) moveStackSubject(ctx actor.Context, out *strings.Builder, focusPaneID string, req moveRequest) error {
	srcTab, srcLane, srcGroup, err := w.resolveSubjectStack(req.subjectRef, focusPaneID)
	if err != nil {
		return moveFail(out, "%v", err)
	}
	paneIDs := paneIDsOfGroup(srcGroup)
	if len(paneIDs) == 0 {
		return moveFail(out, "stack %s holds no movable pane", shortID(srcGroup.ID))
	}

	switch req.dest.kind {
	case "up", "down":
		reply, ok := requestMove[*tabMoveReply](ctx, srcTab.pid,
			&tabMoveStackRequest{laneID: srcLane.ID, groupID: srcGroup.ID, dir: moveDirection(req.dest.kind)})
		if !ok || reply == nil || !reply.ok {
			return moveFail(out, "stack %s is already at the %s of lane %s", shortID(srcGroup.ID), edgeWord(req.dest.kind), laneLabel(srcLane))
		}
		fmt.Fprintf(out, "\n[rysh] moved stack %s %s within lane %s\n", shortID(srcGroup.ID), req.dest.kind, laneLabel(srcLane))
		return nil
	case "left", "right":
		snap := w.queryTabSnapshot(srcTab.id)
		idx := laneIndexInTab(snap, srcLane.ID)
		target := idx - 1
		if req.dest.kind == "right" {
			target = idx + 1
		}
		if snap == nil || idx < 0 || target < 0 || target >= len(snap.Lanes) {
			return moveFail(out, "no lane to the %s of %s", req.dest.kind, laneLabel(srcLane))
		}
		req.dest = moveDest{kind: "lane", ref: snap.Lanes[target].ID}
	case "out":
		return moveFail(out, "`out` moves a pane out of a stack; a stack is what it moves out of")
	}

	plan, desc, err := w.resolvePaneDestination(ctx, req, focusPaneID, srcTab, srcLane, srcGroup)
	if err != nil {
		return moveFail(out, "%v", err)
	}
	if err := w.guardTabNotEmptied(srcTab, plan.destTab, len(paneIDs)); err != nil {
		return moveFail(out, "%v", err)
	}
	// The stack's own height weight travels with it, so a deliberately tall
	// stack does not silently reset to the destination's average.
	plan.rowFlex = srcGroup.RowFlex

	moved, err := w.transferStack(ctx, paneIDs, plan)
	if err != nil {
		return moveFail(out, "%v (moved %d of %d panes)", err, moved, len(paneIDs))
	}
	fmt.Fprintf(out, "\n[rysh] moved stack %s (%d pane(s)) -> %s\n", shortID(srcGroup.ID), len(paneIDs), desc)
	return nil
}

// transferStack moves every pane of one stack into a single destination stack,
// in order. The first pane establishes the destination (creating a stack, and a
// lane, when the plan asks for one); the rest follow it there, which is what
// keeps a moved stack a stack instead of scattering it.
func (w *WorkspaceActor) transferStack(ctx actor.Context, paneIDs []string, plan paneMovePlan) (int, error) {
	moved := 0
	for i, paneID := range paneIDs {
		step := plan
		if i > 0 {
			// The destination stack (and lane) now exist and were written back
			// into plan below, so later panes JOIN them. Leaving groupAt/laneAt
			// set would make each pane create another container of its own,
			// which is the failure mode a stack move has: arriving as N stacks
			// instead of the one stack it was.
			step.groupAt = -1
			step.laneAt = -1
		}
		laneID, groupID, err := w.transferPane(ctx, paneID, step)
		if err != nil {
			return moved, err
		}
		moved++
		plan.laneID = laneID
		plan.groupID = groupID
		if plan.index >= 0 {
			plan.index++ // keep the incoming order at an explicit position
		}
	}
	return moved, nil
}

// ---------------------------------------------------------------------------
// Subject: lane
// ---------------------------------------------------------------------------

func (w *WorkspaceActor) moveLaneSubject(ctx actor.Context, out *strings.Builder, focusPaneID string, req moveRequest) error {
	srcTab, srcLane, err := w.resolveSubjectLane(req.subjectRef, focusPaneID)
	if err != nil {
		return moveFail(out, "%v", err)
	}

	switch req.dest.kind {
	case "left", "right", "up", "down":
		reply, ok := requestMove[*tabMoveReply](ctx, srcTab.pid,
			&tabMoveLaneRequest{laneID: srcLane.ID, dir: moveDirection(req.dest.kind)})
		if !ok || reply == nil || !reply.ok {
			return moveFail(out, "lane %s is already at the %s of tab %q", laneLabel(srcLane), edgeWord(req.dest.kind), srcTab.title)
		}
		fmt.Fprintf(out, "\n[rysh] moved lane %s %s within tab %q\n", laneLabel(srcLane), req.dest.kind, srcTab.title)
		return nil
	}

	var destTab *tabInfo
	switch req.dest.kind {
	case "tab":
		destTab = w.resolveTabArg(firstNonEmpty(req.dest.ref, req.tabArg))
		if destTab == nil {
			return moveFail(out, "tab not found: %q (see ##tab list)", firstNonEmpty(req.dest.ref, req.tabArg))
		}
	case "new-tab":
		destTab = w.createEmptyTab(ctx)
		if destTab == nil {
			return moveFail(out, "could not create a tab")
		}
	default:
		return moveFail(out, "a lane moves to-tab, to-new-tab, left or right — not %q", req.dest.kind)
	}
	if destTab.id == srcTab.id {
		return moveFail(out, "lane %s is already in tab %q", laneLabel(srcLane), destTab.title)
	}

	total := 0
	for i := range srcLane.PaneGroups {
		total += len(srcLane.PaneGroups[i].Panes)
	}
	if err := w.guardTabNotEmptied(srcTab, destTab, total); err != nil {
		return moveFail(out, "%v", err)
	}

	// One new lane in the destination, carrying this lane's name and width, then
	// one new stack per source stack, carrying its height.
	laneAt := w.resolveLanePosition(w.queryTabSnapshot(destTab.id), req.pos)
	destLaneID := ""
	movedPanes := 0
	for gi := range srcLane.PaneGroups {
		g := srcLane.PaneGroups[gi]
		paneIDs := paneIDsOfGroup(&g)
		if len(paneIDs) == 0 {
			continue
		}
		plan := paneMovePlan{
			destTab:  destTab,
			laneID:   destLaneID,
			index:    -1,
			groupAt:  -1,
			laneAt:   laneAt,
			rowFlex:  g.RowFlex,
			laneName: srcLane.Name,
			laneFlex: srcLane.Flex,
		}
		n, err := w.transferStack(ctx, paneIDs, plan)
		movedPanes += n
		if err != nil {
			return moveFail(out, "%v (moved %d of %d panes)", err, movedPanes, total)
		}
		if destLaneID == "" {
			// Learn the lane the first stack created, so the rest join it.
			if _, lane, _ := w.locatePane(paneIDs[0]); lane != nil {
				destLaneID = lane.ID
			}
		}
	}
	if movedPanes == 0 {
		return moveFail(out, "lane %s holds no movable pane", laneLabel(srcLane))
	}
	fmt.Fprintf(out, "\n[rysh] moved lane %s (%d pane(s)) -> tab %q\n", laneLabel(srcLane), movedPanes, destTab.title)
	return nil
}

// ---------------------------------------------------------------------------
// Subject: tab
// ---------------------------------------------------------------------------

func (w *WorkspaceActor) moveTabSubject(_ actor.Context, out *strings.Builder, focusPaneID string, req moveRequest) error {
	tab := w.resolveTabArg(req.subjectRef)
	if tab == nil && req.subjectRef == "" {
		tab = w.resolveOriginTab(focusPaneID)
	}
	if tab == nil {
		return moveFail(out, "tab not found: %q (see ##tab list)", req.subjectRef)
	}
	switch req.dest.kind {
	case "left", "right", "up", "down":
		if !w.moveTabByID(tab.id, moveDirection(req.dest.kind)) {
			return moveFail(out, "tab %q is already at the %s of the tab bar", tab.title, edgeWord(req.dest.kind))
		}
		fmt.Fprintf(out, "\n[rysh] moved tab %q %s\n", tab.title, req.dest.kind)
		return nil
	}
	return moveFail(out, "a tab moves left or right — not %q", req.dest.kind)
}

// ---------------------------------------------------------------------------
// The transfer itself
// ---------------------------------------------------------------------------

// paneMovePlan is a fully resolved destination for one pane.
type paneMovePlan struct {
	destTab *tabInfo
	// laneID "" creates a lane in destTab; groupID "" creates a stack in the
	// destination lane.
	laneID  string
	groupID string
	// index is the slot within the destination stack, groupAt the slot within
	// the destination lane, laneAt the slot within the destination tab. All
	// 0-based, and <0 means append.
	index   int
	groupAt int
	laneAt  int
	// rowFlex / laneFlex / laneName seed containers this move creates.
	rowFlex  int
	laneFlex int
	laneName string
}

// transferPane releases one running pane from wherever it is and adopts it at
// the plan's destination, returning the lane and stack it landed in.
//
// The release and the adopt are two hops, and between them the pane is held only
// by the handle in flight — a running PTY with nothing rendering it. So a failed
// adopt puts it back rather than returning an error and leaking it: first into
// the exact stack it came from, then into its old lane, then into a fresh lane
// in its old tab. Only if all three fail is the pane genuinely stranded, and
// that is reported as such.
func (w *WorkspaceActor) transferPane(ctx actor.Context, paneID string, plan paneMovePlan) (string, string, error) {
	srcTab, srcLane, srcGroup := w.locatePane(paneID)
	if srcTab == nil || srcLane == nil || srcGroup == nil {
		return "", "", fmt.Errorf("cannot locate pane %s", shortID(paneID))
	}
	if plan.destTab == nil {
		return "", "", fmt.Errorf("no destination tab")
	}
	// Destinations the pane is already at. Both of these have to be caught
	// BEFORE the release, because the release is what would destroy them: a
	// pane alone in its stack takes the stack with it when it leaves, and a
	// pane alone in its lane takes the lane. Releasing first and discovering
	// the destination gone afterwards is how `##move pane <p> to-tab <its own
	// tab>` ended up shuffling a pane into a brand-new lane and reporting that
	// the destination had refused it.
	if plan.groupID != "" && plan.groupID == srcGroup.ID {
		return srcLane.ID, srcGroup.ID, nil
	}
	if plan.groupID == "" && plan.laneID == srcLane.ID && domain.CountPanesInLane(srcLane) == 1 {
		return srcLane.ID, srcGroup.ID, nil
	}

	rel, ok := requestMove[*tabReleasePaneReply](ctx, srcTab.pid, &tabReleasePaneRequest{paneID: paneID})
	if !ok || rel == nil || !rel.ok || rel.handle == nil {
		return "", "", fmt.Errorf("tab %q would not release pane %s", srcTab.title, shortID(paneID))
	}

	adopt, ok := requestMove[*tabAdoptPaneReply](ctx, plan.destTab.pid, &tabAdoptPaneRequest{
		handle:   rel.handle,
		laneID:   plan.laneID,
		groupID:  plan.groupID,
		index:    plan.index,
		groupAt:  plan.groupAt,
		laneAt:   plan.laneAt,
		rowFlex:  plan.rowFlex,
		laneName: plan.laneName,
		laneFlex: plan.laneFlex,
	})
	if ok && adopt != nil && adopt.ok {
		return adopt.laneID, adopt.groupID, nil
	}

	for _, fallback := range []tabAdoptPaneRequest{
		{handle: rel.handle, laneID: srcLane.ID, groupID: srcGroup.ID, index: -1, groupAt: -1, laneAt: -1},
		{handle: rel.handle, laneID: srcLane.ID, index: -1, groupAt: -1, laneAt: -1},
		{handle: rel.handle, index: -1, groupAt: -1, laneAt: -1, laneName: srcLane.Name, laneFlex: srcLane.Flex},
	} {
		f := fallback
		if back, ok := requestMove[*tabAdoptPaneReply](ctx, srcTab.pid, &f); ok && back != nil && back.ok {
			return "", "", fmt.Errorf("destination refused pane %s; it was put back where it was", shortID(paneID))
		}
	}
	return "", "", fmt.Errorf("destination refused pane %s and it could not be put back — the pane is still running but is no longer in the layout; `##rysh reload` or restarting the daemon will restore it from KV", shortID(paneID))
}

// guardTabNotEmptied refuses a cross-tab move that would leave the source tab
// with no panes at all. An empty tab is a tab nothing can be typed into and
// nothing draws, and `##tab delete` already refuses to produce the same state.
func (w *WorkspaceActor) guardTabNotEmptied(src, dest *tabInfo, moving int) error {
	if src == nil || dest == nil || src.id == dest.id {
		return nil
	}
	if total := w.queryPaneCount(src.id); total > 0 && total <= moving {
		return fmt.Errorf("that would leave tab %q with no panes — move something into it first, or close it with ##tab delete", src.title)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Resolution helpers
// ---------------------------------------------------------------------------

// locatePane finds the tab, lane and stack that hold a pane right now.
func (w *WorkspaceActor) locatePane(paneID string) (*tabInfo, *domain.LaneSnapshot, *domain.PaneGroupSnapshot) {
	if paneID == "" {
		return nil, nil, nil
	}
	for _, info := range w.tabs {
		snap := w.queryTabSnapshot(info.id)
		if snap == nil {
			continue
		}
		lane := domain.LaneOfPane(snap, paneID)
		group := domain.GroupOfPane(snap, paneID)
		if lane != nil && group != nil {
			return info, lane, group
		}
	}
	return nil, nil, nil
}

// resolveMovePane resolves the subject pane: an explicit reference, or the pane
// the command was issued from.
func (w *WorkspaceActor) resolveMovePane(ref, focusPaneID string) string {
	if ref == "" {
		return focusPaneID
	}
	return w.resolvePaneID(ref)
}

// resolveSubjectStack resolves the stack to move: an id, a `lane:index` style
// index within the issuing lane, a pane that is in it, or — with no reference —
// the issuing pane's own stack.
func (w *WorkspaceActor) resolveSubjectStack(ref, focusPaneID string) (*tabInfo, *domain.LaneSnapshot, *domain.PaneGroupSnapshot, error) {
	if ref == "" {
		tab, lane, group := w.locatePane(focusPaneID)
		if tab == nil {
			return nil, nil, nil, fmt.Errorf("no stack to move — name one, or run this from a pane")
		}
		return tab, lane, group, nil
	}
	if tab, lane, group := w.findGroupByID(ref); group != nil {
		return tab, lane, group, nil
	}
	if paneID := w.resolvePaneID(ref); paneID != "" {
		tab, lane, group := w.locatePane(paneID)
		if group != nil {
			return tab, lane, group, nil
		}
	}
	// A bare index numbers the stacks of the issuing pane's lane.
	if tab, lane, _ := w.locatePane(focusPaneID); lane != nil {
		if g := domain.ResolveGroup(lane, ref); g != nil {
			return tab, lane, g, nil
		}
	}
	return nil, nil, nil, fmt.Errorf("stack not found: %q (an id, an index within this lane, or a pane in it)", ref)
}

// resolveSubjectLane resolves the lane to move.
func (w *WorkspaceActor) resolveSubjectLane(ref, focusPaneID string) (*tabInfo, *domain.LaneSnapshot, error) {
	originTab := w.resolveOriginTab(focusPaneID)
	if ref == "" {
		tab, lane, _ := w.locatePane(focusPaneID)
		if lane != nil {
			return tab, lane, nil
		}
		if originTab != nil {
			if lane := domain.ResolveLane(w.queryTabSnapshot(originTab.id), ""); lane != nil {
				return originTab, lane, nil
			}
		}
		return nil, nil, fmt.Errorf("no lane to move — name one, or run this from a pane")
	}
	// An id matches anywhere; a name or index is read within the issuing tab,
	// because both are only unique there.
	for _, info := range w.tabs {
		snap := w.queryTabSnapshot(info.id)
		if snap == nil {
			continue
		}
		for i := range snap.Lanes {
			if snap.Lanes[i].ID == ref {
				return info, &snap.Lanes[i], nil
			}
		}
	}
	if originTab != nil {
		if lane := domain.ResolveLane(w.queryTabSnapshot(originTab.id), ref); lane != nil {
			return originTab, lane, nil
		}
	}
	return nil, nil, fmt.Errorf("lane not found: %q (see ##lane list)", ref)
}

// resolveStackRef resolves a DESTINATION stack. `##move pane to-stacked-pane
// <pane>` names a pane inside the stack rather than the stack itself, which is
// the form a human has to hand — stacks have ids but no names.
func (w *WorkspaceActor) resolveStackRef(ref string, srcTab *tabInfo, srcLane *domain.LaneSnapshot, srcGroup *domain.PaneGroupSnapshot) (*tabInfo, *domain.LaneSnapshot, *domain.PaneGroupSnapshot, error) {
	if ref == "" {
		return nil, nil, nil, fmt.Errorf("to-stack needs a stack id or a pane in the destination stack")
	}
	if tab, lane, group := w.findGroupByID(ref); group != nil {
		return tab, lane, group, nil
	}
	if paneID := w.resolvePaneID(ref); paneID != "" {
		tab, lane, group := w.locatePane(paneID)
		if group != nil {
			return tab, lane, group, nil
		}
	}
	if srcLane != nil {
		if g := domain.ResolveGroup(srcLane, ref); g != nil && (srcGroup == nil || g.ID != srcGroup.ID) {
			return srcTab, srcLane, g, nil
		}
	}
	return nil, nil, nil, fmt.Errorf("stack not found: %q (a stack id, or a pane in it — see ##pane list)", ref)
}

// findGroupByID looks a stack up across every tab.
func (w *WorkspaceActor) findGroupByID(groupID string) (*tabInfo, *domain.LaneSnapshot, *domain.PaneGroupSnapshot) {
	for _, info := range w.tabs {
		snap := w.queryTabSnapshot(info.id)
		if snap == nil {
			continue
		}
		for li := range snap.Lanes {
			for gi := range snap.Lanes[li].PaneGroups {
				if snap.Lanes[li].PaneGroups[gi].ID == groupID {
					return info, &snap.Lanes[li], &snap.Lanes[li].PaneGroups[gi]
				}
			}
		}
	}
	return nil, nil, nil
}

// moveDestTab picks the tab a lane-flavoured destination is read in: --tab when
// given, the subject's own tab otherwise. Keeping the default on the SUBJECT's
// tab (not the active one) is what makes `##move pane <id> to-lane 2` mean the
// same thing whether a human or an agent in another tab typed it.
func (w *WorkspaceActor) moveDestTab(tabArg string, srcTab *tabInfo) (*tabInfo, error) {
	if tabArg == "" {
		if srcTab == nil {
			return nil, fmt.Errorf("no tab to move within")
		}
		return srcTab, nil
	}
	tab := w.resolveTabArg(tabArg)
	if tab == nil {
		return nil, fmt.Errorf("tab not found: %q (see ##tab list)", tabArg)
	}
	return tab, nil
}

// resolvePanePosition turns a --before/--after/--index/--first into a 0-based
// slot within a destination stack. Anything unresolvable appends, which is the
// documented default rather than a silent failure.
func resolvePanePosition(g *domain.PaneGroupSnapshot, pos movePos) int {
	if g == nil {
		return -1
	}
	switch pos.mode {
	case "first":
		return 0
	case "index":
		if pos.index-1 <= len(g.Panes) {
			return pos.index - 1
		}
	case "before", "after":
		for i := range g.Panes {
			p := g.Panes[i]
			if p.ID == pos.ref || p.Title == pos.ref || (p.GivenName != "" && p.GivenName == pos.ref) {
				if pos.mode == "before" {
					return i
				}
				return i + 1
			}
		}
	}
	return -1
}

// resolveGroupPosition is resolvePanePosition for a stack's slot within a lane.
func (w *WorkspaceActor) resolveGroupPosition(lane *domain.LaneSnapshot, pos movePos) int {
	if lane == nil {
		return -1
	}
	switch pos.mode {
	case "first":
		return 0
	case "index":
		if pos.index-1 <= len(lane.PaneGroups) {
			return pos.index - 1
		}
	case "before", "after":
		if g := domain.ResolveGroup(lane, pos.ref); g != nil {
			idx := groupIndexInLane(lane, g.ID)
			if pos.mode == "after" {
				idx++
			}
			return idx
		}
	}
	return -1
}

// resolveLanePosition is resolvePanePosition for a lane's slot within a tab.
func (w *WorkspaceActor) resolveLanePosition(snap *domain.TabSnapshot, pos movePos) int {
	if snap == nil {
		return -1
	}
	switch pos.mode {
	case "first":
		return 0
	case "index":
		if pos.index-1 <= len(snap.Lanes) {
			return pos.index - 1
		}
	case "before", "after":
		if lane := domain.ResolveLane(snap, pos.ref); lane != nil {
			idx := laneIndexInTab(snap, lane.ID)
			if pos.mode == "after" {
				idx++
			}
			return idx
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

func moveDirection(kind string) msg.Direction {
	switch kind {
	case "up":
		return msg.DirUp
	case "down":
		return msg.DirDown
	case "left":
		return msg.DirLeft
	case "right":
		return msg.DirRight
	}
	return msg.DirUp
}

func edgeWord(kind string) string {
	switch kind {
	case "up":
		return "top"
	case "down":
		return "bottom"
	case "left":
		return "start"
	}
	return "end"
}

func paneIDsOfGroup(g *domain.PaneGroupSnapshot) []string {
	if g == nil {
		return nil
	}
	ids := make([]string, 0, len(g.Panes))
	for i := range g.Panes {
		ids = append(ids, g.Panes[i].ID)
	}
	return ids
}

func groupIndexInLane(lane *domain.LaneSnapshot, groupID string) int {
	if lane == nil {
		return -1
	}
	for i := range lane.PaneGroups {
		if lane.PaneGroups[i].ID == groupID {
			return i
		}
	}
	return -1
}

func laneIndexInTab(snap *domain.TabSnapshot, laneID string) int {
	if snap == nil {
		return -1
	}
	for i := range snap.Lanes {
		if snap.Lanes[i].ID == laneID {
			return i
		}
	}
	return -1
}

func laneLabel(lane *domain.LaneSnapshot) string {
	if lane == nil {
		return "?"
	}
	if lane.Name != "" {
		return lane.Name
	}
	return shortID(lane.ID)
}

// paneLabel names a pane the way a human referred to it: its given name if it
// has one, else its title, else a short id.
func (w *WorkspaceActor) paneLabel(paneID string) string {
	for _, info := range w.tabs {
		snap := w.queryTabSnapshot(info.id)
		if snap == nil {
			continue
		}
		if p := domain.FindPaneInTab(snap, paneID); p != nil {
			if p.GivenName != "" {
				return p.GivenName
			}
			if p.Title != "" {
				return p.Title
			}
		}
	}
	return shortID(paneID)
}

func orSelf(ref string) string {
	if ref == "" {
		return "(this pane)"
	}
	return ref
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// moveFail writes a refusal to the pane and returns it as the command's error,
// so a script sees a non-zero status for the same reason a human sees a line.
func moveFail(out *strings.Builder, format string, args ...interface{}) error {
	err := fmt.Errorf(format, args...)
	fmt.Fprintf(out, "\n[rysh] %v\n", err)
	return err
}

func (w *WorkspaceActor) moveUsage(out *strings.Builder) {
	ryshWriter(out).UsageIn("move",
		"##move pane [<pane>] to-lane <lane> [--tab <tab>]   pane becomes its own stack in that lane",
		"##move pane [<pane>] to-stacked-pane <pane>         join the stack that pane is in",
		"##move pane [<pane>] to-stack <stack-id>            same, addressing the stack directly",
		"##move pane [<pane>] to-tab <tab>                   into that tab's active lane",
		"##move pane [<pane>] to-new-lane [--tab <tab>]      into a lane created for it",
		"##move pane [<pane>] to-new-tab                     into a tab created for it",
		"##move pane [<pane>] here                           into the stack this command came from",
		"##move pane [<pane>] out                            leave the stack, into its own stack",
		"##move pane [<pane>] up|down                        reorder within its stack",
		"##move pane [<pane>] left|right                     to the neighbouring lane",
		"##move stack [<ref>] to-lane|to-tab|to-new-lane|to-new-tab ...  the whole stack, as one",
		"##move stack [<ref>] up|down|left|right             reorder / move to the neighbouring lane",
		"##move lane [<lane>] to-tab <tab> | to-new-tab      the whole lane, name and width intact",
		"##move lane [<lane>] left|right                     reorder within its tab",
		"##move tab [<tab>] left|right                       reorder the tab bar",
		"",
		"position: --first | --last (default) | --index <n> | --before <ref> | --after <ref>",
		"aliases:  to-stack = to-stacked-pane = to-pg = to-panegroup = to-group; out = unstack;",
		"          stack = pg = panegroup = group; pane = p",
		"--tab applies to to-lane / to-new-lane only: pane and stack ids are session-unique,",
		"so they already name their tab, while a lane index or name is only unique within one.",
		"omitting <pane>/<ref> means the pane this command was typed in. (alias: ##mv)",
	)
}
