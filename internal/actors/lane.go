// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	petname "github.com/dustinkirkland/golang-petname"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/agentic"
	"github.com/rysh-ai/rysh-cli-code/internal/bridge"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// laneGroupRef records the metadata for a pane group within a lane.
type laneGroupRef struct {
	id           string
	rowFlex      int
	activePaneID string
	paneCount    int
}

// LaneActor manages the pane groups within a single lane (column).
//
// All fields are unguarded -- the proto.actor mailbox guarantees sequential Receive().
type LaneActor struct {
	id      string
	tabID   string // owning tab id (threaded to groups/panes for scoped-tool resolution)
	flex    int
	name    string // user-assigned lane name; empty means "inherit the tab's pipeline name"
	cfg     config.Config
	pub     *msg.NATSPublisher
	agSetup *agentic.Setup
	nc      *nats.Conn
	kvStore nats.KeyValue   // rysh-panes bucket
	secrets *secretResolver // workspace-scoped ##secret lookup, threaded to panes
	br      *bridge.NATSBridge

	groupRefs     []*laneGroupRef
	activeGroup   int
	groupSubjects map[string]string // groupID -> inbox subject
	groupPIDs     map[string]*actor.PID
	groupActors   map[string]*PaneGroupActor
	paneToGroup   map[string]string // paneID -> groupID

	// Initial pane info -- set at creation, consumed on *actor.Started.
	initialPaneID string
	initialTitle  string

	// initialExtraPanes seeds additional pane groups (beyond the initial one)
	// during *actor.Started, used by the grid seed path so a lane is built with
	// multiple panes synchronously. Consumed once and set to nil.
	initialExtraPanes []panePair

	restoreData *laneKV
}

// panePair carries a pre-generated pane ID and title for inline seeding.
type panePair struct {
	id    string
	title string
}

// NewLaneActor creates a new LaneActor with an initial pane group.
func NewLaneActor(
	id string,
	tabID string,
	flex int,
	initialPaneID, initialTitle string,
	cfg config.Config,
	pub *msg.NATSPublisher,
	nc *nats.Conn,
	agSetup *agentic.Setup,
	kvStore nats.KeyValue,
	secrets *secretResolver,
) *LaneActor {
	return &LaneActor{
		id:            id,
		tabID:         tabID,
		flex:          flex,
		initialPaneID: initialPaneID,
		initialTitle:  initialTitle,
		cfg:           cfg,
		pub:           pub,
		agSetup:       agSetup,
		nc:            nc,
		kvStore:       kvStore,
		secrets:       secrets,
		groupSubjects: make(map[string]string),
		groupPIDs:     make(map[string]*actor.PID),
		groupActors:   make(map[string]*PaneGroupActor),
		paneToGroup:   make(map[string]string),
	}
}

// NewLaneActorFromKV creates a LaneActor that will restore state from KV data
// on its first *actor.Started message.
func NewLaneActorFromKV(
	tabID string,
	cfg config.Config,
	pub *msg.NATSPublisher,
	nc *nats.Conn,
	agSetup *agentic.Setup,
	kvStore nats.KeyValue,
	secrets *secretResolver,
	kv laneKV,
) *LaneActor {
	la := NewLaneActor(kv.ID, tabID, kv.Flex, "", "", cfg, pub, nc, agSetup, kvStore, secrets)
	la.restoreData = &kv
	return la
}

// Receive implements actor.Actor.
func (l *LaneActor) Receive(ctx actor.Context) {
	switch m := ctx.Message().(type) {
	case *actor.Started:
		l.br = bridge.New(l.nc, ctx.Self(), ctx.ActorSystem(), l.pub.Codecs())
		_ = l.br.AddSubject(msg.T("lane", l.id, "inbox"))
		_ = l.br.AddSubject(msg.T("lane", l.id, "snapshot"))

		if l.restoreData != nil {
			l.doRestoreFromKV(ctx, *l.restoreData)
			l.restoreData = nil
		} else if l.initialPaneID != "" {
			l.createPaneGroup(ctx, l.initialPaneID, l.initialTitle, "", "", "", nil)
			l.initialPaneID = ""
			l.initialTitle = ""
			// Grid seed: build the remaining pane groups inline.
			for _, p := range l.initialExtraPanes {
				l.createPaneGroup(ctx, p.id, p.title, "", "", "", nil)
			}
			l.initialExtraPanes = nil
			if len(l.groupRefs) > 1 {
				l.equalizeGroups()
				l.activeGroup = 0
			}
		}
		replayScope(l.agSetup, agentic.ScopeLane, agentic.ScopeIDs{TabID: l.tabID, LaneID: l.id})

	case *actor.Stopping:
		teardownScope(l.agSetup, agentic.ScopeLane, l.id)
		if l.br != nil {
			l.br.Stop()
			l.br = nil
		}

	case *msg.MsgSetWorkingDir:
		// Update this lane's config copy (used when it spawns new groups/panes)
		// and forward to existing groups.
		l.cfg.WorkingDirectory = m.Dir
		for _, gr := range l.groupRefs {
			_ = l.pub.Send(msg.T("pane-group", gr.id, "inbox"), &msg.MsgSetWorkingDir{Dir: m.Dir})
		}

	case *msg.MsgLaneCreatePaneGroup:
		l.createPaneGroup(ctx, m.PaneID, m.Title, m.GroupID, m.WorkingDir, m.PaneType, m.Meta)

	case *msg.MsgLaneClosePaneGroup:
		l.closeActiveGroup(ctx)

	case *msg.MsgLaneCloseActivePane:
		l.closeActivePaneInGroup(ctx)

	case *msg.MsgLaneFocusGroup:
		switch m.Direction {
		case msg.DirUp:
			if len(l.groupRefs) > 1 && l.activeGroup > 0 {
				l.activeGroup--
				l.updateActivePaneFromGroup()
			}
		case msg.DirDown:
			if len(l.groupRefs) > 1 && l.activeGroup < len(l.groupRefs)-1 {
				l.activeGroup++
				l.updateActivePaneFromGroup()
			}
		}

	case *msg.MsgLaneFocusPaneByID:
		l.focusPaneByID(m.ID)

	case *msg.MsgLaneCreateStackedPane:
		l.createStackedPaneInActiveGroup(m.PaneID, m.Title)

	case *msg.MsgLaneStackedPane:
		l.forwardToActiveGroup(&msg.MsgPaneGroupStackedPane{Direction: m.Direction})
		l.updateActivePaneFromGroup()

	case *msg.MsgLaneStackedPaneSelect:
		l.forwardToActiveGroup(&msg.MsgPaneGroupStackedPaneSelect{Index: m.Index})
		l.updateActivePaneFromGroup()

	case *msg.MsgLaneSetPaneHidden:
		// Addressed at the group that HOLDS the pane, not the active one: a
		// pane can be hidden from anywhere, including a lane the human is not
		// looking at.
		if groupID, ok := l.paneToGroup[m.PaneID]; ok {
			if subject, ok := l.groupSubjects[groupID]; ok {
				_ = l.pub.Send(subject, &msg.MsgPaneGroupSetPaneHidden{PaneID: m.PaneID, Hidden: m.Hidden})
			}
		}

	case *msg.MsgLaneStackedPaneMove:
		// If the active group is a real stack (>1 pane), reorder the active
		// pane within that stack. Otherwise reorder the whole group within the
		// lane's vertical column, so ctrl+y also reorders single-pane groups
		// stacked with ctrl+p v (split-down).
		if l.ActiveGroupPaneCount() > 1 {
			l.forwardToActiveGroup(&msg.MsgPaneGroupStackedPaneMove{Direction: m.Direction})
		} else {
			l.moveActiveGroup(m.Direction)
		}
		l.updateActivePaneFromGroup()

	case *msg.MsgLaneResizeGroupHeight:
		slog.Debug("lane: received MsgLaneResizeGroupHeight", "delta", m.Delta, "numGroups", len(l.groupRefs), "activeGroup", l.activeGroup)
		l.resizeActiveGroup(m.Delta)

	case *msg.MsgLaneEqualizeGroups:
		slog.Debug("lane: received MsgLaneEqualizeGroups", "numGroups", len(l.groupRefs))
		l.equalizeGroups()

	// Serialise for persistence on THIS actor's goroutine, cascading to the
	// child pane groups. See kv_cascade.go.
	case *laneKVRequest:
		ctx.Respond(&laneKVReply{kv: l.toKVViaActors(ctx)})

	// --- CLI targeted operations ---

	case *msg.MsgLaneDeletePaneGroup:
		l.deleteGroupByID(ctx, m.PaneGroupID)

	case *msg.MsgLaneCreateStackedPaneInGroup:
		l.createStackedPaneInGroup(m.PaneGroupID, m.PaneID, m.Title)

	// --- live pane transfer (##move); see pane_move.go ---

	case *laneReleasePaneRequest:
		handle, empty, ok := l.releasePane(ctx, m.paneID)
		ctx.Respond(&laneReleasePaneReply{handle: handle, empty: empty, ok: ok})

	case *laneAdoptPaneRequest:
		groupID, ok := l.adoptPane(ctx, m)
		ctx.Respond(&laneAdoptPaneReply{groupID: groupID, ok: ok})

	case *laneMoveGroupRequest:
		ctx.Respond(&laneMoveGroupReply{ok: l.moveGroup(m.groupID, m.dir)})

	case *groupMovePaneRequest:
		// Reorder a pane inside whichever of this lane's stacks holds it.
		ok := false
		if groupID, found := l.paneToGroup[m.paneID]; found {
			if reply, got := requestMove[*groupMovePaneReply](ctx, l.groupPIDs[groupID], m); got && reply != nil {
				ok = reply.ok
			}
			if ok {
				l.updateActivePaneFromGroup()
			}
		}
		ctx.Respond(&groupMovePaneReply{ok: ok})

	case *msg.RequestEnvelope:
		switch inner := m.Inner.(type) {
		case *msg.MsgGetLaneSnapshot:
			snap := l.collectSnapshot(inner.LayoutOnly, inner.NoHistories)
			_ = m.Reply(&msg.MsgLaneSnapshotReply{Snapshot: snap})
		case *msg.MsgGetLaneActivePane:
			paneID := ""
			totalPanes := 0
			for _, gr := range l.groupRefs {
				totalPanes += gr.paneCount
			}
			if len(l.groupRefs) > 0 && l.activeGroup >= 0 && l.activeGroup < len(l.groupRefs) {
				paneID = l.groupRefs[l.activeGroup].activePaneID
			}
			_ = m.Reply(&msg.MsgLaneActivePaneReply{PaneID: paneID, PaneCount: totalPanes})
		}
	}
}

// ---------------------------------------------------------------------------
// PaneGroup lifecycle
// ---------------------------------------------------------------------------

// paneGroupCfg is the config a new pane group is born with: the lane's config,
// with WorkingDirectory overridden when the group is created inside a worktree
// (design 008). Kept as a pure function so the override is testable without an
// actor system.
func paneGroupCfg(base config.Config, workingDir string) config.Config {
	if workingDir != "" {
		base.WorkingDirectory = workingDir
	}
	return base
}

// createPaneGroup spawns a new pane group. groupID pre-assigns the group's ID
// ("" generates one) and workingDir overrides the lane config's working
// directory for the group's panes — both are the worktree-lifecycle birth
// parameters (design 008; see MsgLaneCreatePaneGroup). paneType marks the
// initial pane as a special variant ("replay" panes never start a shell);
// empty = normal pane.
func (l *LaneActor) createPaneGroup(ctx actor.Context, paneID, title, groupID, workingDir, paneType string, meta map[string]string) {
	if groupID == "" {
		groupID = uuid.NewString()
	}
	if paneID == "" {
		paneID = uuid.NewString()
	}
	if title == "" {
		title = petname.Generate(2, "-")
	}

	ga := NewPaneGroupActor(groupID, l.tabID, l.id, paneID, title, paneGroupCfg(l.cfg, workingDir), l.pub, l.nc, l.agSetup, l.kvStore, l.secrets)
	ga.initialPaneType = paneType
	ga.initialMeta = meta
	groupProps := actor.PropsFromProducer(func() actor.Actor { return ga })
	pid := ctx.Spawn(groupProps)

	l.groupPIDs[groupID] = pid
	l.groupActors[groupID] = ga
	l.groupSubjects[groupID] = msg.T("pane-group", groupID, "inbox")
	l.paneToGroup[paneID] = groupID

	// A new group joins at the AVERAGE of its siblings' weights and does not
	// resize them — the vertical counterpart of createLane's rule. Keeps an
	// equal column equal, and lets the new group blend into a deliberately
	// uneven one instead of dominating it. See createLane for the full rationale.
	rowFlex := defaultFlex
	if n := len(l.groupRefs); n > 0 {
		total := 0
		for _, gr := range l.groupRefs {
			total += gr.rowFlex
		}
		if rowFlex = total / n; rowFlex < 1 {
			rowFlex = 1
		}
	}

	ref := &laneGroupRef{
		id:           groupID,
		rowFlex:      rowFlex,
		activePaneID: paneID,
		paneCount:    1,
	}

	l.groupRefs = append(l.groupRefs, ref)
	l.activeGroup = len(l.groupRefs) - 1
}

// closeActivePaneInGroup deletes the active (top) pane from the active group's stack.
// The group must have more than one pane; if it has only one, this is a no-op.
func (l *LaneActor) closeActivePaneInGroup(ctx actor.Context) {
	if len(l.groupRefs) == 0 || l.activeGroup < 0 || l.activeGroup >= len(l.groupRefs) {
		return
	}
	gr := l.groupRefs[l.activeGroup]
	if gr.paneCount <= 1 {
		return // only one pane -- use closeActiveGroup instead
	}

	paneID := gr.activePaneID
	if paneID == "" {
		return
	}

	// Remove the pane-to-group mapping.
	delete(l.paneToGroup, paneID)

	// Tell the PaneGroupActor to delete this specific pane.
	if subj, ok := l.groupSubjects[gr.id]; ok {
		_ = l.pub.Send(subj, &msg.MsgPaneGroupDeletePane{PaneID: paneID})
	}

	gr.paneCount--

	// Update the active pane ID from the group after the delete.
	l.updateActivePaneFromGroup()
}

func (l *LaneActor) closeActiveGroup(ctx actor.Context) {
	if len(l.groupRefs) <= 1 || l.activeGroup < 0 || l.activeGroup >= len(l.groupRefs) {
		return
	}

	idx := l.activeGroup
	removed := l.groupRefs[idx]

	// Stop the group actor (which stops all its child pane actors).
	if pid, ok := l.groupPIDs[removed.id]; ok {
		ctx.Stop(pid)
	}

	// Delete KV entries for all panes in the group.
	if ga, ok := l.groupActors[removed.id]; ok {
		ga.DeleteAllPaneKV()
		for _, pID := range ga.PaneIDs() {
			delete(l.paneToGroup, pID)
		}
	}

	// Do NOT redistribute the removed group's rowFlex onto a single neighbor.
	// Height is proportional to the sum of all group rowFlex weights, so simply
	// dropping the group returns its space to every survivor proportionally,
	// keeping the column balanced. (The old code added the whole removed weight
	// to one neighbor, doubling it: Family-A of the layout-drift bug.)
	delete(l.groupPIDs, removed.id)
	delete(l.groupActors, removed.id)
	delete(l.groupSubjects, removed.id)

	l.groupRefs = append(l.groupRefs[:idx], l.groupRefs[idx+1:]...)
	if idx >= len(l.groupRefs) {
		l.activeGroup = len(l.groupRefs) - 1
	} else {
		l.activeGroup = idx
	}

	l.updateActivePaneFromGroup()
}

// deleteGroupByID deletes a specific pane group identified by ID.
func (l *LaneActor) deleteGroupByID(ctx actor.Context, groupID string) {
	idx := -1
	for i, gr := range l.groupRefs {
		if gr.id == groupID {
			idx = i
			break
		}
	}
	if idx < 0 || len(l.groupRefs) <= 1 {
		return
	}

	removed := l.groupRefs[idx]

	if pid, ok := l.groupPIDs[removed.id]; ok {
		ctx.Stop(pid)
	}
	if ga, ok := l.groupActors[removed.id]; ok {
		ga.DeleteAllPaneKV()
		for _, pID := range ga.PaneIDs() {
			delete(l.paneToGroup, pID)
		}
	}

	// Drop the group without dumping its rowFlex onto one neighbor; the renderer
	// returns the freed height to all survivors proportionally. (See
	// closeActiveGroup — Family-A of the layout-drift bug.)
	delete(l.groupPIDs, removed.id)
	delete(l.groupActors, removed.id)
	delete(l.groupSubjects, removed.id)

	l.groupRefs = append(l.groupRefs[:idx], l.groupRefs[idx+1:]...)
	if l.activeGroup >= len(l.groupRefs) {
		l.activeGroup = len(l.groupRefs) - 1
	}

	l.updateActivePaneFromGroup()
}

// createStackedPaneInGroup creates a stacked pane in a specific pane group by ID.
func (l *LaneActor) createStackedPaneInGroup(groupID, paneID, title string) {
	subject, ok := l.groupSubjects[groupID]
	if !ok {
		return
	}
	_ = l.pub.Send(subject, &msg.MsgPaneGroupCreateStackedPane{PaneID: paneID, Title: title})
	l.paneToGroup[paneID] = groupID

	for _, gr := range l.groupRefs {
		if gr.id == groupID {
			gr.paneCount++
			gr.activePaneID = paneID
			break
		}
	}
}

// ---------------------------------------------------------------------------
// Stacked pane helpers
// ---------------------------------------------------------------------------

func (l *LaneActor) forwardToActiveGroup(message interface{}) {
	if len(l.groupRefs) == 0 || l.activeGroup < 0 || l.activeGroup >= len(l.groupRefs) {
		return
	}
	gr := l.groupRefs[l.activeGroup]
	subject, ok := l.groupSubjects[gr.id]
	if !ok {
		return
	}
	_ = l.pub.Send(subject, message)
}

func (l *LaneActor) createStackedPaneInActiveGroup(paneID, title string) {
	if len(l.groupRefs) == 0 || l.activeGroup < 0 || l.activeGroup >= len(l.groupRefs) {
		return
	}
	gr := l.groupRefs[l.activeGroup]
	subject, ok := l.groupSubjects[gr.id]
	if !ok {
		return
	}
	if paneID == "" {
		paneID = uuid.NewString()
	}
	if title == "" {
		title = petname.Generate(2, "-")
	}

	_ = l.pub.Send(subject, &msg.MsgPaneGroupCreateStackedPane{PaneID: paneID, Title: title})
	l.paneToGroup[paneID] = gr.id
	gr.paneCount++
	gr.activePaneID = paneID // new pane is now on top
}

// ---------------------------------------------------------------------------
// Resize
// ---------------------------------------------------------------------------

// resizeActiveGroup is resizeActiveLane's vertical twin, and follows the same
// rule on the same reasoning — read that one first. dir > 0 (↓) makes the ACTIVE
// group taller, dir < 0 (↑) shorter; the height is taken from — or handed back
// to — the group BELOW it, so only the boundary under the active group moves and
// its top edge stays where it is.
//
// The bottommost group is the exception the anchor cannot cover: its lower edge
// is the bottom of the lane. It borrows from the group above instead, and ↓
// still means taller.
//
// dir keeps its screen-direction encoding (+1 = down) so callers are unchanged.
// Note that a CLIENT has to agree on that encoding for any of this to be true on
// screen: the desktop app sent ↑ as +1 and ↓ as -1 for the height axis, which
// inverted every vertical resize there (fixed in rysh-cli-app, same change).
func (l *LaneActor) resizeActiveGroup(dir int) {
	if dir == 0 || len(l.groupRefs) < 2 {
		return
	}

	// The partner is always the group BELOW — that is the boundary the arrows
	// are allowed to move. Only the bottommost group, which has none, falls
	// back to the group above it.
	grow := dir > 0
	neighborIdx := l.activeGroup + 1
	if neighborIdx >= len(l.groupRefs) {
		neighborIdx = l.activeGroup - 1
	}
	if neighborIdx < 0 {
		return
	}

	// Compute 10% of total rowFlex as the resize increment.
	totalRowFlex := 0
	for _, gr := range l.groupRefs {
		totalRowFlex += gr.rowFlex
	}
	d := totalRowFlex / 10
	if d < 1 {
		d = 1
	}

	const minFlex = 1
	// The group that shrinks must keep at least minFlex.
	shrinkIdx := neighborIdx
	if !grow {
		shrinkIdx = l.activeGroup
	}
	if l.groupRefs[shrinkIdx].rowFlex-d < minFlex {
		d = l.groupRefs[shrinkIdx].rowFlex - minFlex
	}
	if d <= 0 {
		return
	}
	if grow {
		l.groupRefs[l.activeGroup].rowFlex += d
		l.groupRefs[neighborIdx].rowFlex -= d
	} else {
		l.groupRefs[l.activeGroup].rowFlex -= d
		l.groupRefs[neighborIdx].rowFlex += d
	}

	// Normalize: ensure no group has rowFlex < 1.
	for _, gr := range l.groupRefs {
		if gr.rowFlex < 1 {
			gr.rowFlex = 1
		}
	}
}

// equalizeGroups resets all group rowFlex weights in this lane to 10.
func (l *LaneActor) equalizeGroups() {
	for _, gr := range l.groupRefs {
		gr.rowFlex = 10
	}
}

// ---------------------------------------------------------------------------
// Active pane sync
// ---------------------------------------------------------------------------

// focusPaneByID makes the pane group containing paneID the active group within
// the lane. It is a no-op if the pane is not in this lane. This keeps the lane
// snapshot's ActivePaneID in sync when focus is restored to a pane that lives
// in a non-active group (e.g. after creating a new pane group in the lane).
func (l *LaneActor) focusPaneByID(paneID string) {
	groupID, ok := l.paneToGroup[paneID]
	if !ok {
		return
	}
	for i, gr := range l.groupRefs {
		if gr.id == groupID {
			l.activeGroup = i
			gr.activePaneID = paneID
			// Tell the GROUP too: it owns the stack's active index (what is
			// expanded, where input goes). Without this, focusing a background
			// stacked pane by id updated tab/lane bookkeeping only — the group
			// kept the old pane expanded while the workspace routed input to
			// the newly-active but still-collapsed pane (focus split-brain:
			// vanished highlight, keystrokes into an invisible pane).
			if subject, ok := l.groupSubjects[gr.id]; ok {
				_ = l.pub.Send(subject, &msg.MsgPaneGroupFocusPaneByID{PaneID: paneID})
			}
			return
		}
	}
}

// moveActiveGroup reorders the active pane group up or down within the lane's
// vertical column (split-down layout), keeping it active. DirUp moves it toward
// the top (index 0); DirDown moves it toward the bottom. No-op at the top/bottom
// edge or when the lane has a single group. This lets ctrl+y reorder single-pane
// groups stacked with ctrl+p v, complementing intra-stack reorder.
func (l *LaneActor) moveActiveGroup(dir msg.Direction) {
	if len(l.groupRefs) <= 1 {
		return
	}
	i := l.activeGroup
	switch dir {
	case msg.DirUp:
		if i <= 0 {
			return
		}
		l.groupRefs[i-1], l.groupRefs[i] = l.groupRefs[i], l.groupRefs[i-1]
		l.activeGroup = i - 1
	case msg.DirDown:
		if i >= len(l.groupRefs)-1 {
			return
		}
		l.groupRefs[i+1], l.groupRefs[i] = l.groupRefs[i], l.groupRefs[i+1]
		l.activeGroup = i + 1
	}
}

func (l *LaneActor) updateActivePaneFromGroup() {
	if len(l.groupRefs) == 0 || l.activeGroup < 0 || l.activeGroup >= len(l.groupRefs) {
		return
	}
	gr := l.groupRefs[l.activeGroup]
	reply, err := l.pub.Request(
		msg.T("pane-group", gr.id, "inbox"),
		&msg.MsgGetPaneGroupActivePane{},
		time.Second,
	)
	if err != nil {
		return
	}
	if r, ok := reply.(*msg.MsgPaneGroupActivePaneReply); ok {
		gr.activePaneID = r.PaneID
		gr.paneCount = r.PaneCount
	}
}

// ---------------------------------------------------------------------------
// Snapshot
// ---------------------------------------------------------------------------

func (l *LaneActor) collectSnapshot(layoutOnly, noHistories bool) domain.LaneSnapshot {
	snap := domain.LaneSnapshot{
		ID:   l.id,
		Flex: l.flex,
		Name: l.name,
	}
	if len(l.groupRefs) > 0 && l.activeGroup >= 0 && l.activeGroup < len(l.groupRefs) {
		snap.ActivePaneID = l.groupRefs[l.activeGroup].activePaneID
	}

	// Fetch the pane groups concurrently (one round-trip instead of one per
	// group). Each goroutine writes a distinct results[i]; the parent appends in
	// order after all complete.
	results := make([]domain.PaneGroupSnapshot, len(l.groupRefs))
	fetch := func(i int, id, activePaneID string, rowFlex int) {
		reply, err := l.pub.Request(
			msg.T("pane-group", id, "snapshot"),
			&msg.MsgGetPaneGroupSnapshot{LayoutOnly: layoutOnly, NoHistories: noHistories},
			2*time.Second,
		)
		if err != nil {
			// Timeout/error stub: PRESERVE ActivePaneID (the lane's own
			// bookkeeping). Without it a degraded snapshot arrived with no
			// active pane for the group, and snapshot-driven clients fell
			// back to expanding the group's first pane — a focus/deck jump
			// under load (e.g. a claude redraw storm congesting the mailbox).
			results[i] = domain.PaneGroupSnapshot{
				ID:           id,
				ActivePaneID: activePaneID,
				Panes: []domain.PaneSnapshot{{
					ID:     activePaneID,
					Title:  "unreachable",
					Status: "unreachable",
					Output: fmt.Sprintf("group snapshot error: %v", err),
				}},
			}
			return
		}
		gReply, ok := reply.(*msg.MsgPaneGroupSnapshotReply)
		if !ok {
			results[i] = domain.PaneGroupSnapshot{
				ID:           id,
				ActivePaneID: activePaneID,
				Panes: []domain.PaneSnapshot{{
					ID:     activePaneID,
					Title:  "unreachable",
					Status: "unreachable",
					Output: "unexpected reply type",
				}},
			}
			return
		}
		gs := gReply.Snapshot
		gs.RowFlex = rowFlex
		results[i] = gs
	}
	// Concurrent only with more than one group (cascade is depth-sequential).
	if len(l.groupRefs) > 1 {
		var wg sync.WaitGroup
		for i, gr := range l.groupRefs {
			wg.Add(1)
			go func(i int, id, activePaneID string, rowFlex int) {
				defer wg.Done()
				fetch(i, id, activePaneID, rowFlex)
			}(i, gr.id, gr.activePaneID, gr.rowFlex)
		}
		wg.Wait()
	} else {
		for i, gr := range l.groupRefs {
			fetch(i, gr.id, gr.activePaneID, gr.rowFlex)
		}
	}
	snap.PaneGroups = append(snap.PaneGroups, results...)

	return snap
}

// ---------------------------------------------------------------------------
// Cross-actor access methods (used by TabActor for shutdown and ## commands)
// ---------------------------------------------------------------------------

// FlushAllPanes forces KV persistence for all pane actors in all groups.
func (l *LaneActor) FlushAllPanes() {
	for _, ga := range l.groupActors {
		ga.FlushAllPanes()
	}
}

// SetName sets the user-assigned name of this lane. Empty means the lane
// inherits the tab's pipeline name for display.
func (l *LaneActor) SetName(name string) {
	l.name = name
}

// Name returns the user-assigned lane name (empty if unset).
func (l *LaneActor) Name() string {
	return l.name
}

// PaneIDs returns the IDs of all panes across all groups in this lane (in order).
func (l *LaneActor) PaneIDs() []string {
	var ids []string
	for _, gr := range l.groupRefs {
		if ga, ok := l.groupActors[gr.id]; ok {
			ids = append(ids, ga.PaneIDs()...)
		}
	}
	return ids
}

// ActivePaneID returns the ID of the currently active pane, or "".
func (l *LaneActor) ActivePaneID() string {
	if len(l.groupRefs) == 0 || l.activeGroup < 0 || l.activeGroup >= len(l.groupRefs) {
		return ""
	}
	return l.groupRefs[l.activeGroup].activePaneID
}

// PaneHistory returns the shell or prompt history for a specific pane.
func (l *LaneActor) PaneHistory(paneID, mode string) []string {
	groupID, ok := l.paneToGroup[paneID]
	if !ok {
		return nil
	}
	ga, ok := l.groupActors[groupID]
	if !ok {
		return nil
	}
	return ga.PaneHistory(paneID, mode)
}

// PaneSnapshot returns the current snapshot for a specific pane (no NATS round-trip).
func (l *LaneActor) PaneSnapshot(paneID string) *domain.PaneSnapshot {
	groupID, ok := l.paneToGroup[paneID]
	if !ok {
		return nil
	}
	ga, ok := l.groupActors[groupID]
	if !ok {
		return nil
	}
	return ga.PaneSnapshot(paneID)
}

// PanePrivateOutput returns the dedicated private (raw) output buffer for a pane.
func (l *LaneActor) PanePrivateOutput(paneID string) string {
	groupID, ok := l.paneToGroup[paneID]
	if !ok {
		return ""
	}
	ga, ok := l.groupActors[groupID]
	if !ok {
		return ""
	}
	return ga.PanePrivateOutput(paneID)
}

// PaneChatOutput returns the chat output buffer for a pane.
// Cross-actor read — informational only.
func (l *LaneActor) PaneChatOutput(paneID string) string {
	groupID, ok := l.paneToGroup[paneID]
	if !ok {
		return ""
	}
	ga, ok := l.groupActors[groupID]
	if !ok {
		return ""
	}
	return ga.PaneChatOutput(paneID)
}

// PaneHoppedInfo returns the hopped content info for a pane.
// Cross-actor read — informational only.
func (l *LaneActor) PaneHoppedInfo(paneID string) *HoppedInfo {
	groupID, ok := l.paneToGroup[paneID]
	if !ok {
		return nil
	}
	ga, ok := l.groupActors[groupID]
	if !ok {
		return nil
	}
	return ga.PaneHoppedInfo(paneID)
}

// AppendPaneSystemOutput appends text to a pane's display-only output buffer.
func (l *LaneActor) AppendPaneSystemOutput(paneID, text string) {
	groupID, ok := l.paneToGroup[paneID]
	if !ok {
		return
	}
	ga, ok := l.groupActors[groupID]
	if !ok {
		return
	}
	ga.AppendPaneSystemOutput(paneID, text)
}

// AppendPaneRyshOutput appends text to a pane's rysh-mode output buffer.
func (l *LaneActor) AppendPaneRyshOutput(paneID, text string) {
	groupID, ok := l.paneToGroup[paneID]
	if !ok {
		return
	}
	ga, ok := l.groupActors[groupID]
	if !ok {
		return
	}
	ga.AppendPaneRyshOutput(paneID, text)
}

// DeleteAllPaneKV removes all pane entries from the KV store.
func (l *LaneActor) DeleteAllPaneKV() {
	for _, ga := range l.groupActors {
		ga.DeleteAllPaneKV()
	}
}

// ContainsPane returns true if this lane contains a pane with the given ID.
func (l *LaneActor) ContainsPane(paneID string) bool {
	_, ok := l.paneToGroup[paneID]
	return ok
}

// HasGivenName checks if any pane in this lane already uses the given name.
// excludePaneID allows excluding a specific pane (the one being renamed).
func (l *LaneActor) HasGivenName(name, excludePaneID string) bool {
	for _, ga := range l.groupActors {
		if ga.HasGivenName(name, excludePaneID) {
			return true
		}
	}
	return false
}

// GroupCount returns the number of pane groups in this lane.
func (l *LaneActor) GroupCount() int {
	return len(l.groupRefs)
}

// ActiveGroupPaneCount returns the number of panes in the active group.
func (l *LaneActor) ActiveGroupPaneCount() int {
	if len(l.groupRefs) == 0 || l.activeGroup < 0 || l.activeGroup >= len(l.groupRefs) {
		return 0
	}
	return l.groupRefs[l.activeGroup].paneCount
}

// ActiveGroupIndex returns the current active group index.
func (l *LaneActor) ActiveGroupIndex() int {
	return l.activeGroup
}

// SetActiveGroup sets the active group index.
func (l *LaneActor) SetActiveGroup(idx int) {
	if idx >= 0 && idx < len(l.groupRefs) {
		l.activeGroup = idx
	}
}

// ---------------------------------------------------------------------------
// KV persistence
// ---------------------------------------------------------------------------

type laneKV struct {
	ID           string        `json:"id"`
	Flex         int           `json:"flex"`
	Name         string        `json:"name,omitempty"`
	PaneGroups   []paneGroupKV `json:"pane_groups"`
	GroupRowFlex []int         `json:"group_row_flex,omitempty"`
	ActiveGroup  int           `json:"active_group"`
}

// toKVViaActors serialises the lane for persistence, asking each child pane
// group to serialise ITSELF on its own goroutine rather than reading the group
// structs directly. Must be called from inside this lane's Receive (it needs a
// live actor.Context). See kv_cascade.go for why.
//
// Lane-owned fields (id/flex/name/activeGroup, and the per-group rowFlex, which
// lives on this actor's groupRefs) are read directly — they belong to this
// goroutine. Only the group documents come from the child actors.
func (l *LaneActor) toKVViaActors(ctx actor.Context) laneKV {
	kv := laneKV{
		ID:           l.id,
		Flex:         l.flex,
		Name:         l.name,
		ActiveGroup:  l.activeGroup,
		PaneGroups:   make([]paneGroupKV, len(l.groupRefs)),
		GroupRowFlex: make([]int, len(l.groupRefs)),
	}
	for i, gr := range l.groupRefs {
		kv.GroupRowFlex[i] = gr.rowFlex // lane-owned: safe to read here
		if reply, ok := requestKV[*paneGroupKVReply](ctx, l.groupPIDs[gr.id], &paneGroupKVRequest{}); ok {
			kv.PaneGroups[i] = reply.kv
			continue
		}
		// Fallback: the group actor is gone or did not answer in time. Prefer a
		// direct read over dropping the group from the document entirely.
		if ga, ok := l.groupActors[gr.id]; ok {
			kv.PaneGroups[i] = ga.ToKV()
		} else {
			kv.PaneGroups[i] = paneGroupKV{ID: gr.id}
		}
	}
	return kv
}

// ToKV serialises the lane state for JetStream KV persistence.
//
// Deprecated for cross-actor use: this reads child pane-group structs directly
// and therefore races with those actors. It is retained only as the fallback
// path in toKVViaActors / TabActor.toKVViaActors (and for tests). Callers on
// another goroutine must use the cascade instead.
func (l *LaneActor) ToKV() laneKV {
	kv := laneKV{
		ID:           l.id,
		Flex:         l.flex,
		Name:         l.name,
		ActiveGroup:  l.activeGroup,
		PaneGroups:   make([]paneGroupKV, len(l.groupRefs)),
		GroupRowFlex: make([]int, len(l.groupRefs)),
	}
	for i, gr := range l.groupRefs {
		kv.GroupRowFlex[i] = gr.rowFlex
		if ga, ok := l.groupActors[gr.id]; ok {
			kv.PaneGroups[i] = ga.ToKV()
		} else {
			kv.PaneGroups[i] = paneGroupKV{
				ID: gr.id,
			}
		}
	}
	return kv
}

// doRestoreFromKV restores lane and pane group state from persisted laneKV data.
// Must be called from within Receive() (i.e., with a valid actor.Context).
func (l *LaneActor) doRestoreFromKV(ctx actor.Context, kv laneKV) {
	l.id = kv.ID
	l.flex = kv.Flex
	l.name = kv.Name
	l.activeGroup = kv.ActiveGroup

	for _, gkv := range kv.PaneGroups {
		gkvCopy := gkv // capture for closure
		ga := NewPaneGroupActorFromKV(l.tabID, l.id, l.cfg, l.pub, l.nc, l.agSetup, l.kvStore, l.secrets, gkvCopy)
		groupProps := actor.PropsFromProducer(func() actor.Actor { return ga })
		pid := ctx.Spawn(groupProps)

		// Determine rowFlex from persisted data, defaulting to 10.
		rowFlex := 10
		idx := len(l.groupRefs)
		if idx < len(kv.GroupRowFlex) && kv.GroupRowFlex[idx] > 0 {
			rowFlex = kv.GroupRowFlex[idx]
		}

		ref := &laneGroupRef{
			id:        gkvCopy.ID,
			rowFlex:   rowFlex,
			paneCount: len(gkvCopy.PaneRefs),
		}
		// Set activePaneID from KV data.
		if gkvCopy.ActivePane >= 0 && gkvCopy.ActivePane < len(gkvCopy.PaneRefs) {
			ref.activePaneID = gkvCopy.PaneRefs[gkvCopy.ActivePane].ID
		}

		l.groupRefs = append(l.groupRefs, ref)
		l.groupPIDs[gkvCopy.ID] = pid
		l.groupActors[gkvCopy.ID] = ga
		l.groupSubjects[gkvCopy.ID] = msg.T("pane-group", gkvCopy.ID, "inbox")

		// Update pane-to-group mapping.
		for _, pk := range gkvCopy.PaneRefs {
			l.paneToGroup[pk.ID] = gkvCopy.ID
		}
	}

	// Normalize rowFlex: if total rowFlex is below 10*numGroups (e.g. old sessions
	// or sessions where rowFlex was never set), scale values up so 10% resize works.
	if len(l.groupRefs) > 1 {
		totalRowFlex := 0
		for _, gr := range l.groupRefs {
			totalRowFlex += gr.rowFlex
		}
		if totalRowFlex > 0 && totalRowFlex < 10*len(l.groupRefs) {
			scale := (10*len(l.groupRefs) + totalRowFlex - 1) / totalRowFlex
			if scale > 1 {
				for _, gr := range l.groupRefs {
					gr.rowFlex *= scale
				}
			}
		}
	}
}
