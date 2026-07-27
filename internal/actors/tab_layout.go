package actors

// ---------------------------------------------------------------------------
// Resize
// ---------------------------------------------------------------------------

// resizeActiveLane moves the split boundary on one side of the active lane in
// the arrow's direction. dir encodes the screen direction: dir > 0 points
// toward the higher index (right), dir < 0 toward the lower index (left).
//
// The boundary always follows the arrow, regardless of which lane is focused:
//   - If the active lane has a neighbor on the arrow side, the boundary between
//     them moves that way, so the active lane GROWS into the neighbor.
//   - If the active lane is already at that edge, the boundary on its other
//     side moves that way instead, so the active lane SHRINKS.
//
// e.g. with the right lane focused, pressing → moves the divider right (the
// focused lane shrinks); pressing ← moves it left (the focused lane grows).
func (t *TabActor) resizeActiveLane(dir int) {
	if dir == 0 || len(t.laneRefs) < 2 {
		return
	}
	step := 1
	if dir < 0 {
		step = -1
	}

	// Prefer the neighbor on the arrow side (grow); fall back to the opposite
	// neighbor (shrink) when the active lane is at that edge.
	grow := true
	neighborIdx := t.activeLane + step
	if neighborIdx < 0 || neighborIdx >= len(t.laneRefs) {
		neighborIdx = t.activeLane - step
		grow = false
	}
	if neighborIdx < 0 || neighborIdx >= len(t.laneRefs) {
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
