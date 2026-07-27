package actors

import "testing"

// The split boundary must follow the arrow direction regardless of which
// lane/group is focused. dir > 0 means the arrow points toward the higher
// index (right for lanes, down for groups); dir < 0 toward the lower index
// (left / up).

// Horizontal: RIGHT (dir +1) moves the divider right -> the left lane (index 0)
// ends up wider and the right lane (index 1) narrower, for either focus.
func TestResizeActiveLaneBoundaryFollowsArrow(t *testing.T) {
	mk := func(active int) *TabActor {
		return &TabActor{
			laneRefs: []*laneRef{
				{id: "l0", flex: 10},
				{id: "l1", flex: 10},
			},
			activeLane: active,
		}
	}

	for _, active := range []int{0, 1} {
		// RIGHT arrow -> divider moves right -> l0 grows, l1 shrinks.
		tb := mk(active)
		tb.resizeActiveLane(1)
		t.Logf("active=%d RIGHT(+1): l0=%d l1=%d", active, tb.laneRefs[0].flex, tb.laneRefs[1].flex)
		if !(tb.laneRefs[0].flex > tb.laneRefs[1].flex) {
			t.Errorf("active=%d RIGHT: expected l0>l1 (divider moved right), got l0=%d l1=%d",
				active, tb.laneRefs[0].flex, tb.laneRefs[1].flex)
		}

		// LEFT arrow -> divider moves left -> l0 shrinks, l1 grows.
		tb = mk(active)
		tb.resizeActiveLane(-1)
		t.Logf("active=%d LEFT(-1): l0=%d l1=%d", active, tb.laneRefs[0].flex, tb.laneRefs[1].flex)
		if !(tb.laneRefs[0].flex < tb.laneRefs[1].flex) {
			t.Errorf("active=%d LEFT: expected l0<l1 (divider moved left), got l0=%d l1=%d",
				active, tb.laneRefs[0].flex, tb.laneRefs[1].flex)
		}
	}
}

// Vertical: DOWN (dir +1) moves the divider down -> the top group (index 0)
// ends up taller and the bottom group (index 1) shorter, for either focus.
func TestResizeActiveGroupBoundaryFollowsArrow(t *testing.T) {
	mk := func(active int) *LaneActor {
		return &LaneActor{
			groupRefs: []*laneGroupRef{
				{id: "g0", rowFlex: 10},
				{id: "g1", rowFlex: 10},
			},
			activeGroup: active,
		}
	}

	for _, active := range []int{0, 1} {
		// DOWN arrow -> divider moves down -> g0 grows, g1 shrinks.
		la := mk(active)
		la.resizeActiveGroup(1)
		t.Logf("active=%d DOWN(+1): g0=%d g1=%d", active, la.groupRefs[0].rowFlex, la.groupRefs[1].rowFlex)
		if !(la.groupRefs[0].rowFlex > la.groupRefs[1].rowFlex) {
			t.Errorf("active=%d DOWN: expected g0>g1 (divider moved down), got g0=%d g1=%d",
				active, la.groupRefs[0].rowFlex, la.groupRefs[1].rowFlex)
		}

		// UP arrow -> divider moves up -> g0 shrinks, g1 grows.
		la = mk(active)
		la.resizeActiveGroup(-1)
		t.Logf("active=%d UP(-1): g0=%d g1=%d", active, la.groupRefs[0].rowFlex, la.groupRefs[1].rowFlex)
		if !(la.groupRefs[0].rowFlex < la.groupRefs[1].rowFlex) {
			t.Errorf("active=%d UP: expected g0<g1 (divider moved up), got g0=%d g1=%d",
				active, la.groupRefs[0].rowFlex, la.groupRefs[1].rowFlex)
		}
	}
}
