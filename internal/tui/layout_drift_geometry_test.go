// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

// These tests lock in the Family-B fix for the layout-drift investigation: the
// LAST lane/pane used to absorb an unclamped remainder that could go negative
// once earlier elements were clamped up past their proportional share (lopsided
// flex weights or a small terminal). A negative Width/Height corrupts both the
// lipgloss render and the mouse-hit rects derived from the same numbers.

// laneWidths: with grossly lopsided flex on a narrow terminal, the last lane
// must never be driven below the 12-column floor (previously it went negative).
func TestLaneWidthsLastLaneNeverNegative(t *testing.T) {
	cases := []struct {
		name  string
		flex  []int
		width int
	}{
		{"one-huge-two-tiny narrow", []int{100, 1, 1}, 40},
		{"one-huge-one-tiny wide", []int{100, 1}, 113},
		{"descending drift 5,3,1,1 narrow", []int{5, 3, 1, 1}, 50},
		{"many tiny + one huge", []int{1, 1, 1, 1, 100}, 60},
		{"zero-flex tail", []int{50, 0, 0}, 44},
	}
	for _, c := range cases {
		lanes := make([]domain.LaneRenderInfo, len(c.flex))
		for i, f := range c.flex {
			lanes[i] = domain.LaneRenderInfo{LaneID: "l", Flex: f}
		}
		w := laneWidths(lanes, c.width)
		t.Logf("%s: flex %v width %d -> %v", c.name, c.flex, c.width, w)
		if len(w) != len(c.flex) {
			t.Fatalf("%s: got %d widths, want %d", c.name, len(w), len(c.flex))
		}
		for i, x := range w {
			if x < 12 {
				t.Errorf("%s: lane %d width %d below the 12-col floor (was negative before the fix): %v",
					c.name, i, x, w)
			}
		}
	}
}

// flexPaneHeights: many vertical splits on a short terminal must not drive the
// last pane's height negative; every pane keeps its 4-row minimum.
func TestFlexPaneHeightsLastPaneNeverNegative(t *testing.T) {
	cases := []struct {
		name    string
		rowFlex []int
		height  int
	}{
		{"8 equal splits, tiny budget", []int{10, 10, 10, 10, 10, 10, 10, 10}, 8},
		{"one-huge-two-tiny", []int{100, 1, 1}, 12},
		{"descending drift 5,3,1,1", []int{5, 3, 1, 1}, 10},
		{"zero-flex tail", []int{50, 0, 0}, 9},
	}
	for _, c := range cases {
		panes := make([]domain.PaneSnapshot, len(c.rowFlex))
		for i, f := range c.rowFlex {
			panes[i] = domain.PaneSnapshot{ID: "p", RowFlex: f}
		}
		h := flexPaneHeights(panes, c.height)
		t.Logf("%s: rowFlex %v height %d -> %v", c.name, c.rowFlex, c.height, h)
		if len(h) != len(c.rowFlex) {
			t.Fatalf("%s: got %d heights, want %d", c.name, len(h), len(c.rowFlex))
		}
		for i, y := range h {
			if y < 4 {
				t.Errorf("%s: pane %d height %d below the 4-row floor (was negative before the fix): %v",
					c.name, i, y, h)
			}
		}
	}
}

// flexPaneHeights floors its input budget to nPanes*4 so that even a
// pathologically short terminal cannot starve a pane. (laneWidths already
// floors its width budget to nLanes*12; this asserts the height equivalent.)
func TestFlexPaneHeightsBudgetFloored(t *testing.T) {
	panes := make([]domain.PaneSnapshot, 5)
	for i := range panes {
		panes[i] = domain.PaneSnapshot{ID: "p", RowFlex: 10}
	}
	h := flexPaneHeights(panes, 3) // absurdly small budget
	sum := 0
	for _, y := range h {
		sum += y
	}
	if sum < 5*4 {
		t.Errorf("expected total height floored to >= 20 (5 panes * 4), got %d from %v", sum, h)
	}
}

// Monotonicity must still hold after the floor clamps: a larger weight yields a
// larger (or equal) dimension, not a smaller one.
func TestLaneWidthsMonotonicWithFloor(t *testing.T) {
	lanes := []domain.LaneRenderInfo{
		{LaneID: "l0", Flex: 30},
		{LaneID: "l1", Flex: 10},
	}
	w := laneWidths(lanes, 120)
	if w[0] < w[1] {
		t.Errorf("expected wider lane for larger flex, got %v", w)
	}
}
