package actors

import (
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// ---------------------------------------------------------------------------
// Forwarding to active lane
// ---------------------------------------------------------------------------

func (t *TabActor) forwardToActiveLane(message interface{}) {
	if len(t.laneRefs) == 0 || t.activeLane < 0 || t.activeLane >= len(t.laneRefs) {
		return
	}
	lr := t.laneRefs[t.activeLane]
	subject, ok := t.laneSubjects[lr.id]
	if !ok {
		return
	}
	_ = t.pub.Send(subject, message)
}

// forwardToAllLanes sends the same message to every lane in the tab. Used by
// the equalize-all layout command so group heights are reset in every column,
// not just the active one.
func (t *TabActor) forwardToAllLanes(message interface{}) {
	for _, lr := range t.laneRefs {
		if subject, ok := t.laneSubjects[lr.id]; ok {
			_ = t.pub.Send(subject, message)
		}
	}
}

// SetActiveLaneName sets the name of the active lane. Returns false if there
// is no active lane. Used by the WorkspaceActor for ## lane-naming commands,
// mirroring the direct-write pattern used for pipelineName.
func (t *TabActor) SetActiveLaneName(name string) bool {
	if len(t.laneRefs) == 0 || t.activeLane < 0 || t.activeLane >= len(t.laneRefs) {
		return false
	}
	lr := t.laneRefs[t.activeLane]
	la, ok := t.laneActors[lr.id]
	if !ok {
		return false
	}
	la.SetName(name)
	return true
}

// ActiveLaneName returns the effective name of the active lane (its explicit
// name, or the tab's pipeline name when unset). Returns "" if no active lane.
func (t *TabActor) ActiveLaneName() string {
	if len(t.laneRefs) == 0 || t.activeLane < 0 || t.activeLane >= len(t.laneRefs) {
		return ""
	}
	lr := t.laneRefs[t.activeLane]
	if la, ok := t.laneActors[lr.id]; ok {
		if n := la.Name(); n != "" {
			return n
		}
	}
	return t.pipelineName
}

// ---------------------------------------------------------------------------
// Focus navigation
// ---------------------------------------------------------------------------

func (t *TabActor) focusLaneLeft() {
	if len(t.laneRefs) < 2 || t.activeLane <= 0 {
		return
	}
	t.activeLane--
	t.updateActivePaneFromLane()
}

func (t *TabActor) focusLaneRight() {
	if len(t.laneRefs) < 2 || t.activeLane >= len(t.laneRefs)-1 {
		return
	}
	t.activeLane++
	t.updateActivePaneFromLane()
}

func (t *TabActor) focusLaneUp() {
	t.forwardToActiveLane(&msg.MsgLaneFocusGroup{Direction: msg.DirUp})
	t.updateActivePaneFromLane()
}

func (t *TabActor) focusLaneDown() {
	t.forwardToActiveLane(&msg.MsgLaneFocusGroup{Direction: msg.DirDown})
	t.updateActivePaneFromLane()
}

func (t *TabActor) focusPaneByID(id string) {
	laneID, ok := t.paneToLane[id]
	if !ok {
		return
	}
	for li, lr := range t.laneRefs {
		if lr.id == laneID {
			t.activeLane = li
			lr.activePaneID = id
			break
		}
	}
	// Also make the pane's group the active group within its lane, so the lane
	// snapshot reports the correct active pane (e.g. when focus is restored to a
	// pane that lives in a non-active group). Sent via the lane inbox so it is
	// ordered after any pending create message on the same subject.
	if subject, ok := t.laneSubjects[laneID]; ok {
		_ = t.pub.Send(subject, &msg.MsgLaneFocusPaneByID{ID: id})
	}
}

// focusNextPaneGlobal cycles through all pane groups across all lanes.
func (t *TabActor) focusNextPaneGlobal() {
	if len(t.laneRefs) == 0 {
		return
	}

	// Build a flat list of (laneIdx, groupCount) pairs.
	type entry struct {
		laneIdx    int
		groupCount int
	}
	var entries []entry
	totalGroups := 0
	for li, lr := range t.laneRefs {
		la, ok := t.laneActors[lr.id]
		if !ok {
			continue
		}
		gc := la.GroupCount()
		if gc > 0 {
			entries = append(entries, entry{li, gc})
			totalGroups += gc
		}
	}
	if totalGroups == 0 {
		return
	}

	// Find current position.
	curLane := t.activeLane
	curGroup := 0
	if t.activeLane >= 0 && t.activeLane < len(t.laneRefs) {
		lr := t.laneRefs[t.activeLane]
		if la, ok := t.laneActors[lr.id]; ok {
			curGroup = la.ActiveGroupIndex()
		}
	}

	// Convert to flat index and advance.
	flatIdx := 0
	for _, e := range entries {
		if e.laneIdx == curLane {
			flatIdx += curGroup
			break
		}
		flatIdx += e.groupCount
	}
	flatIdx = (flatIdx + 1) % totalGroups

	// Convert back.
	running := 0
	for _, e := range entries {
		if flatIdx < running+e.groupCount {
			t.activeLane = e.laneIdx
			la := t.laneActors[t.laneRefs[e.laneIdx].id]
			la.SetActiveGroup(flatIdx - running)
			la.updateActivePaneFromGroup()
			t.updateActivePaneFromLane()
			return
		}
		running += e.groupCount
	}
}

// focusPrevPaneGlobal cycles backward through all pane groups.
func (t *TabActor) focusPrevPaneGlobal() {
	if len(t.laneRefs) == 0 {
		return
	}

	type entry struct {
		laneIdx    int
		groupCount int
	}
	var entries []entry
	totalGroups := 0
	for li, lr := range t.laneRefs {
		la, ok := t.laneActors[lr.id]
		if !ok {
			continue
		}
		gc := la.GroupCount()
		if gc > 0 {
			entries = append(entries, entry{li, gc})
			totalGroups += gc
		}
	}
	if totalGroups == 0 {
		return
	}

	curLane := t.activeLane
	curGroup := 0
	if t.activeLane >= 0 && t.activeLane < len(t.laneRefs) {
		lr := t.laneRefs[t.activeLane]
		if la, ok := t.laneActors[lr.id]; ok {
			curGroup = la.ActiveGroupIndex()
		}
	}

	flatIdx := 0
	for _, e := range entries {
		if e.laneIdx == curLane {
			flatIdx += curGroup
			break
		}
		flatIdx += e.groupCount
	}
	flatIdx = (flatIdx - 1 + totalGroups) % totalGroups

	running := 0
	for _, e := range entries {
		if flatIdx < running+e.groupCount {
			t.activeLane = e.laneIdx
			la := t.laneActors[t.laneRefs[e.laneIdx].id]
			la.SetActiveGroup(flatIdx - running)
			la.updateActivePaneFromGroup()
			t.updateActivePaneFromLane()
			return
		}
		running += e.groupCount
	}
}

// refreshAllLanePaneCounts queries every lane for its current total pane count
// and updates the cached lr.paneCount values. This avoids stale counts when the
// workspace queries total panes to decide whether to close a tab.
func (t *TabActor) refreshAllLanePaneCounts() {
	for _, lr := range t.laneRefs {
		reply, err := t.pub.Request(
			msg.T("lane", lr.id, "inbox"),
			&msg.MsgGetLaneActivePane{},
			time.Second,
		)
		if err != nil {
			continue
		}
		if r, ok := reply.(*msg.MsgLaneActivePaneReply); ok {
			lr.activePaneID = r.PaneID
			lr.paneCount = r.PaneCount
		}
	}
}

// updateActivePaneFromLane queries the active lane via NATS request/reply.
func (t *TabActor) updateActivePaneFromLane() {
	if len(t.laneRefs) == 0 || t.activeLane < 0 || t.activeLane >= len(t.laneRefs) {
		return
	}
	lr := t.laneRefs[t.activeLane]
	reply, err := t.pub.Request(
		msg.T("lane", lr.id, "inbox"),
		&msg.MsgGetLaneActivePane{},
		time.Second,
	)
	if err != nil {
		return
	}
	if r, ok := reply.(*msg.MsgLaneActivePaneReply); ok {
		lr.activePaneID = r.PaneID
		lr.paneCount = r.PaneCount
	}
}
