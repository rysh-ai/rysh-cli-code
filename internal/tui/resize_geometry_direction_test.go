package tui

import (
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

// Higher Flex must yield a wider column.
func TestLaneWidthsMonotonic(t *testing.T) {
	lanes := []domain.LaneRenderInfo{
		{LaneID: "l0", Flex: 12},
		{LaneID: "l1", Flex: 8},
	}
	w := laneWidths(lanes, 100)
	t.Logf("flex 12/8 -> widths %v", w)
	if w[0] <= w[1] {
		t.Errorf("expected lane0 (flex 12) wider than lane1 (flex 8), got %v", w)
	}
}

// Higher RowFlex must yield a taller pane.
func TestFlexPaneHeightsMonotonic(t *testing.T) {
	panes := []domain.PaneSnapshot{
		{ID: "p0", RowFlex: 12},
		{ID: "p1", RowFlex: 8},
	}
	h := flexPaneHeights(panes, 40)
	t.Logf("rowflex 12/8 -> heights %v", h)
	if h[0] <= h[1] {
		t.Errorf("expected pane0 (rowflex 12) taller than pane1 (rowflex 8), got %v", h)
	}
}
