// SPDX-License-Identifier: Apache-2.0

package actors

// ---------------------------------------------------------------------------
// Resize
// ---------------------------------------------------------------------------

// resizeActiveLane widens or narrows the ACTIVE lane, anchored on its left
// edge. dir > 0 (→) makes it wider, dir < 0 (←) narrower; the width is taken
// from — or handed back to — the lane on its RIGHT, so only the boundary on the
// active lane's right-hand side moves and its left edge stays where it is.
//
// The rightmost lane is the one case where that anchor cannot hold: its right
// edge is the edge of the tab and nothing can move it, so it borrows from its
// LEFT neighbor instead and → still means wider. The alternative was to make
// the arrows inert there, which costs a whole lane its keys (with two lanes,
// half of them) to preserve an invariant that is already unachievable.
//
// This replaces "the boundary follows the arrow regardless of focus" (c3eb7a8).
// That rule reads correctly when you are looking at a divider, but it means ←
// GROWS a focused middle lane leftward — the focused lane's own left edge slides
// out from under it, which is what made the gesture hard to aim. dir keeps its
// screen-direction encoding (+1 = right) so every caller is unchanged; only what
// the lane does with it changed. resizeActiveGroup is the vertical twin and
// follows this rule exactly, with "below" in place of "right".
func (t *TabActor) resizeActiveLane(dir int) {
	if dir == 0 || len(t.laneRefs) < 2 {
		return
	}

	// The partner is always the lane on the RIGHT — that is the boundary the
	// arrows are allowed to move. Only the rightmost lane, which has none,
	// falls back to the lane on its left.
	grow := dir > 0
	neighborIdx := t.activeLane + 1
	if neighborIdx >= len(t.laneRefs) {
		neighborIdx = t.activeLane - 1
	}
	if neighborIdx < 0 {
		return
	}

	// Compute 10% of total flex as the resize increment.
	totalFlex := 0
	for _, lr := range t.laneRefs {
		totalFlex += lr.flex
	}
	d := totalFlex / 10
	if d < 1 {
		d = 1
	}

	const minFlex = 1
	// The lane that shrinks must keep at least minFlex.
	shrinkIdx := neighborIdx
	if !grow {
		shrinkIdx = t.activeLane
	}
	if t.laneRefs[shrinkIdx].flex-d < minFlex {
		d = t.laneRefs[shrinkIdx].flex - minFlex
	}
	if d <= 0 {
		return
	}
	if grow {
		t.laneRefs[t.activeLane].flex += d
		t.laneRefs[neighborIdx].flex -= d
	} else {
		t.laneRefs[t.activeLane].flex -= d
		t.laneRefs[neighborIdx].flex += d
	}
	t.normalizeFlex()
}

func (t *TabActor) normalizeFlex() {
	for _, lr := range t.laneRefs {
		if lr.flex < 1 {
			lr.flex = 1
		}
	}
}

// ---------------------------------------------------------------------------
// Layout management
// ---------------------------------------------------------------------------

func (t *TabActor) equalizeLanes() {
	for _, lr := range t.laneRefs {
		lr.flex = 10
	}
}

func (t *TabActor) swapLanes() {
	if len(t.laneRefs) < 2 {
		return
	}
	next := (t.activeLane + 1) % len(t.laneRefs)
	t.laneRefs[t.activeLane], t.laneRefs[next] = t.laneRefs[next], t.laneRefs[t.activeLane]
	t.activeLane = next
	t.updateActivePaneFromLane()
}
