// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"github.com/asynkron/protoactor-go/actor"
	petname "github.com/dustinkirkland/golang-petname"
	"github.com/google/uuid"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// ---------------------------------------------------------------------------
// Lane lifecycle
// ---------------------------------------------------------------------------

func (t *TabActor) createLane(ctx actor.Context, title string) {
	laneID := uuid.NewString()
	paneID := uuid.NewString()
	if title == "" {
		title = petname.Generate(2, "-")
	}
	// A new lane joins at the AVERAGE of its siblings' weights and does not
	// resize them. Width is proportional to the sum of all lane flexes, so this
	// keeps an equal layout equal (average of 10s is 10) while making the new
	// lane blend into a deliberately uneven one instead of dominating it.
	//
	// A fixed default was wrong in both directions. It ignored what the siblings
	// were actually at: dropping a flex-10 lane next to lanes sitting at 5/3/2
	// handed the newcomer nearly half the screen and crushed the rest. (The
	// original bug was the opposite — halving the ACTIVE lane, which drifted an
	// equal layout to 5,3,2,1,1… as each create halved an ever-smaller lane.)
	flex := defaultFlex
	if n := len(t.laneRefs); n > 0 {
		total := 0
		for _, lr := range t.laneRefs {
			total += lr.flex
		}
		if flex = total / n; flex < 1 {
			flex = 1
		}
	}

	la := NewLaneActor(laneID, t.id, flex, paneID, title, t.cfg, t.pub, t.nc, t.agSetup, t.kvStore, t.secrets)
	laneProps := actor.PropsFromProducer(func() actor.Actor { return la })
	pid := ctx.Spawn(laneProps)

	t.lanePIDs[laneID] = pid
	t.laneActors[laneID] = la
	t.laneSubjects[laneID] = msg.T("lane", laneID, "inbox")
	t.paneToLane[paneID] = laneID

	ref := &laneRef{
		id:           laneID,
		flex:         flex,
		activePaneID: paneID,
		paneCount:    1,
	}

	// Insert after active lane (new lane to the right).
	insertAt := len(t.laneRefs)
	if len(t.laneRefs) > 0 && t.activeLane >= 0 && t.activeLane < len(t.laneRefs) {
		insertAt = t.activeLane + 1
	}
	t.laneRefs = append(t.laneRefs, nil)
	copy(t.laneRefs[insertAt+1:], t.laneRefs[insertAt:])
	t.laneRefs[insertAt] = ref
	t.activeLane = insertAt

	t.normalizeFlex()
}

// createLaneWithPanes creates a lane seeded with one pane group per title
// (each group holding a single pane). It is used by the grid seed path during
// *actor.Started so the whole lane is built synchronously without relying on
// NATS messages to the not-yet-ready lane bridge. Lanes are appended in order
// with equal flex; the LaneActor seeds the extra pane groups inline.
func (t *TabActor) createLaneWithPanes(ctx actor.Context, paneTitles []string) {
	if len(paneTitles) == 0 {
		return
	}
	laneID := uuid.NewString()
	paneIDs := make([]string, len(paneTitles))
	for i := range paneIDs {
		paneIDs[i] = uuid.NewString()
	}

	la := NewLaneActor(laneID, t.id, 10, paneIDs[0], paneTitles[0], t.cfg, t.pub, t.nc, t.agSetup, t.kvStore, t.secrets)
	for i := 1; i < len(paneIDs); i++ {
		la.initialExtraPanes = append(la.initialExtraPanes, panePair{id: paneIDs[i], title: paneTitles[i]})
	}
	pid := ctx.Spawn(actor.PropsFromProducer(func() actor.Actor { return la }))

	t.lanePIDs[laneID] = pid
	t.laneActors[laneID] = la
	t.laneSubjects[laneID] = msg.T("lane", laneID, "inbox")
	for _, paneID := range paneIDs {
		t.paneToLane[paneID] = laneID
	}

	ref := &laneRef{
		id:           laneID,
		flex:         10,
		activePaneID: paneIDs[0],
		paneCount:    len(paneIDs),
	}
	t.laneRefs = append(t.laneRefs, ref)
	t.activeLane = len(t.laneRefs) - 1
}

// createGridHere appends a grid of lanes to this already-running tab: one lane
// per entry in laneTitles, each holding one pane group (single pane) per inner
// title. Existing lanes are preserved; all lanes are equalized afterward so the
// tab reads as a clean grid. Used by `##new grid <lanes>x<panes> --here`.
func (t *TabActor) createGridHere(ctx actor.Context, laneTitles [][]string) {
	if len(laneTitles) == 0 {
		return
	}
	prevActive := t.activeLane
	for _, paneTitles := range laneTitles {
		t.createLaneWithPanes(ctx, paneTitles)
	}
	t.equalizeLanes()
	// Keep focus on the originating lane; the workspace also restores focus to
	// the issuing pane after this message is processed.
	if prevActive >= 0 && prevActive < len(t.laneRefs) {
		t.activeLane = prevActive
	} else {
		t.activeLane = 0
	}
	t.normalizeFlex()
}

// createPaneInLane creates a new pane group in the active lane.
func (t *TabActor) createPaneInLane(ctx actor.Context, title string) {
	if len(t.laneRefs) == 0 || t.activeLane < 0 || t.activeLane >= len(t.laneRefs) {
		// No lanes yet; create a new lane instead.
		t.createLane(ctx, title)
		return
	}

	paneID := uuid.NewString()
	if title == "" {
		title = petname.Generate(2, "-")
	}

	lr := t.laneRefs[t.activeLane]
	_ = t.pub.Send(t.laneSubjects[lr.id], &msg.MsgLaneCreatePaneGroup{
		PaneID: paneID,
		Title:  title,
	})
	t.paneToLane[paneID] = lr.id
	lr.paneCount++
	lr.activePaneID = paneID
}

// closePaneInLane closes the active pane.  It handles three cases:
//  1. The active group has multiple stacked panes → delete just the top pane.
//  2. The active group has one pane and the lane has multiple groups → close the group.
//  3. The active group has one pane and the lane has one group → close the entire lane.
func (t *TabActor) closePaneInLane(ctx actor.Context) {
	if len(t.laneRefs) == 0 || t.activeLane < 0 || t.activeLane >= len(t.laneRefs) {
		return
	}

	lr := t.laneRefs[t.activeLane]
	la, ok := t.laneActors[lr.id]
	if !ok {
		return
	}

	activeGroupPanes := la.ActiveGroupPaneCount()
	groupCount := la.GroupCount()

	if activeGroupPanes > 1 {
		// Multiple panes stacked in the active group: close only the top pane.
		_ = t.pub.Send(t.laneSubjects[lr.id], &msg.MsgLaneCloseActivePane{})
		t.updateActivePaneFromLane()
	} else if groupCount <= 1 {
		// Only one group with one pane in the lane: close the entire lane.
		t.closeActiveLane(ctx)
	} else {
		// Multiple groups, active group has one pane: close the group.
		_ = t.pub.Send(t.laneSubjects[lr.id], &msg.MsgLaneClosePaneGroup{})
		t.updateActivePaneFromLane()
	}
}

func (t *TabActor) closeActiveLane(ctx actor.Context) {
	if len(t.laneRefs) <= 1 || t.activeLane < 0 || t.activeLane >= len(t.laneRefs) {
		return
	}

	idx := t.activeLane
	removed := t.laneRefs[idx]

	// Stop the lane actor (which stops all its children).
	if pid, ok := t.lanePIDs[removed.id]; ok {
		ctx.Stop(pid)
	}

	// Delete KV entries for all panes in the lane.
	if la, ok := t.laneActors[removed.id]; ok {
		la.DeleteAllPaneKV()
		for _, pID := range la.PaneIDs() {
			delete(t.paneToLane, pID)
		}
	}

	// Do NOT redistribute the removed lane's flex onto a single neighbor. Width
	// is proportional to the sum of all lane flexes, so simply dropping the lane
	// hands its space back to every survivor in proportion to their existing
	// weight — keeping the layout balanced. (The old code added the whole removed
	// weight to one neighbor, doubling it relative to the others: Family-A drift.)
	delete(t.lanePIDs, removed.id)
	delete(t.laneActors, removed.id)
	delete(t.laneSubjects, removed.id)

	t.laneRefs = append(t.laneRefs[:idx], t.laneRefs[idx+1:]...)
	if idx >= len(t.laneRefs) {
		t.activeLane = len(t.laneRefs) - 1
	} else {
		t.activeLane = idx
	}

	t.updateActivePaneFromLane()
	t.normalizeFlex()
}

// deleteLaneByID deletes a specific lane identified by ID.
func (t *TabActor) deleteLaneByID(ctx actor.Context, laneID string) {
	idx := -1
	for i, lr := range t.laneRefs {
		if lr.id == laneID {
			idx = i
			break
		}
	}
	if idx < 0 || len(t.laneRefs) <= 1 {
		return
	}

	removed := t.laneRefs[idx]

	// Stop the lane actor (which stops all its children).
	if pid, ok := t.lanePIDs[removed.id]; ok {
		ctx.Stop(pid)
	}

	// Delete KV entries for all panes in the lane.
	if la, ok := t.laneActors[removed.id]; ok {
		la.DeleteAllPaneKV()
		for _, pID := range la.PaneIDs() {
			delete(t.paneToLane, pID)
		}
	}

	// Drop the lane without dumping its flex onto one neighbor; the renderer
	// hands the freed width back to all survivors proportionally. (See
	// closeActiveLane — Family-A of the layout-drift bug.)
	delete(t.lanePIDs, removed.id)
	delete(t.laneActors, removed.id)
	delete(t.laneSubjects, removed.id)

	t.laneRefs = append(t.laneRefs[:idx], t.laneRefs[idx+1:]...)
	if t.activeLane >= len(t.laneRefs) {
		t.activeLane = len(t.laneRefs) - 1
	}

	t.updateActivePaneFromLane()
	t.normalizeFlex()
}

// createPaneGroupInLane creates a new pane group in a specific lane by ID.
// paneID, when non-empty, pre-assigns the initial pane's ID (see
// MsgTabCreatePaneGroupInLane); paneType marks a special pane variant
// ("replay" panes never start a shell).
func (t *TabActor) createPaneGroupInLane(ctx actor.Context, laneID, title, groupID, workingDir, paneID, paneType string, meta map[string]string) {
	subject, ok := t.laneSubjects[laneID]
	if !ok {
		return
	}
	if paneID == "" {
		paneID = uuid.NewString()
	}
	if title == "" {
		title = petname.Generate(2, "-")
	}
	_ = t.pub.Send(subject, &msg.MsgLaneCreatePaneGroup{
		PaneID: paneID, Title: title, GroupID: groupID, WorkingDir: workingDir, PaneType: paneType,
		Meta: meta,
	})
	t.paneToLane[paneID] = laneID

	// Update the lane ref's pane count.
	for _, lr := range t.laneRefs {
		if lr.id == laneID {
			lr.paneCount++
			lr.activePaneID = paneID
			break
		}
	}
}

// createGroupsInLane appends one pane group (a single pane) per title to the
// given lane, stacking them vertically, then equalizes the lane's group heights
// so they share the column evenly. Used by `##new grid <n>`.
func (t *TabActor) createGroupsInLane(laneID string, titles []string) {
	subject, ok := t.laneSubjects[laneID]
	if !ok || len(titles) == 0 {
		return
	}
	var lastPaneID string
	for _, title := range titles {
		paneID := uuid.NewString()
		if title == "" {
			title = petname.Generate(2, "-")
		}
		_ = t.pub.Send(subject, &msg.MsgLaneCreatePaneGroup{PaneID: paneID, Title: title})
		t.paneToLane[paneID] = laneID
		lastPaneID = paneID
	}
	for _, lr := range t.laneRefs {
		if lr.id == laneID {
			lr.paneCount += len(titles)
			lr.activePaneID = lastPaneID
			break
		}
	}
	// Even out the vertical heights now that all groups exist. This is published
	// to the same lane subject after the creates, so it is processed last.
	_ = t.pub.Send(subject, &msg.MsgLaneEqualizeGroups{})
}

// createStackedPaneInLane creates a stacked pane in a specific lane's pane group.
func (t *TabActor) createStackedPaneInLane(laneID, paneGroupID, title string) {
	paneID := uuid.NewString()
	if title == "" {
		title = petname.Generate(2, "-")
	}

	if paneGroupID != "" {
		// Send directly to the lane to create in a specific group.
		subject, ok := t.laneSubjects[laneID]
		if !ok {
			return
		}
		_ = t.pub.Send(subject, &msg.MsgLaneCreateStackedPaneInGroup{
			PaneGroupID: paneGroupID,
			PaneID:      paneID,
			Title:       title,
		})
	} else {
		// Create in active group of the lane.
		subject, ok := t.laneSubjects[laneID]
		if !ok {
			return
		}
		_ = t.pub.Send(subject, &msg.MsgLaneCreateStackedPane{
			PaneID: paneID,
			Title:  title,
		})
	}
	t.paneToLane[paneID] = laneID
	t.updateActivePaneFromLane()
}
