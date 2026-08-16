// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

// Positional refs for `##lane info <ref>` and `##panegroup info <ref>`.
//
// F-55: both arms used to take only the ambient paneID and drop args on the
// floor, so `##lane info 3` reported lane 1 — in full, confidently, exit 0 —
// and `##pg info <anything>` reported the caller's own stack. That is F-16
// (`##pane info <id>`, fixed 2026-08-09 in c80c27e) unfixed in two more
// families: the fix went to the instance, not the class.
//
// The three rules from F-16 hold here unchanged, and each is a way the old
// behaviour was wrong rather than merely incomplete:
//
//  1. REFUSE, DO NOT GUESS. An unresolvable ref is an error naming the ref.
//     Falling back to the ambient lane is what made the defect invisible.
//  2. DO NOT FOCUS. A named lane or stack may live in another tab, so
//     resolution searches other tabs' snapshots and never calls focusPaneByID
//     — `info` is a read, and a read must not move the human's cursor.
//  3. NO ARGUMENT KEEPS WORKING. The bare forms still report the caller's own
//     lane/stack. That is long-standing behaviour people rely on.
//
// What resolves, and why the scoping differs by ref shape: an id is
// session-unique, so it is searched across every tab. An index or a lane name
// is unique only within one container — a lane index within a tab, a stack
// index within a lane — so those resolve against the CALLER's tab/lane only,
// and never leak into a sweep where they would silently match a different
// container's slot 3. This is the same reasoning `##move` documents for why
// `--tab` applies to `to-lane` but not to pane or stack ids
// (designs/033-live-layout-moves.md §1).

// infoRef returns the positional ref of a `##<family> info <ref>` invocation,
// or "" for the bare form. args is the whole subcommand slice, so args[0] is
// "info" itself.
func infoRef(args []string) string {
	if len(args) < 2 {
		return ""
	}
	return strings.TrimSpace(args[1])
}

// laneInfoTarget is a resolved `##lane info` subject: the lane and enough of
// its surroundings to describe where it sits.
type laneInfoTarget struct {
	tab     *tabInfo
	tabSnap *domain.TabSnapshot
	lane    *domain.LaneSnapshot
	laneIdx int
}

// groupInfoTarget is laneInfoTarget for a stack.
type groupInfoTarget struct {
	tab      *tabInfo
	lane     *domain.LaneSnapshot
	group    *domain.PaneGroupSnapshot
	laneIdx  int
	groupIdx int
}

// callerTab is the tab holding the calling pane, falling back to the active tab
// when the pane cannot be placed. Preferring the caller's own tab is what makes
// a bare `##lane info` answer about the agent that asked rather than about
// wherever the human's focus has drifted (the ambient-attribution hazard of
// design 025 §4).
func (w *WorkspaceActor) callerTab(paneID string) *tabInfo {
	if paneID != "" {
		if t := w.findPaneTab(paneID); t != nil {
			return t
		}
	}
	return w.currentTab()
}

// resolveLaneInfoTarget resolves the subject of `##lane info [<ref>]`.
func (w *WorkspaceActor) resolveLaneInfoTarget(paneID, ref string) (*laneInfoTarget, error) {
	tab := w.callerTab(paneID)
	if tab == nil {
		return nil, fmt.Errorf("no active tab")
	}
	tabSnap := w.queryTabSnapshot(tab.id)
	if tabSnap == nil {
		return nil, fmt.Errorf("could not fetch tab snapshot")
	}

	if ref == "" {
		if paneID == "" {
			return nil, fmt.Errorf("no active pane")
		}
		site, ok := domain.LocatePaneInTab(tabSnap, paneID)
		if !ok || site.Lane == nil {
			return nil, fmt.Errorf("pane %s not found in any lane", paneID)
		}
		return &laneInfoTarget{tab: tab, tabSnap: tabSnap, lane: site.Lane, laneIdx: site.LaneIndex}, nil
	}

	// The caller's own tab first: this is where an index or a name means what
	// the user just read off `##lane list`.
	if lane := domain.ResolveLane(tabSnap, ref); lane != nil {
		return &laneInfoTarget{tab: tab, tabSnap: tabSnap, lane: lane, laneIdx: laneIndexIn(tabSnap, lane.ID)}, nil
	}

	// Then every other tab, by id or unique id prefix only.
	t, snap, lane, err := w.findLaneAcrossTabs(ref, tab.id)
	if err != nil {
		return nil, err
	}
	if lane == nil {
		return nil, fmt.Errorf("lane not found: %q (see ##lane list)", ref)
	}
	return &laneInfoTarget{tab: t, tabSnap: snap, lane: lane, laneIdx: laneIndexIn(snap, lane.ID)}, nil
}

// resolveGroupInfoTarget resolves the subject of `##panegroup info [<ref>]`.
func (w *WorkspaceActor) resolveGroupInfoTarget(paneID, ref string) (*groupInfoTarget, error) {
	tab := w.callerTab(paneID)
	if tab == nil {
		return nil, fmt.Errorf("no active tab")
	}
	tabSnap := w.queryTabSnapshot(tab.id)
	if tabSnap == nil {
		return nil, fmt.Errorf("could not fetch tab snapshot")
	}

	var callerLane *domain.LaneSnapshot
	callerLaneIdx := -1
	if paneID != "" {
		if site, ok := domain.LocatePaneInTab(tabSnap, paneID); ok {
			callerLane = site.Lane
			callerLaneIdx = site.LaneIndex
			if ref == "" {
				if site.Group == nil {
					return nil, fmt.Errorf("pane %s not found in any group", paneID)
				}
				return &groupInfoTarget{
					tab: tab, lane: site.Lane, group: site.Group,
					laneIdx: site.LaneIndex, groupIdx: site.GroupIndex,
				}, nil
			}
		}
	}
	if ref == "" {
		if paneID == "" {
			return nil, fmt.Errorf("no active pane")
		}
		return nil, fmt.Errorf("pane %s not found in any group", paneID)
	}

	// A stack index is scoped to a lane, so it resolves against the caller's
	// lane and nowhere else.
	if callerLane != nil {
		if g := domain.ResolveGroup(callerLane, ref); g != nil {
			return &groupInfoTarget{
				tab: tab, lane: callerLane, group: g,
				laneIdx: callerLaneIdx, groupIdx: groupIndexIn(callerLane, g.ID),
			}, nil
		}
	}

	t, lane, g, laneIdx, err := w.findGroupAcrossTabs(ref)
	if err != nil {
		return nil, err
	}
	if g == nil {
		return nil, fmt.Errorf("stack not found: %q (see ##panegroup list)", ref)
	}
	return &groupInfoTarget{
		tab: t, lane: lane, group: g,
		laneIdx: laneIdx, groupIdx: groupIndexIn(lane, g.ID),
	}, nil
}

// findLaneAcrossTabs matches a lane id, or an unambiguous id prefix, in every
// tab except skipTabID (already searched by the caller). Index and name are
// deliberately NOT matched here — they are per-tab, and sweeping them would
// match some other tab's slot 3.
func (w *WorkspaceActor) findLaneAcrossTabs(ref, skipTabID string) (*tabInfo, *domain.TabSnapshot, *domain.LaneSnapshot, error) {
	var (
		foundTab  *tabInfo
		foundSnap *domain.TabSnapshot
		foundLane *domain.LaneSnapshot
		prefixHit int
	)
	for _, info := range w.tabs {
		if info.id == skipTabID {
			continue
		}
		snap := w.queryTabSnapshot(info.id)
		if snap == nil {
			continue
		}
		for li := range snap.Lanes {
			lane := &snap.Lanes[li]
			if lane.ID == ref {
				return info, snap, lane, nil // exact id wins outright
			}
			if len(ref) >= domain.MinIDPrefix && strings.HasPrefix(lane.ID, ref) {
				prefixHit++
				foundTab, foundSnap, foundLane = info, snap, lane
			}
		}
	}
	if prefixHit > 1 {
		return nil, nil, nil, fmt.Errorf("lane ref %q is ambiguous — it matches %d lanes (see ##lane list)", ref, prefixHit)
	}
	return foundTab, foundSnap, foundLane, nil
}

// findGroupAcrossTabs is findLaneAcrossTabs for a stack. Nothing is skipped:
// the caller's lane was searched by index, which says nothing about ids in the
// caller's OTHER lanes.
func (w *WorkspaceActor) findGroupAcrossTabs(ref string) (*tabInfo, *domain.LaneSnapshot, *domain.PaneGroupSnapshot, int, error) {
	var (
		foundTab   *tabInfo
		foundLane  *domain.LaneSnapshot
		foundGroup *domain.PaneGroupSnapshot
		foundIdx   int
		prefixHit  int
	)
	for _, info := range w.tabs {
		snap := w.queryTabSnapshot(info.id)
		if snap == nil {
			continue
		}
		for li := range snap.Lanes {
			lane := &snap.Lanes[li]
			for gi := range lane.PaneGroups {
				g := &lane.PaneGroups[gi]
				if g.ID == ref {
					return info, lane, g, li, nil
				}
				if len(ref) >= domain.MinIDPrefix && strings.HasPrefix(g.ID, ref) {
					prefixHit++
					foundTab, foundLane, foundGroup, foundIdx = info, lane, g, li
				}
			}
		}
	}
	if prefixHit > 1 {
		return nil, nil, nil, 0, fmt.Errorf("stack ref %q is ambiguous — it matches %d stacks (see ##panegroup list)", ref, prefixHit)
	}
	return foundTab, foundLane, foundGroup, foundIdx, nil
}

func laneIndexIn(snap *domain.TabSnapshot, laneID string) int {
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

func groupIndexIn(lane *domain.LaneSnapshot, groupID string) int {
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
