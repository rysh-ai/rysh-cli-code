// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"time"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/google/uuid"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// ---------------------------------------------------------------------------
// Live pane transfer (##move)
// ---------------------------------------------------------------------------
//
// Moving a pane between stacks, lanes or tabs must not disturb what is RUNNING
// in it. A pane is a PTY plus a claude session that may be minutes into a turn;
// re-creating it at the destination and restoring its scrollback from KV would
// look right on screen and be wrong in every way that matters — the shell, its
// children, and the agent's in-memory state would all be gone.
//
// So a move never touches the PaneActor. It transfers BOOKKEEPING: the group's
// paneRefs entry, the PID, and the struct pointer move from one PaneGroupActor
// to another, and the pane keeps running throughout. Everything a pane is
// addressed by is keyed on its own id — the snapshot cascade asks NATS for
// `pane.{id}.snapshot`, input routes to `pane.{id}.rawinput` — so which group
// lists the id is the whole of "where the pane is".
//
// The one thing that is NOT bookkeeping is protoactor supervision. Panes used to
// be spawned as children of their pane group, and stopping a parent stops its
// children (actorContext.handleStop → stopAllChildren). A pane moved out of a
// group that was then closed would have been killed by the close. That is why
// panes are spawned detached (see spawnDetached) and stopped explicitly by the
// group that currently holds them.
//
// The transfer itself runs as in-process proto.actor request/reply — the same
// mechanism as the KV cascade (kv_cascade.go), and for the same reason: these
// messages carry live Go pointers, which no NATS codec could ever serialise.
// The call graph is strictly downward (workspace → tab → lane → group), so the
// nested futures cannot deadlock.
//
// Two things a move does NOT carry, both because they were captured before it:
//
//   - RYSH_LANE / RYSH_STACK / RYSH_TAB in the pane's shell environment. They
//     are set once when the shell is exec'd (paneIdentityEnv) and no process can
//     have its environment rewritten from outside. RYSH_PANE and RYSH_SESSION —
//     the two an agent actually addresses itself by — stay correct.
//   - The agentic ScopeIDs the pane's LLM executor was born with, which decide
//     which lane/stack-scoped forge and MCP tools it inherits. Those are read at
//     pane start; a moved pane keeps its old scope parents until the daemon
//     restarts. Model bindings are NOT affected: model scope is recomputed from
//     the live snapshot walk (model_scope.go), so it follows the move.

// moveHopTimeout bounds each hop of a transfer. Longer than the KV cascade's,
// because a hop here can spawn a pane group or a lane on the way through, and
// short enough that a wedged actor fails the move instead of stalling the
// workspace mailbox behind it.
const moveHopTimeout = 2 * time.Second

// paneHandle is a live pane in flight between two pane groups: its position
// metadata, its PID, and the actor struct itself. It exists only between a
// release and the adopt that follows; a handle that is dropped strands a running
// pane with nothing rendering it, which is why every caller either adopts it or
// puts it back.
type paneHandle struct {
	ref *paneRefInGroup
	pid *actor.PID
	pa  *PaneActor
}

// ID returns the moving pane's id ("" for a nil handle).
func (h *paneHandle) ID() string {
	if h == nil || h.ref == nil {
		return ""
	}
	return h.ref.id
}

// ---------------------------------------------------------------------------
// Message types (in-process only — they carry pointers)
// ---------------------------------------------------------------------------

// paneRehomeRequest tells a moved pane which tab/lane/stack it now lives in.
type paneRehomeRequest struct {
	tabID   string
	laneID  string
	groupID string
}

// groupReleasePaneRequest asks a PaneGroupActor to give up a pane without
// stopping it.
type groupReleasePaneRequest struct{ paneID string }

// groupReleasePaneReply carries the released pane. empty reports that the group
// has no panes left, so its owning lane can drop it.
type groupReleasePaneReply struct {
	handle *paneHandle
	empty  bool
	ok     bool
}

// groupAdoptPaneRequest asks a PaneGroupActor to take in a running pane at
// index (<0 or past the end appends).
type groupAdoptPaneRequest struct {
	handle *paneHandle
	index  int
}

type groupAdoptPaneReply struct{ ok bool }

// groupMovePaneRequest reorders one pane within its own stack.
type groupMovePaneRequest struct {
	paneID string
	dir    msg.Direction
}

type groupMovePaneReply struct{ ok bool }

// laneReleasePaneRequest asks a LaneActor to release a pane it holds, dropping
// the pane's group if that empties it.
type laneReleasePaneRequest struct{ paneID string }

// laneReleasePaneReply carries the released pane. empty reports that the lane
// has no stacks left, so its owning tab can drop it — the LANE is asked rather
// than the tab inferring it from laneRef.paneCount, which is a cached value
// refreshed on demand. Dropping a lane on a stale count would stop a lane that
// still held panes.
type laneReleasePaneReply struct {
	handle *paneHandle
	empty  bool
	ok     bool
}

// laneAdoptPaneRequest asks a LaneActor to take in a running pane. groupID
// selects an existing stack; "" creates a new single-pane stack at groupAt
// (<0 appends). index is the position within the destination stack.
type laneAdoptPaneRequest struct {
	handle  *paneHandle
	groupID string
	index   int
	groupAt int
	// rowFlex seeds a newly created group's height weight; 0 uses the lane's
	// own averaging rule. Carries a moved stack's height across lanes.
	rowFlex int
}

type laneAdoptPaneReply struct {
	groupID string
	ok      bool
}

// laneMoveGroupRequest reorders one stack within its lane.
type laneMoveGroupRequest struct {
	groupID string
	dir     msg.Direction
}

type laneMoveGroupReply struct{ ok bool }

// tabReleasePaneRequest asks a TabActor to release a pane, dropping the pane's
// lane if that empties it.
type tabReleasePaneRequest struct{ paneID string }

type tabReleasePaneReply struct {
	handle *paneHandle
	ok     bool
}

// tabAdoptPaneRequest asks a TabActor to take in a running pane. laneID selects
// an existing lane; "" creates a new lane at laneAt (<0 appends).
type tabAdoptPaneRequest struct {
	handle  *paneHandle
	laneID  string
	groupID string
	index   int
	groupAt int
	laneAt  int
	rowFlex int
	// laneName / laneFlex seed a newly created lane, carrying a moved lane's
	// name and width across tabs.
	laneName string
	laneFlex int
}

type tabAdoptPaneReply struct {
	laneID  string
	groupID string
	ok      bool
}

// tabMovePaneInStackRequest reorders a pane within its stack, addressed from the
// tab (the workspace does not know which lane holds it).
type tabMovePaneInStackRequest struct {
	paneID string
	dir    msg.Direction
}

// tabMoveStackRequest reorders a stack within its lane. laneID is resolved by
// the caller from the tab snapshot — the tab keeps a pane→lane map but no
// group→lane one, and building one by reading its lanes' structs from another
// goroutine is exactly the race kv_cascade.go exists to avoid.
type tabMoveStackRequest struct {
	laneID  string
	groupID string
	dir     msg.Direction
}

// tabMoveLaneRequest reorders a lane within its tab.
type tabMoveLaneRequest struct {
	laneID string
	dir    msg.Direction
}

type tabMoveReply struct{ ok bool }

// requestMove performs one hop of a transfer. ok is false when the actor is
// gone, times out, or answers with an unexpected type — the caller then aborts
// the move rather than half-applying it.
func requestMove[T any](ctx actor.Context, pid *actor.PID, request interface{}) (T, bool) {
	var zero T
	if pid == nil {
		return zero, false
	}
	res, err := ctx.RequestFuture(pid, request, moveHopTimeout).Result()
	if err != nil {
		return zero, false
	}
	reply, ok := res.(T)
	if !ok {
		return zero, false
	}
	return reply, true
}

// ---------------------------------------------------------------------------
// Pane group: release / adopt / reorder
// ---------------------------------------------------------------------------

// releasePane removes a pane from this group's bookkeeping WITHOUT stopping it,
// and hands it back for another group to adopt.
//
// Unlike deletePaneByID this is allowed to empty the group: the group's own
// last pane can move away, and the lane drops the emptied group afterwards.
// Approval panes refuse — they are ephemeral UI owned by this group and have no
// PaneActor to carry.
func (g *PaneGroupActor) releasePane(paneID string) (*paneHandle, bool, bool) {
	idx := -1
	for i, ref := range g.paneRefs {
		if ref.id == paneID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, false, false
	}
	ref := g.paneRefs[idx]
	if ref.paneType == "approval" {
		return nil, false, false
	}
	pid, hasPID := g.panePIDs[paneID]
	pa, hasActor := g.paneActors[paneID]
	if !hasPID || !hasActor || pid == nil || pa == nil {
		return nil, false, false
	}

	delete(g.panePIDs, paneID)
	delete(g.paneActors, paneID)
	delete(g.paneSubjects, paneID)

	wasActive := g.activePane == idx
	g.paneRefs = append(g.paneRefs[:idx], g.paneRefs[idx+1:]...)
	// Same index fixup as deletePaneByID: the successor takes the slot, and a
	// released tail falls back to the pane before it.
	switch {
	case wasActive:
		if idx >= len(g.paneRefs) {
			g.activePane = len(g.paneRefs) - 1
		} else {
			g.activePane = idx
		}
	case g.activePane > idx:
		g.activePane--
	}
	if g.activePane < 0 {
		g.activePane = 0
	}

	return &paneHandle{ref: ref, pid: pid, pa: pa}, len(g.paneRefs) == 0, true
}

// adoptPane takes a running pane into this group at index, and tells the pane
// where it now lives so a shell started from here on inherits the right
// coordinates.
func (g *PaneGroupActor) adoptPane(ctx actor.Context, h *paneHandle, index int) bool {
	if h == nil || h.ref == nil || h.pid == nil || h.pa == nil {
		return false
	}
	if _, exists := g.panePIDs[h.ref.id]; exists {
		return false
	}
	if index < 0 || index > len(g.paneRefs) {
		index = len(g.paneRefs)
	}

	g.paneRefs = append(g.paneRefs, nil)
	copy(g.paneRefs[index+1:], g.paneRefs[index:])
	g.paneRefs[index] = h.ref

	g.panePIDs[h.ref.id] = h.pid
	g.paneActors[h.ref.id] = h.pa
	g.paneSubjects[h.ref.id] = msg.T("pane", h.ref.id, "inbox")
	g.activePane = index

	// The pane owns these fields; it updates them on its own goroutine. Nil ctx
	// is the unit-test path, where there is no actor system and no pane behind
	// the PID — the bookkeeping above is the whole of what those tests assert.
	if ctx != nil {
		ctx.Send(h.pid, &paneRehomeRequest{tabID: g.tabID, laneID: g.laneID, groupID: g.id})
	}
	return true
}

// movePaneInStack reorders one named pane within this stack, whether or not it
// is the active one. moveStackedPaneUp/Down are the keyboard path and always
// act on the active pane; ##move has to be able to name a pane it is not
// sitting in.
func (g *PaneGroupActor) movePaneInStack(paneID string, dir msg.Direction) bool {
	idx := -1
	for i, ref := range g.paneRefs {
		if ref.id == paneID {
			idx = i
			break
		}
	}
	if idx < 0 || len(g.paneRefs) <= 1 {
		return false
	}
	switch dir {
	case msg.DirUp, msg.DirPrev, msg.DirLeft:
		if idx <= 0 {
			return false
		}
		g.paneRefs[idx-1], g.paneRefs[idx] = g.paneRefs[idx], g.paneRefs[idx-1]
		g.activePane = idx - 1
	case msg.DirDown, msg.DirNext, msg.DirRight:
		if idx >= len(g.paneRefs)-1 {
			return false
		}
		g.paneRefs[idx+1], g.paneRefs[idx] = g.paneRefs[idx], g.paneRefs[idx+1]
		g.activePane = idx + 1
	default:
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// Lane: release / adopt / reorder
// ---------------------------------------------------------------------------

// releasePane forwards a release to the group holding the pane and drops that
// group if the release emptied it. Stopping an emptied group is safe precisely
// because panes are spawned detached — the pane that just left is not a child of
// the group being stopped.
func (l *LaneActor) releasePane(ctx actor.Context, paneID string) (*paneHandle, bool, bool) {
	groupID, ok := l.paneToGroup[paneID]
	if !ok {
		return nil, false, false
	}
	reply, ok := requestMove[*groupReleasePaneReply](ctx, l.groupPIDs[groupID], &groupReleasePaneRequest{paneID: paneID})
	if !ok || reply == nil || !reply.ok {
		return nil, false, false
	}
	delete(l.paneToGroup, paneID)

	for i, gr := range l.groupRefs {
		if gr.id != groupID {
			continue
		}
		if gr.paneCount > 0 {
			gr.paneCount--
		}
		if reply.empty {
			if pid, ok := l.groupPIDs[groupID]; ok {
				ctx.Stop(pid)
			}
			delete(l.groupPIDs, groupID)
			delete(l.groupActors, groupID)
			delete(l.groupSubjects, groupID)
			l.groupRefs = append(l.groupRefs[:i], l.groupRefs[i+1:]...)
			if l.activeGroup >= len(l.groupRefs) {
				l.activeGroup = len(l.groupRefs) - 1
			}
			if l.activeGroup < 0 {
				l.activeGroup = 0
			}
		}
		break
	}
	l.updateActivePaneFromGroup()
	return reply.handle, len(l.groupRefs) == 0, true
}

// adoptPane takes a running pane into this lane. An empty groupID creates a new
// single-pane stack; otherwise the pane joins the named one. Returns the id of
// the stack the pane landed in.
func (l *LaneActor) adoptPane(ctx actor.Context, req *laneAdoptPaneRequest) (string, bool) {
	if req == nil || req.handle == nil {
		return "", false
	}
	groupID := req.groupID
	if groupID == "" {
		groupID = l.createEmptyGroup(ctx, req.groupAt, req.rowFlex)
		if groupID == "" {
			return "", false
		}
	}
	pid, ok := l.groupPIDs[groupID]
	if !ok {
		return "", false
	}
	reply, ok := requestMove[*groupAdoptPaneReply](ctx, pid, &groupAdoptPaneRequest{handle: req.handle, index: req.index})
	if !ok || reply == nil || !reply.ok {
		return "", false
	}

	l.paneToGroup[req.handle.ID()] = groupID
	for i, gr := range l.groupRefs {
		if gr.id == groupID {
			gr.paneCount++
			gr.activePaneID = req.handle.ID()
			l.activeGroup = i
			break
		}
	}
	return groupID, true
}

// createEmptyGroup spawns a pane group with no initial pane, ready to adopt one.
// Every other creation path is born holding a pane; this is the only shape a
// move can use, because the pane it will hold already exists.
func (l *LaneActor) createEmptyGroup(ctx actor.Context, at, rowFlex int) string {
	groupID := uuid.NewString()
	ga := NewPaneGroupActor(groupID, l.tabID, l.id, "", "", l.cfg, l.pub, l.nc, l.agSetup, l.kvStore, l.secrets)
	pid := ctx.Spawn(actor.PropsFromProducer(func() actor.Actor { return ga }))

	l.groupPIDs[groupID] = pid
	l.groupActors[groupID] = ga
	l.groupSubjects[groupID] = msg.T("pane-group", groupID, "inbox")

	if rowFlex < 1 {
		// A group with no explicit weight joins at the average of its siblings,
		// exactly as createPaneGroup does — see the rationale there.
		rowFlex = defaultFlex
		if n := len(l.groupRefs); n > 0 {
			total := 0
			for _, gr := range l.groupRefs {
				total += gr.rowFlex
			}
			if rowFlex = total / n; rowFlex < 1 {
				rowFlex = 1
			}
		}
	}

	ref := &laneGroupRef{id: groupID, rowFlex: rowFlex}
	if at < 0 || at > len(l.groupRefs) {
		at = len(l.groupRefs)
	}
	l.groupRefs = append(l.groupRefs, nil)
	copy(l.groupRefs[at+1:], l.groupRefs[at:])
	l.groupRefs[at] = ref
	l.activeGroup = at
	return groupID
}

// moveGroup reorders a named stack within this lane. moveActiveGroup is the
// keyboard path and only ever moves the active one.
func (l *LaneActor) moveGroup(groupID string, dir msg.Direction) bool {
	idx := -1
	for i, gr := range l.groupRefs {
		if gr.id == groupID {
			idx = i
			break
		}
	}
	if idx < 0 || len(l.groupRefs) <= 1 {
		return false
	}
	switch dir {
	case msg.DirUp, msg.DirPrev, msg.DirLeft:
		if idx <= 0 {
			return false
		}
		l.groupRefs[idx-1], l.groupRefs[idx] = l.groupRefs[idx], l.groupRefs[idx-1]
		l.activeGroup = idx - 1
	case msg.DirDown, msg.DirNext, msg.DirRight:
		if idx >= len(l.groupRefs)-1 {
			return false
		}
		l.groupRefs[idx+1], l.groupRefs[idx] = l.groupRefs[idx], l.groupRefs[idx+1]
		l.activeGroup = idx + 1
	default:
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// Tab: release / adopt / reorder
// ---------------------------------------------------------------------------

// releasePane forwards a release to the lane holding the pane and drops that
// lane if the release emptied it.
func (t *TabActor) releasePane(ctx actor.Context, paneID string) (*paneHandle, bool) {
	laneID, ok := t.paneToLane[paneID]
	if !ok {
		return nil, false
	}
	reply, ok := requestMove[*laneReleasePaneReply](ctx, t.lanePIDs[laneID], &laneReleasePaneRequest{paneID: paneID})
	if !ok || reply == nil || !reply.ok {
		return nil, false
	}
	delete(t.paneToLane, paneID)

	for i, lr := range t.laneRefs {
		if lr.id != laneID {
			continue
		}
		if lr.paneCount > 0 {
			lr.paneCount--
		}
		if reply.empty {
			if pid, ok := t.lanePIDs[laneID]; ok {
				ctx.Stop(pid)
			}
			delete(t.lanePIDs, laneID)
			delete(t.laneActors, laneID)
			delete(t.laneSubjects, laneID)
			t.laneRefs = append(t.laneRefs[:i], t.laneRefs[i+1:]...)
			if t.activeLane >= len(t.laneRefs) {
				t.activeLane = len(t.laneRefs) - 1
			}
			if t.activeLane < 0 {
				t.activeLane = 0
			}
			t.normalizeFlex()
		}
		break
	}
	t.updateActivePaneFromLane()
	return reply.handle, true
}

// adoptPane takes a running pane into this tab. An empty laneID creates a new
// lane; an empty groupID creates a new stack within the destination lane.
func (t *TabActor) adoptPane(ctx actor.Context, req *tabAdoptPaneRequest) (string, string, bool) {
	if req == nil || req.handle == nil {
		return "", "", false
	}
	laneID := req.laneID
	if laneID == "" {
		laneID = t.createEmptyLane(ctx, req.laneAt, req.laneName, req.laneFlex)
		if laneID == "" {
			return "", "", false
		}
	}
	pid, ok := t.lanePIDs[laneID]
	if !ok {
		return "", "", false
	}
	reply, ok := requestMove[*laneAdoptPaneReply](ctx, pid, &laneAdoptPaneRequest{
		handle:  req.handle,
		groupID: req.groupID,
		index:   req.index,
		groupAt: req.groupAt,
		rowFlex: req.rowFlex,
	})
	if !ok || reply == nil || !reply.ok {
		return "", "", false
	}

	t.paneToLane[req.handle.ID()] = laneID
	for i, lr := range t.laneRefs {
		if lr.id == laneID {
			lr.paneCount++
			lr.activePaneID = req.handle.ID()
			t.activeLane = i
			break
		}
	}
	return laneID, reply.groupID, true
}

// createEmptyLane spawns a lane with no initial pane group, ready to adopt one.
func (t *TabActor) createEmptyLane(ctx actor.Context, at int, name string, flex int) string {
	laneID := uuid.NewString()
	if flex < 1 {
		flex = defaultFlex
		if n := len(t.laneRefs); n > 0 {
			total := 0
			for _, lr := range t.laneRefs {
				total += lr.flex
			}
			if flex = total / n; flex < 1 {
				flex = 1
			}
		}
	}
	la := NewLaneActor(laneID, t.id, flex, "", "", t.cfg, t.pub, t.nc, t.agSetup, t.kvStore, t.secrets)
	la.SetName(name)
	pid := ctx.Spawn(actor.PropsFromProducer(func() actor.Actor { return la }))

	t.lanePIDs[laneID] = pid
	t.laneActors[laneID] = la
	t.laneSubjects[laneID] = msg.T("lane", laneID, "inbox")

	ref := &laneRef{id: laneID, flex: flex}
	if at < 0 || at > len(t.laneRefs) {
		at = len(t.laneRefs)
	}
	t.laneRefs = append(t.laneRefs, nil)
	copy(t.laneRefs[at+1:], t.laneRefs[at:])
	t.laneRefs[at] = ref
	t.activeLane = at
	t.normalizeFlex()
	return laneID
}

// moveLane reorders a named lane within this tab.
func (t *TabActor) moveLane(laneID string, dir msg.Direction) bool {
	idx := -1
	for i, lr := range t.laneRefs {
		if lr.id == laneID {
			idx = i
			break
		}
	}
	if idx < 0 || len(t.laneRefs) <= 1 {
		return false
	}
	switch dir {
	case msg.DirLeft, msg.DirPrev, msg.DirUp:
		if idx <= 0 {
			return false
		}
		t.laneRefs[idx-1], t.laneRefs[idx] = t.laneRefs[idx], t.laneRefs[idx-1]
		t.activeLane = idx - 1
	case msg.DirRight, msg.DirNext, msg.DirDown:
		if idx >= len(t.laneRefs)-1 {
			return false
		}
		t.laneRefs[idx+1], t.laneRefs[idx] = t.laneRefs[idx], t.laneRefs[idx+1]
		t.activeLane = idx + 1
	default:
		return false
	}
	return true
}
