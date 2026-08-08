package domain

import (
	"iter"
	"strconv"
)

// ---------------------------------------------------------------------------
// Tree navigation.
//
// The workspace tree is WorkspaceSnapshot > TabSnapshot > LaneSnapshot >
// PaneGroupSnapshot > PaneSnapshot, and walking it by hand is the single most
// repeated shape in this codebase — roughly 97 hand-rolled `range …Lanes`
// loops and 89 `range …PaneGroups` loops, each re-deriving the same four
// levels of nesting, and each re-inventing selector parsing with its own
// strconv.Atoi.
//
// This file is that walk, written once. It was not designed here: it is the
// navigation layer that already existed (and was already unit-tested) inside
// internal/actors/cmd_broadcast.go, serving exactly one command — ##cmd —
// while everything else hand-rolled its own. Promoting it into the domain
// package makes it reachable. domain imports no other package in this module,
// so nothing here can create an import cycle.
//
// # Pointers, deliberately
//
// Every function here takes a POINTER to its container and, where it returns a
// node, returns a POINTER into that container's slice. This is not incidental.
// The snapshot types carry value-receiver methods (FlatPanes, FlatLanes), so a
// value-taking helper would be handed a temporary copy and would return a
// pointer into that copy: writes through it are discarded, and pointer identity
// comparisons against the real tree fail. Both failure modes are silent.
//
// Keeping the parameters as pointers is what makes these safe to use for
// mutation as well as inspection. Do not "simplify" them to value parameters.
//
// # These are not FlatPanes/FlatLanes
//
// TabSnapshot.FlatPanes and TabSnapshot.FlatLanes look like traversals but are
// TUI rendering helpers: they rewrite Flex, LaneID, RowFlex, StackPosition,
// StackTotal and StackCollapsed on every pane they return, and they return
// copies. Use them for layout; use this file for navigation. See
// messages_test.go for the pinned behaviour.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Iteration
// ---------------------------------------------------------------------------

// PanesInGroup yields a pointer to every pane in the group, in stable creation
// order.
func PanesInGroup(g *PaneGroupSnapshot) iter.Seq[*PaneSnapshot] {
	return func(yield func(*PaneSnapshot) bool) {
		if g == nil {
			return
		}
		for i := range g.Panes {
			if !yield(&g.Panes[i]) {
				return
			}
		}
	}
}

// PanesInLane yields a pointer to every pane in the lane, group by group.
func PanesInLane(l *LaneSnapshot) iter.Seq[*PaneSnapshot] {
	return func(yield func(*PaneSnapshot) bool) {
		if l == nil {
			return
		}
		for gi := range l.PaneGroups {
			for pi := range l.PaneGroups[gi].Panes {
				if !yield(&l.PaneGroups[gi].Panes[pi]) {
					return
				}
			}
		}
	}
}

// PanesInTab yields a pointer to every pane in the tab, lane by lane. Unlike
// TabSnapshot.FlatPanes it does not touch the panes it yields — no geometry is
// derived, and the pointers address the real tree, so writes through them stick.
func PanesInTab(t *TabSnapshot) iter.Seq[*PaneSnapshot] {
	return func(yield func(*PaneSnapshot) bool) {
		if t == nil {
			return
		}
		for li := range t.Lanes {
			for gi := range t.Lanes[li].PaneGroups {
				for pi := range t.Lanes[li].PaneGroups[gi].Panes {
					if !yield(&t.Lanes[li].PaneGroups[gi].Panes[pi]) {
						return
					}
				}
			}
		}
	}
}

// PanesInWorkspace yields a pointer to every pane in every tab.
func PanesInWorkspace(snap *WorkspaceSnapshot) iter.Seq[*PaneSnapshot] {
	return func(yield func(*PaneSnapshot) bool) {
		if snap == nil {
			return
		}
		for ti := range snap.Tabs {
			for p := range PanesInTab(&snap.Tabs[ti]) {
				if !yield(p) {
					return
				}
			}
		}
	}
}

// PaneSite is a pane together with the lane and group that contain it, and its
// index at each level. It is what a hand-rolled triple-nested walk has in scope
// at its innermost point.
//
// All three pointers address the tree passed in, so they may be written through.
type PaneSite struct {
	Lane  *LaneSnapshot
	Group *PaneGroupSnapshot
	Pane  *PaneSnapshot

	LaneIndex  int
	GroupIndex int
	PaneIndex  int
}

// PaneSitesInTab yields every pane in the tab along with its containing lane
// and group. This is the direct replacement for the lane→group→pane triple loop.
func PaneSitesInTab(t *TabSnapshot) iter.Seq[PaneSite] {
	return func(yield func(PaneSite) bool) {
		if t == nil {
			return
		}
		for li := range t.Lanes {
			lane := &t.Lanes[li]
			for gi := range lane.PaneGroups {
				g := &lane.PaneGroups[gi]
				for pi := range g.Panes {
					site := PaneSite{
						Lane: lane, Group: g, Pane: &g.Panes[pi],
						LaneIndex: li, GroupIndex: gi, PaneIndex: pi,
					}
					if !yield(site) {
						return
					}
				}
			}
		}
	}
}

// GroupsInTab yields every pane group in the tab together with its lane.
func GroupsInTab(t *TabSnapshot) iter.Seq2[*LaneSnapshot, *PaneGroupSnapshot] {
	return func(yield func(*LaneSnapshot, *PaneGroupSnapshot) bool) {
		if t == nil {
			return
		}
		for li := range t.Lanes {
			lane := &t.Lanes[li]
			for gi := range lane.PaneGroups {
				if !yield(lane, &lane.PaneGroups[gi]) {
					return
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Containment
// ---------------------------------------------------------------------------

// GroupContainsPane reports whether the group holds a pane with this id.
func GroupContainsPane(g *PaneGroupSnapshot, paneID string) bool {
	for p := range PanesInGroup(g) {
		if p.ID == paneID {
			return true
		}
	}
	return false
}

// LaneContainsPane reports whether any group in the lane holds this pane.
func LaneContainsPane(l *LaneSnapshot, paneID string) bool {
	for p := range PanesInLane(l) {
		if p.ID == paneID {
			return true
		}
	}
	return false
}

// TabContainsPane reports whether any lane in the tab holds this pane.
func TabContainsPane(t *TabSnapshot, paneID string) bool {
	for p := range PanesInTab(t) {
		if p.ID == paneID {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Lookup
// ---------------------------------------------------------------------------

// FindPaneInTab returns a pointer to the pane with this id, or nil. The pointer
// addresses the tab passed in, so it may be written through.
func FindPaneInTab(t *TabSnapshot, paneID string) *PaneSnapshot {
	for p := range PanesInTab(t) {
		if p.ID == paneID {
			return p
		}
	}
	return nil
}

// FindPaneInWorkspace returns a pointer to the pane with this id in any tab, or nil.
func FindPaneInWorkspace(snap *WorkspaceSnapshot, paneID string) *PaneSnapshot {
	for p := range PanesInWorkspace(snap) {
		if p.ID == paneID {
			return p
		}
	}
	return nil
}

// LocatePaneInTab finds a pane and returns it with the lane and group that hold
// it. ok is false when no such pane exists, in which case the site is zero.
func LocatePaneInTab(t *TabSnapshot, paneID string) (site PaneSite, ok bool) {
	for s := range PaneSitesInTab(t) {
		if s.Pane.ID == paneID {
			return s, true
		}
	}
	return PaneSite{}, false
}

// LocatePaneInWorkspace is LocatePaneInTab across every tab; it additionally
// returns the containing tab.
func LocatePaneInWorkspace(snap *WorkspaceSnapshot, paneID string) (tab *TabSnapshot, site PaneSite, ok bool) {
	if snap == nil {
		return nil, PaneSite{}, false
	}
	for ti := range snap.Tabs {
		if s, found := LocatePaneInTab(&snap.Tabs[ti], paneID); found {
			return &snap.Tabs[ti], s, true
		}
	}
	return nil, PaneSite{}, false
}

// LaneOfPane returns the lane holding this pane, or nil.
func LaneOfPane(t *TabSnapshot, paneID string) *LaneSnapshot {
	if s, ok := LocatePaneInTab(t, paneID); ok {
		return s.Lane
	}
	return nil
}

// GroupOfPane returns the pane group holding this pane, or nil.
func GroupOfPane(t *TabSnapshot, paneID string) *PaneGroupSnapshot {
	if s, ok := LocatePaneInTab(t, paneID); ok {
		return s.Group
	}
	return nil
}

// FindTabByID returns the tab with this id, or nil.
func FindTabByID(snap *WorkspaceSnapshot, tabID string) *TabSnapshot {
	if snap == nil {
		return nil
	}
	for i := range snap.Tabs {
		if snap.Tabs[i].ID == tabID {
			return &snap.Tabs[i]
		}
	}
	return nil
}

// FindLaneByID returns the lane with this id, or nil.
func FindLaneByID(t *TabSnapshot, laneID string) *LaneSnapshot {
	if t == nil {
		return nil
	}
	for i := range t.Lanes {
		if t.Lanes[i].ID == laneID {
			return &t.Lanes[i]
		}
	}
	return nil
}

// FindGroupByID returns the pane group with this id anywhere in the tab, or nil.
func FindGroupByID(t *TabSnapshot, groupID string) *PaneGroupSnapshot {
	for _, g := range GroupsInTab(t) {
		if g.ID == groupID {
			return g
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Counting
// ---------------------------------------------------------------------------

// CountPanesInGroup counts every pane in the group.
func CountPanesInGroup(g *PaneGroupSnapshot) int {
	if g == nil {
		return 0
	}
	return len(g.Panes)
}

// CountPanesInLane counts every pane across the lane's groups.
func CountPanesInLane(l *LaneSnapshot) int {
	if l == nil {
		return 0
	}
	n := 0
	for i := range l.PaneGroups {
		n += len(l.PaneGroups[i].Panes)
	}
	return n
}

// CountPanesInTab counts every pane across the tab's lanes.
func CountPanesInTab(t *TabSnapshot) int {
	if t == nil {
		return 0
	}
	n := 0
	for i := range t.Lanes {
		n += CountPanesInLane(&t.Lanes[i])
	}
	return n
}

// CountPanesInWorkspace counts every pane in every tab.
func CountPanesInWorkspace(snap *WorkspaceSnapshot) int {
	if snap == nil {
		return 0
	}
	n := 0
	for i := range snap.Tabs {
		n += CountPanesInTab(&snap.Tabs[i])
	}
	return n
}

// CountGroupsInTab counts every pane group across the tab's lanes.
func CountGroupsInTab(t *TabSnapshot) int {
	if t == nil {
		return 0
	}
	n := 0
	for i := range t.Lanes {
		n += len(t.Lanes[i].PaneGroups)
	}
	return n
}

// ---------------------------------------------------------------------------
// Collecting
// ---------------------------------------------------------------------------

// PaneIDsInGroup returns the id of every pane in the group, in order.
func PaneIDsInGroup(g *PaneGroupSnapshot) []string {
	ids, _ := PaneIDsInGroupWhere(g, nil)
	return ids
}

// PaneIDsInLane returns the id of every pane in the lane, in order.
func PaneIDsInLane(l *LaneSnapshot) []string {
	ids, _ := PaneIDsInLaneWhere(l, nil)
	return ids
}

// PaneIDsInTab returns the id of every pane in the tab, in order.
func PaneIDsInTab(t *TabSnapshot) []string {
	ids, _ := PaneIDsInTabWhere(t, nil)
	return ids
}

// PaneIDsInWorkspace returns the id of every pane in every tab, in order.
func PaneIDsInWorkspace(snap *WorkspaceSnapshot) []string {
	ids, _ := PaneIDsInWorkspaceWhere(snap, nil)
	return ids
}

// PaneIDsInGroupWhere returns the ids of panes the keep predicate accepts, and
// the number it rejected. A nil predicate accepts everything.
//
// The predicate is how callers express an exclusion policy — "not shared
// upstream", "not in a pipeline tab" — without that policy leaking into the
// domain package, and the rejected count is what lets them report it ("skipped
// 2 shared panes") rather than silently dropping panes.
func PaneIDsInGroupWhere(g *PaneGroupSnapshot, keep func(*PaneSnapshot) bool) (ids []string, skipped int) {
	for p := range PanesInGroup(g) {
		if keep != nil && !keep(p) {
			skipped++
			continue
		}
		ids = append(ids, p.ID)
	}
	return ids, skipped
}

// PaneIDsInLaneWhere is PaneIDsInGroupWhere across the lane's groups.
func PaneIDsInLaneWhere(l *LaneSnapshot, keep func(*PaneSnapshot) bool) (ids []string, skipped int) {
	for p := range PanesInLane(l) {
		if keep != nil && !keep(p) {
			skipped++
			continue
		}
		ids = append(ids, p.ID)
	}
	return ids, skipped
}

// PaneIDsInTabWhere is PaneIDsInGroupWhere across the tab's lanes.
func PaneIDsInTabWhere(t *TabSnapshot, keep func(*PaneSnapshot) bool) (ids []string, skipped int) {
	for p := range PanesInTab(t) {
		if keep != nil && !keep(p) {
			skipped++
			continue
		}
		ids = append(ids, p.ID)
	}
	return ids, skipped
}

// PaneIDsInWorkspaceWhere is PaneIDsInGroupWhere across every tab.
func PaneIDsInWorkspaceWhere(snap *WorkspaceSnapshot, keep func(*PaneSnapshot) bool) (ids []string, skipped int) {
	for p := range PanesInWorkspace(snap) {
		if keep != nil && !keep(p) {
			skipped++
			continue
		}
		ids = append(ids, p.ID)
	}
	return ids, skipped
}

// ---------------------------------------------------------------------------
// Selector resolution
//
// A selector is the string a user types to name an entity. Every level resolves
// it the same way, in the same order:
//
//	""       → the active entity at that level, falling back to the first
//	<id>     → exact id match
//	<n>      → 1-based index, as displayed in listings
//	<name>   → title / name match, where the level has one
//
// The 1-based index is deliberate: it matches what `##rysh` listings print, so
// "lane 2" means the second lane the user was shown. Resolution is ordered so
// an id always wins over an index — a numeric id would otherwise be shadowed.
// ---------------------------------------------------------------------------

// ResolveTab resolves a tab selector against the workspace. Returns nil when
// there are no tabs, or when a non-empty selector matches nothing.
func ResolveTab(snap *WorkspaceSnapshot, arg string) *TabSnapshot {
	if snap == nil || len(snap.Tabs) == 0 {
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
	if n, err := strconv.Atoi(arg); err == nil && n >= 1 && n <= len(snap.Tabs) {
		return &snap.Tabs[n-1]
	}
	for i := range snap.Tabs {
		if snap.Tabs[i].Title == arg {
			return &snap.Tabs[i]
		}
	}
	return nil
}

// ResolveLane resolves a lane selector within a tab. The empty selector picks
// the lane holding the tab's active pane, falling back to the first lane.
func ResolveLane(tab *TabSnapshot, arg string) *LaneSnapshot {
	if tab == nil || len(tab.Lanes) == 0 {
		return nil
	}
	if arg == "" {
		if tab.ActivePaneID != "" {
			for i := range tab.Lanes {
				if LaneContainsPane(&tab.Lanes[i], tab.ActivePaneID) {
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
	if n, err := strconv.Atoi(arg); err == nil && n >= 1 && n <= len(tab.Lanes) {
		return &tab.Lanes[n-1]
	}
	for i := range tab.Lanes {
		if tab.Lanes[i].Name == arg {
			return &tab.Lanes[i]
		}
	}
	return nil
}

// ResolveGroup resolves a pane-group selector within a lane. The empty selector
// picks the group holding the lane's active pane, falling back to the first.
// Pane groups have no name, so only id and index are matched.
func ResolveGroup(lane *LaneSnapshot, arg string) *PaneGroupSnapshot {
	if lane == nil || len(lane.PaneGroups) == 0 {
		return nil
	}
	if arg == "" {
		if lane.ActivePaneID != "" {
			for i := range lane.PaneGroups {
				if GroupContainsPane(&lane.PaneGroups[i], lane.ActivePaneID) {
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
	if n, err := strconv.Atoi(arg); err == nil && n >= 1 && n <= len(lane.PaneGroups) {
		return &lane.PaneGroups[n-1]
	}
	return nil
}

// ResolvePane resolves a pane selector within a group. The empty selector picks
// the group's active pane, falling back to the first. A non-empty selector
// matches id, title or given name — but NOT an index, because a pane's position
// within its stack is not something listings number.
func ResolvePane(g *PaneGroupSnapshot, arg string) *PaneSnapshot {
	if g == nil || len(g.Panes) == 0 {
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
