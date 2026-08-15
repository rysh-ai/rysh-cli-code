// SPDX-License-Identifier: Apache-2.0

package actors

import "testing"

// Both axes follow one rule: the arrow grows or shrinks the FOCUSED lane/group,
// and the space is traded with the neighbor on the far side (right / below), so
// the focused thing's near edge — its left edge, its top edge — does not move.
// dir keeps its screen-direction encoding: dir > 0 points toward the higher
// index (right for lanes, down for groups), dir < 0 toward the lower index.

// Horizontal: → widens the FOCUSED lane and ← narrows it, whichever lane is
// focused, and the width always comes from the lane on its right — so the
// focused lane's LEFT edge does not move. Expressed in flex: the boundary the
// arrows move is the one to the right of the active lane, which means every
// lane at a LOWER index keeps the flex it had.
func TestResizeActiveLaneAnchorsLeftEdge(t *testing.T) {
	mk := func(active int) *TabActor {
		return &TabActor{
			laneRefs: []*laneRef{
				{id: "l0", flex: 10},
				{id: "l1", flex: 10},
				{id: "l2", flex: 10},
			},
			activeLane: active,
		}
	}

	// Lanes 0 and 1 both have a lane to their right, so the anchor holds for
	// both arrows: they widen/narrow, and nothing to their left is touched.
	for _, active := range []int{0, 1} {
		for _, tc := range []struct {
			name string
			dir  int
			// wider reports what the focused lane must do.
			wider bool
		}{
			{"RIGHT(+1) widens", 1, true},
			{"LEFT(-1) narrows", -1, false},
		} {
			tb := mk(active)
			before := tb.laneRefs[active].flex
			tb.resizeActiveLane(tc.dir)
			after := tb.laneRefs[active].flex
			t.Logf("active=%d %s: flexes=%d,%d,%d", active, tc.name,
				tb.laneRefs[0].flex, tb.laneRefs[1].flex, tb.laneRefs[2].flex)

			if tc.wider && after <= before {
				t.Errorf("active=%d %s: focused lane %d -> %d, wanted wider", active, tc.name, before, after)
			}
			if !tc.wider && after >= before {
				t.Errorf("active=%d %s: focused lane %d -> %d, wanted narrower", active, tc.name, before, after)
			}
			// The anchor: every lane left of the active one is untouched, so the
			// active lane's left edge sits at the same coordinate as before.
			for i := 0; i < active; i++ {
				if tb.laneRefs[i].flex != 10 {
					t.Errorf("active=%d %s: lane %d moved (%d, want 10) — the left edge did not hold",
						active, tc.name, i, tb.laneRefs[i].flex)
				}
			}
			// And the width came from the RIGHT neighbor, not somewhere else.
			if tb.laneRefs[active+1].flex == 10 {
				t.Errorf("active=%d %s: right neighbor unchanged; the width came from the wrong side", active, tc.name)
			}
		}
	}
}

// The rightmost lane is the documented exception: its right edge IS the edge of
// the tab, so it borrows from the left neighbor. → must still mean WIDER there
// — that is the whole point of the change, and it is the case that reverses
// c3eb7a8's "the boundary follows the arrow".
func TestResizeActiveLaneRightmostStillWidensOnRightArrow(t *testing.T) {
	mk := func() *TabActor {
		return &TabActor{
			laneRefs:   []*laneRef{{id: "l0", flex: 10}, {id: "l1", flex: 10}},
			activeLane: 1,
		}
	}

	tb := mk()
	tb.resizeActiveLane(1)
	t.Logf("rightmost RIGHT(+1): l0=%d l1=%d", tb.laneRefs[0].flex, tb.laneRefs[1].flex)
	if !(tb.laneRefs[1].flex > tb.laneRefs[0].flex) {
		t.Errorf("RIGHT on the rightmost lane: expected it to WIDEN, got l0=%d l1=%d",
			tb.laneRefs[0].flex, tb.laneRefs[1].flex)
	}

	tb = mk()
	tb.resizeActiveLane(-1)
	t.Logf("rightmost LEFT(-1): l0=%d l1=%d", tb.laneRefs[0].flex, tb.laneRefs[1].flex)
	if !(tb.laneRefs[1].flex < tb.laneRefs[0].flex) {
		t.Errorf("LEFT on the rightmost lane: expected it to NARROW, got l0=%d l1=%d",
			tb.laneRefs[0].flex, tb.laneRefs[1].flex)
	}
}

// A lane cannot be narrowed out of existence, and a donor cannot be starved:
// whichever side gives up width keeps at least minFlex, and the call is a no-op
// once there is nothing left to give.
func TestResizeActiveLaneKeepsDonorAlive(t *testing.T) {
	tb := &TabActor{
		laneRefs:   []*laneRef{{id: "l0", flex: 19}, {id: "l1", flex: 1}},
		activeLane: 0,
	}
	tb.resizeActiveLane(1) // widen l0 further — l1 is already at the floor
	t.Logf("donor at floor, RIGHT(+1): l0=%d l1=%d", tb.laneRefs[0].flex, tb.laneRefs[1].flex)
	if tb.laneRefs[1].flex < 1 {
		t.Errorf("donor lane fell below minFlex: l1=%d", tb.laneRefs[1].flex)
	}
	if tb.laneRefs[0].flex != 19 || tb.laneRefs[1].flex != 1 {
		t.Errorf("expected a no-op at the floor, got l0=%d l1=%d", tb.laneRefs[0].flex, tb.laneRefs[1].flex)
	}
}

// Vertical, the mirror of TestResizeActiveLaneAnchorsLeftEdge: ↓ makes the
// FOCUSED group taller and ↑ shorter, the height comes from the group below, and
// nothing above the focused group moves — so its top edge holds.
func TestResizeActiveGroupAnchorsTopEdge(t *testing.T) {
	mk := func(active int) *LaneActor {
		return &LaneActor{
			groupRefs: []*laneGroupRef{
				{id: "g0", rowFlex: 10},
				{id: "g1", rowFlex: 10},
				{id: "g2", rowFlex: 10},
			},
			activeGroup: active,
		}
	}

	for _, active := range []int{0, 1} {
		for _, tc := range []struct {
			name   string
			dir    int
			taller bool
		}{
			{"DOWN(+1) grows", 1, true},
			{"UP(-1) shrinks", -1, false},
		} {
			la := mk(active)
			before := la.groupRefs[active].rowFlex
			la.resizeActiveGroup(tc.dir)
			after := la.groupRefs[active].rowFlex
			t.Logf("active=%d %s: rowflex=%d,%d,%d", active, tc.name,
				la.groupRefs[0].rowFlex, la.groupRefs[1].rowFlex, la.groupRefs[2].rowFlex)

			if tc.taller && after <= before {
				t.Errorf("active=%d %s: focused group %d -> %d, wanted taller", active, tc.name, before, after)
			}
			if !tc.taller && after >= before {
				t.Errorf("active=%d %s: focused group %d -> %d, wanted shorter", active, tc.name, before, after)
			}
			for i := 0; i < active; i++ {
				if la.groupRefs[i].rowFlex != 10 {
					t.Errorf("active=%d %s: group %d moved (%d, want 10) — the top edge did not hold",
						active, tc.name, i, la.groupRefs[i].rowFlex)
				}
			}
			if la.groupRefs[active+1].rowFlex == 10 {
				t.Errorf("active=%d %s: group below unchanged; the height came from the wrong side", active, tc.name)
			}
		}
	}
}

// The bottommost group borrows upward, and ↓ must still mean TALLER there.
func TestResizeActiveGroupBottommostStillGrowsOnDownArrow(t *testing.T) {
	mk := func() *LaneActor {
		return &LaneActor{
			groupRefs:   []*laneGroupRef{{id: "g0", rowFlex: 10}, {id: "g1", rowFlex: 10}},
			activeGroup: 1,
		}
	}

	la := mk()
	la.resizeActiveGroup(1)
	t.Logf("bottommost DOWN(+1): g0=%d g1=%d", la.groupRefs[0].rowFlex, la.groupRefs[1].rowFlex)
	if !(la.groupRefs[1].rowFlex > la.groupRefs[0].rowFlex) {
		t.Errorf("DOWN on the bottommost group: expected it to GROW, got g0=%d g1=%d",
			la.groupRefs[0].rowFlex, la.groupRefs[1].rowFlex)
	}

	la = mk()
	la.resizeActiveGroup(-1)
	t.Logf("bottommost UP(-1): g0=%d g1=%d", la.groupRefs[0].rowFlex, la.groupRefs[1].rowFlex)
	if !(la.groupRefs[1].rowFlex < la.groupRefs[0].rowFlex) {
		t.Errorf("UP on the bottommost group: expected it to SHRINK, got g0=%d g1=%d",
			la.groupRefs[0].rowFlex, la.groupRefs[1].rowFlex)
	}
}

// Same floor as the width axis: the donor keeps at least minFlex and the call
// is a no-op once there is nothing left to give.
func TestResizeActiveGroupKeepsDonorAlive(t *testing.T) {
	la := &LaneActor{
		groupRefs:   []*laneGroupRef{{id: "g0", rowFlex: 19}, {id: "g1", rowFlex: 1}},
		activeGroup: 0,
	}
	la.resizeActiveGroup(1) // grow g0 further — g1 is already at the floor
	t.Logf("donor at floor, DOWN(+1): g0=%d g1=%d", la.groupRefs[0].rowFlex, la.groupRefs[1].rowFlex)
	if la.groupRefs[0].rowFlex != 19 || la.groupRefs[1].rowFlex != 1 {
		t.Errorf("expected a no-op at the floor, got g0=%d g1=%d", la.groupRefs[0].rowFlex, la.groupRefs[1].rowFlex)
	}
}
