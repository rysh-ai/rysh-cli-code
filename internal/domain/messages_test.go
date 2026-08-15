// SPDX-License-Identifier: Apache-2.0

package domain

import "testing"

// ---------------------------------------------------------------------------
// Why these tests exist
//
// FlatPanes() and FlatLanes() look like generic tree iterators. They are not.
// Both REWRITE the panes they hand back, deriving geometry from the enclosing
// lane and pane group:
//
//	FlatPanes:  Flex (from the lane), LaneID, RowFlex, StackPosition,
//	            StackTotal, StackCollapsed
//	FlatLanes:  LaneID, RowFlex, StackPosition, StackTotal, StackCollapsed
//	            (but NOT Flex — that difference is deliberate and pinned below)
//
// That makes them TUI *rendering* helpers, not navigation primitives. Every
// call site outside this package lives in internal/tui, and it must stay that
// way: a caller elsewhere that reached for FlatPanes() to avoid hand-rolling a
// lane/group walk would silently receive panes carrying fabricated geometry —
// data corruption, not a style problem.
//
// These tests pin the rewriting so it cannot be quietly "cleaned up" into a
// plain iterator, and so any future generic traversal helper is forced to be a
// separate function rather than a reuse of these two.
// ---------------------------------------------------------------------------

// sampleTab builds a tab with two lanes of differing Flex, groups of differing
// RowFlex, one stacked group, one single-pane group and one empty group.
//
// Panes deliberately carry a nonsense Flex/LaneID/RowFlex/stack metadata of
// their own so the tests can prove those fields are OVERWRITTEN rather than
// passed through.
func sampleTab() TabSnapshot {
	return TabSnapshot{
		ID:    "tab-1",
		Title: "sample",
		Lanes: []LaneSnapshot{
			{
				ID:   "lane-a",
				Flex: 3,
				Name: "left",
				PaneGroups: []PaneGroupSnapshot{
					{
						ID:           "grp-a1",
						RowFlex:      7,
						ActivePaneID: "p2",
						Panes: []PaneSnapshot{
							{ID: "p1", Title: "one", Flex: 99, LaneID: "bogus", RowFlex: 99, StackPosition: 42, StackTotal: 42, StackCollapsed: false},
							{ID: "p2", Title: "two", Flex: 99, LaneID: "bogus", RowFlex: 99, StackPosition: 42, StackTotal: 42, StackCollapsed: true},
							{ID: "p3", Title: "three", Flex: 99, LaneID: "bogus", RowFlex: 99, StackPosition: 42, StackTotal: 42, StackCollapsed: false},
						},
					},
					{
						ID:           "grp-a2",
						RowFlex:      2,
						ActivePaneID: "p4",
						Panes: []PaneSnapshot{
							{ID: "p4", Title: "four", Flex: 99, LaneID: "bogus", RowFlex: 99},
						},
					},
					{
						// Empty group: skipped entirely by both helpers.
						ID:      "grp-a3",
						RowFlex: 5,
						Panes:   nil,
					},
				},
			},
			{
				ID:   "lane-b",
				Flex: 1,
				Name: "right",
				PaneGroups: []PaneGroupSnapshot{
					{
						ID: "grp-b1",
						// ActivePaneID intentionally empty: NO pane matches, so
						// every pane in this group collapses.
						RowFlex: 4,
						Panes: []PaneSnapshot{
							{ID: "p5", Title: "five"},
							{ID: "p6", Title: "six"},
						},
					},
				},
			},
		},
	}
}

func TestFlatPanesRewritesGeometry(t *testing.T) {
	panes := sampleTab().FlatPanes()

	// Order is lane order, then group order, then stable pane creation order.
	// The empty group contributes nothing.
	wantIDs := []string{"p1", "p2", "p3", "p4", "p5", "p6"}
	if len(panes) != len(wantIDs) {
		t.Fatalf("FlatPanes() returned %d panes, want %d: %+v", len(panes), len(wantIDs), panes)
	}
	for i, want := range wantIDs {
		if panes[i].ID != want {
			t.Errorf("panes[%d].ID = %q, want %q", i, panes[i].ID, want)
		}
	}

	// Every field FlatPanes derives, pinned per pane. The source panes carried
	// Flex=99 / LaneID="bogus" / RowFlex=99 / StackPosition=42 / StackTotal=42,
	// so any value below that is NOT 99/bogus/42 proves the rewrite happened.
	want := []struct {
		id        string
		flex      int // from the LANE, not the pane
		laneID    string
		rowFlex   int // from the pane GROUP
		stackPos  int
		stackTot  int
		collapsed bool // true when the pane is not its group's active pane
	}{
		{"p1", 3, "lane-a", 7, 0, 3, true},  // group active is p2
		{"p2", 3, "lane-a", 7, 1, 3, false}, // active pane: expanded
		{"p3", 3, "lane-a", 7, 2, 3, true},
		{"p4", 3, "lane-a", 2, 0, 1, false}, // sole pane, and it is active
		{"p5", 1, "lane-b", 4, 0, 2, true},  // group has no active pane at all,
		{"p6", 1, "lane-b", 4, 1, 2, true},  // so BOTH collapse
	}
	for i, w := range want {
		got := panes[i]
		if got.ID != w.id {
			continue // already reported above
		}
		if got.Flex != w.flex {
			t.Errorf("%s: Flex = %d, want %d (must come from the lane, overwriting the pane's own)", w.id, got.Flex, w.flex)
		}
		if got.LaneID != w.laneID {
			t.Errorf("%s: LaneID = %q, want %q", w.id, got.LaneID, w.laneID)
		}
		if got.RowFlex != w.rowFlex {
			t.Errorf("%s: RowFlex = %d, want %d (must come from the pane group)", w.id, got.RowFlex, w.rowFlex)
		}
		if got.StackPosition != w.stackPos {
			t.Errorf("%s: StackPosition = %d, want %d", w.id, got.StackPosition, w.stackPos)
		}
		if got.StackTotal != w.stackTot {
			t.Errorf("%s: StackTotal = %d, want %d", w.id, got.StackTotal, w.stackTot)
		}
		if got.StackCollapsed != w.collapsed {
			t.Errorf("%s: StackCollapsed = %v, want %v", w.id, got.StackCollapsed, w.collapsed)
		}
	}
}

func TestFlatPanesSkipsEmptyGroups(t *testing.T) {
	ts := TabSnapshot{Lanes: []LaneSnapshot{{
		ID: "lane-a", Flex: 1,
		PaneGroups: []PaneGroupSnapshot{
			{ID: "empty-nil", Panes: nil},
			{ID: "empty-slice", Panes: []PaneSnapshot{}},
			{ID: "has-one", ActivePaneID: "p1", Panes: []PaneSnapshot{{ID: "p1"}}},
		},
	}}}

	panes := ts.FlatPanes()
	if len(panes) != 1 || panes[0].ID != "p1" {
		t.Fatalf("FlatPanes() = %+v, want exactly [p1] (empty groups contribute nothing)", panes)
	}
	// The empty groups must not even affect StackTotal of the surviving group.
	if panes[0].StackTotal != 1 {
		t.Errorf("StackTotal = %d, want 1 (counted per group, not per lane)", panes[0].StackTotal)
	}
}

func TestFlatPanesEmptyAndNil(t *testing.T) {
	if got := (TabSnapshot{}).FlatPanes(); got != nil {
		t.Errorf("zero TabSnapshot: FlatPanes() = %+v, want nil", got)
	}
	if got := (TabSnapshot{Lanes: nil}).FlatPanes(); got != nil {
		t.Errorf("nil Lanes: FlatPanes() = %+v, want nil", got)
	}
	if got := (TabSnapshot{Lanes: []LaneSnapshot{}}).FlatPanes(); got != nil {
		t.Errorf("empty Lanes: FlatPanes() = %+v, want nil", got)
	}
	// A lane with no groups, and a lane whose only group is empty, both yield
	// no panes at all (not a nil-vs-empty distinction worth relying on, but the
	// helper must not panic).
	ts := TabSnapshot{Lanes: []LaneSnapshot{
		{ID: "lane-a"},
		{ID: "lane-b", PaneGroups: []PaneGroupSnapshot{{ID: "g"}}},
	}}
	if got := ts.FlatPanes(); len(got) != 0 {
		t.Errorf("laneless/group-empty tab: FlatPanes() = %+v, want empty", got)
	}
}

// TestFlatPanesDoesNotMutateSource is the other half of the hazard: the helper
// copies each pane (flat := p) before rewriting it. Callers therefore cannot
// use the returned panes to mutate the tree — a returned pane is a detached
// copy, so this is emphatically not a way to obtain a handle on live state.
func TestFlatPanesDoesNotMutateSource(t *testing.T) {
	ts := sampleTab()
	panes := ts.FlatPanes()

	panes[0].Title = "mutated"
	panes[0].Flex = 12345

	src := ts.Lanes[0].PaneGroups[0].Panes[0]
	if src.Title != "one" {
		t.Errorf("source pane Title = %q, want %q — FlatPanes must return copies", src.Title, "one")
	}
	if src.Flex != 99 {
		t.Errorf("source pane Flex = %d, want 99 — FlatPanes must not write back to the tree", src.Flex)
	}
}

func TestFlatLanesRewritesGeometry(t *testing.T) {
	lanes := sampleTab().FlatLanes()

	if len(lanes) != 2 {
		t.Fatalf("FlatLanes() returned %d lanes, want 2", len(lanes))
	}

	// Lane-level fields are copied straight through from the LaneSnapshot.
	if lanes[0].LaneID != "lane-a" || lanes[0].Flex != 3 || lanes[0].Name != "left" {
		t.Errorf("lanes[0] = {%q, %d, %q}, want {lane-a, 3, left}", lanes[0].LaneID, lanes[0].Flex, lanes[0].Name)
	}
	if lanes[1].LaneID != "lane-b" || lanes[1].Flex != 1 || lanes[1].Name != "right" {
		t.Errorf("lanes[1] = {%q, %d, %q}, want {lane-b, 1, right}", lanes[1].LaneID, lanes[1].Flex, lanes[1].Name)
	}

	// Lane A: the empty group is skipped, so 3 + 1 panes.
	gotA := make([]string, 0, len(lanes[0].VisiblePanes))
	for _, p := range lanes[0].VisiblePanes {
		gotA = append(gotA, p.ID)
	}
	if len(gotA) != 4 || gotA[0] != "p1" || gotA[3] != "p4" {
		t.Fatalf("lane-a VisiblePanes = %v, want [p1 p2 p3 p4]", gotA)
	}

	want := []struct {
		lane      int
		idx       int
		id        string
		laneID    string
		rowFlex   int
		stackPos  int
		stackTot  int
		collapsed bool
	}{
		{0, 0, "p1", "lane-a", 7, 0, 3, true},
		{0, 1, "p2", "lane-a", 7, 1, 3, false},
		{0, 2, "p3", "lane-a", 7, 2, 3, true},
		{0, 3, "p4", "lane-a", 2, 0, 1, false},
		{1, 0, "p5", "lane-b", 4, 0, 2, true},
		{1, 1, "p6", "lane-b", 4, 1, 2, true},
	}
	for _, w := range want {
		got := lanes[w.lane].VisiblePanes[w.idx]
		if got.ID != w.id {
			t.Errorf("lanes[%d].VisiblePanes[%d].ID = %q, want %q", w.lane, w.idx, got.ID, w.id)
			continue
		}
		if got.LaneID != w.laneID {
			t.Errorf("%s: LaneID = %q, want %q", w.id, got.LaneID, w.laneID)
		}
		if got.RowFlex != w.rowFlex {
			t.Errorf("%s: RowFlex = %d, want %d", w.id, got.RowFlex, w.rowFlex)
		}
		if got.StackPosition != w.stackPos {
			t.Errorf("%s: StackPosition = %d, want %d", w.id, got.StackPosition, w.stackPos)
		}
		if got.StackTotal != w.stackTot {
			t.Errorf("%s: StackTotal = %d, want %d", w.id, got.StackTotal, w.stackTot)
		}
		if got.StackCollapsed != w.collapsed {
			t.Errorf("%s: StackCollapsed = %v, want %v", w.id, got.StackCollapsed, w.collapsed)
		}
	}
}

// TestFlatLanesLeavesPaneFlexAlone pins the one field where FlatLanes and
// FlatPanes deliberately disagree. FlatPanes flattens away the lane, so it has
// to push the lane's Flex onto each pane for width maths. FlatLanes keeps the
// lane as a container carrying its own Flex, so it leaves PaneSnapshot.Flex
// untouched — whatever the pane already held survives.
//
// Anyone "unifying" these two helpers must confront this on purpose.
func TestFlatLanesLeavesPaneFlexAlone(t *testing.T) {
	lanes := sampleTab().FlatLanes()

	for _, p := range lanes[0].VisiblePanes {
		if p.Flex != 99 {
			t.Errorf("%s: Flex = %d, want 99 — FlatLanes must NOT overwrite pane Flex from the lane (that is FlatPanes' job)", p.ID, p.Flex)
		}
	}
	// Lane B's panes never had a Flex set, so they stay zero rather than
	// inheriting the lane's Flex of 1.
	for _, p := range lanes[1].VisiblePanes {
		if p.Flex != 0 {
			t.Errorf("%s: Flex = %d, want 0 — FlatLanes leaves an unset pane Flex unset", p.ID, p.Flex)
		}
	}
}

// TestFlatLanesEmitsLanesWithoutPanes pins the other asymmetry: FlatLanes
// returns one entry per lane even when the lane renders nothing, so the TUI
// still lays out an empty column. FlatPanes, having no lane container, simply
// drops it.
func TestFlatLanesEmitsLanesWithoutPanes(t *testing.T) {
	ts := TabSnapshot{Lanes: []LaneSnapshot{
		{ID: "lane-empty", Flex: 2, Name: "nothing"},
		{ID: "lane-only-empty-groups", Flex: 1, PaneGroups: []PaneGroupSnapshot{{ID: "g1"}, {ID: "g2", Panes: []PaneSnapshot{}}}},
	}}

	lanes := ts.FlatLanes()
	if len(lanes) != 2 {
		t.Fatalf("FlatLanes() returned %d lanes, want 2 (a pane-less lane still gets an entry)", len(lanes))
	}
	for _, lr := range lanes {
		if len(lr.VisiblePanes) != 0 {
			t.Errorf("lane %q: VisiblePanes = %+v, want none", lr.LaneID, lr.VisiblePanes)
		}
	}
	if lanes[0].Flex != 2 || lanes[0].Name != "nothing" {
		t.Errorf("empty lane lost its Flex/Name: %+v", lanes[0])
	}

	if got := ts.FlatPanes(); len(got) != 0 {
		t.Errorf("FlatPanes() = %+v, want none for the same tab", got)
	}
}

func TestFlatLanesEmptyAndNil(t *testing.T) {
	if got := (TabSnapshot{}).FlatLanes(); got != nil {
		t.Errorf("zero TabSnapshot: FlatLanes() = %+v, want nil", got)
	}
	if got := (TabSnapshot{Lanes: []LaneSnapshot{}}).FlatLanes(); got != nil {
		t.Errorf("empty Lanes: FlatLanes() = %+v, want nil", got)
	}
}

// TestFlatLanesDoesNotMutateSource — same copy semantics as FlatPanes.
func TestFlatLanesDoesNotMutateSource(t *testing.T) {
	ts := sampleTab()
	lanes := ts.FlatLanes()

	lanes[0].VisiblePanes[0].Title = "mutated"

	if got := ts.Lanes[0].PaneGroups[0].Panes[0].Title; got != "one" {
		t.Errorf("source pane Title = %q, want %q — FlatLanes must return copies", got, "one")
	}
}

// TestFlatHelpersAgreeOnPaneSet is a consistency check: whatever geometry each
// helper fabricates, both must visit exactly the same panes in the same order.
// A future change that makes one skip or reorder panes relative to the other
// would desync the TUI's rendering paths against each other.
func TestFlatHelpersAgreeOnPaneSet(t *testing.T) {
	ts := sampleTab()

	var fromLanes []string
	for _, lr := range ts.FlatLanes() {
		for _, p := range lr.VisiblePanes {
			fromLanes = append(fromLanes, p.ID)
		}
	}

	var fromPanes []string
	for _, p := range ts.FlatPanes() {
		fromPanes = append(fromPanes, p.ID)
	}

	if len(fromLanes) != len(fromPanes) {
		t.Fatalf("FlatLanes visited %v, FlatPanes visited %v", fromLanes, fromPanes)
	}
	for i := range fromLanes {
		if fromLanes[i] != fromPanes[i] {
			t.Errorf("pane %d: FlatLanes has %q, FlatPanes has %q", i, fromLanes[i], fromPanes[i])
		}
	}
}
