// SPDX-License-Identifier: Apache-2.0

package actors

import "testing"

func threeLanes(active int) *TabActor {
	return &TabActor{
		laneRefs: []*laneRef{
			{id: "l0", flex: 10},
			{id: "l1", flex: 10},
			{id: "l2", flex: 10},
		},
		activeLane: active,
	}
}

func collapsedSet(t *TabActor) []bool {
	out := make([]bool, len(t.laneRefs))
	for i, lr := range t.laneRefs {
		out[i] = lr.collapsed
	}
	return out
}

func TestSetLaneCollapsed_FoldsAndOpens(t *testing.T) {
	tb := threeLanes(0)
	if e := tb.setLaneCollapsed("l1", true); e != "" {
		t.Fatalf("collapse l1: %s", e)
	}
	if got := collapsedSet(tb); got[0] || !got[1] || got[2] {
		t.Fatalf("after collapse l1: %v", got)
	}
	if tb.activeLane != 0 {
		t.Errorf("folding an unfocused lane moved focus to %d", tb.activeLane)
	}
	// Idempotent both ways.
	if e := tb.setLaneCollapsed("l1", true); e != "" {
		t.Errorf("re-collapse: %s", e)
	}
	if e := tb.setLaneCollapsed("l1", false); e != "" || tb.laneRefs[1].collapsed {
		t.Errorf("expand l1: %q collapsed=%v", e, tb.laneRefs[1].collapsed)
	}
	// Flex is untouched: opening restores the layout the lane had.
	for _, lr := range tb.laneRefs {
		if lr.flex != 10 {
			t.Errorf("lane %s flex = %d; collapse must not touch flex", lr.id, lr.flex)
		}
	}
}

func TestSetLaneCollapsed_LastOpenLaneRefused(t *testing.T) {
	tb := threeLanes(0)
	_ = tb.setLaneCollapsed("l1", true)
	_ = tb.setLaneCollapsed("l2", true)
	if e := tb.setLaneCollapsed("l0", true); e == "" {
		t.Fatal("folding the last open lane must be refused")
	}
	if tb.laneRefs[0].collapsed {
		t.Error("the refused lane was folded anyway")
	}
	if e := tb.setLaneCollapsed("nope", true); e == "" {
		t.Error("an unknown lane must be an error")
	}
}

// Folding the focused lane moves focus to the nearest open lane, right first.
func TestSetLaneCollapsed_MovesFocusOffFoldedLane(t *testing.T) {
	tb := threeLanes(1)
	_ = tb.setLaneCollapsed("l1", true)
	if tb.activeLane != 2 {
		t.Errorf("focus after folding the middle lane = %d; want 2 (right first)", tb.activeLane)
	}
	tb = threeLanes(2)
	_ = tb.setLaneCollapsed("l2", true)
	if tb.activeLane != 1 {
		t.Errorf("focus after folding the last lane = %d; want 1", tb.activeLane)
	}
}

func TestFocusLaneLeftRightLandOnFoldedLanes(t *testing.T) {
	tb := threeLanes(0)
	_ = tb.setLaneCollapsed("l1", true)
	tb.focusLaneRight()
	if tb.activeLane != 1 {
		t.Errorf("right from 0 onto folded 1 = %d; want 1", tb.activeLane)
	}
	tb.focusLaneRight()
	if tb.activeLane != 2 {
		t.Errorf("right from folded 1 = %d; want 2", tb.activeLane)
	}
	tb.focusLaneLeft()
	tb.focusLaneLeft()
	if tb.activeLane != 0 {
		t.Errorf("left twice from 2 = %d; want 0", tb.activeLane)
	}
	tb.focusLaneLeft()
	if tb.activeLane != 0 {
		t.Errorf("left from 0 = %d; want 0 (no lane to the left)", tb.activeLane)
	}
}

func TestSettleCollapsedLanes(t *testing.T) {
	// Only folded lanes left (the open one was deleted): all open again.
	tb := &TabActor{laneRefs: []*laneRef{{id: "a", collapsed: true}, {id: "b", collapsed: true}}}
	tb.settleCollapsedLanes()
	if got := collapsedSet(tb); got[0] || got[1] {
		t.Errorf("no open lane left must open all: %v", got)
	}
	// Focus left on a folded lane moves to an open one.
	tb = &TabActor{laneRefs: []*laneRef{{id: "a"}, {id: "b", collapsed: true}}, activeLane: 1}
	tb.settleCollapsedLanes()
	if tb.activeLane != 0 {
		t.Errorf("focus on a folded lane = %d; want 0", tb.activeLane)
	}
}

// The tab owns the flag and stamps it into the lane's KV document, like flex;
// a restore must bring it back.
func TestLaneCollapsed_PersistsThroughKV(t *testing.T) {
	tb := threeLanes(0)
	_ = tb.setLaneCollapsed("l1", true)
	kv := tb.ToKV()
	if !kv.Lanes[1].Collapsed || kv.Lanes[0].Collapsed {
		t.Fatalf("tab KV lanes collapsed = %v,%v,%v", kv.Lanes[0].Collapsed, kv.Lanes[1].Collapsed, kv.Lanes[2].Collapsed)
	}
}
