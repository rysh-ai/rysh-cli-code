package tui

import (
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

func (m Model) activeTab() *domain.TabSnapshot {
	for _, tab := range m.snapshot.Tabs {
		if tab.ID == m.snapshot.ActiveTabID {
			return &tab
		}
	}
	return nil
}

// isSnapshotPane checks whether a pane ID exists in the current workspace
// snapshot. Returns false for headless actor IDs (agents, humanoids) that
// use name-based pane IDs instead of real UUIDs.
func (m Model) isSnapshotPane(paneID string) bool {
	return domain.FindPaneInWorkspace(&m.snapshot, paneID) != nil
}

// findPaneInSnapshot returns a pointer to a PaneSnapshot by ID, or nil.
func (m Model) findPaneInSnapshot(paneID string) *domain.PaneSnapshot {
	for _, tab := range m.snapshot.Tabs {
		for _, lane := range tab.Lanes {
			for _, g := range lane.PaneGroups {
				for i := range g.Panes {
					if g.Panes[i].ID == paneID {
						return &g.Panes[i]
					}
				}
			}
		}
	}
	return nil
}

// paneExistsInSnapshot returns true if a pane with the given ID is present in
// any tab of the current snapshot (including stacked background panes).
func (m Model) paneExistsInSnapshot(paneID string) bool {
	return domain.FindPaneInWorkspace(&m.snapshot, paneID) != nil
}

// paneAvailWidth returns the total width budget for lipgloss Width() values
// across all columns. lipgloss Width includes padding but EXCLUDES borders, so
// each column's rendered width on screen = Width + 2 (left+right border chars).
// We subtract 2*nColumns for borders and 2 for the body's Padding(0,1).
func (m Model) paneAvailWidth(nColumns int) int {
	return max(24, m.width-2-2*nColumns)
}

// flexPaneHeights computes lipgloss Height() values for panes stacked in
// a lane column, proportional to each pane's RowFlex weight. totalHeight is
// the available content-height budget (borders already subtracted). The last
// pane absorbs any rounding remainder.
func flexPaneHeights(panes []domain.PaneSnapshot, totalHeight int) []int {
	n := len(panes)
	if n == 0 {
		return nil
	}
	if n == 1 {
		return []int{max(4, totalHeight)}
	}

	// Floor the budget so every pane can meet its 4-row minimum. Without this, a
	// column with many vertical splits on a short terminal drove the LAST pane's
	// height negative (the last pane absorbs the remainder), corrupting both the
	// lipgloss render and the mouse-hit rects derived from the same numbers.
	// Family-B of the layout-drift bug. (laneWidths already floors its width
	// budget the same way.)
	if totalHeight < n*4 {
		totalHeight = n * 4
	}

	totalFlex := 0
	for _, p := range panes {
		f := p.RowFlex
		if f < 1 {
			f = 1
		}
		totalFlex += f
	}
	if totalFlex == 0 {
		totalFlex = n
	}

	heights := make([]int, n)
	remaining := totalHeight
	remainingFlex := totalFlex
	for i, p := range panes {
		flex := p.RowFlex
		if flex < 1 {
			flex = 1
		}
		var h int
		switch {
		case i == n-1:
			h = remaining // last pane absorbs the rounding remainder
		case remainingFlex > 0:
			h = remaining * flex / remainingFlex
		default:
			h = remaining / (n - i)
		}
		// Floor EVERY pane to the 4-row minimum (last and zero-flex included).
		// Earlier panes can be clamped up past their proportional share, leaving
		// the remainder short (even negative on a short terminal with many
		// splits); a uniform floor keeps heights non-negative. Family-B of the
		// layout-drift bug.
		if h < 4 {
			h = 4
		}
		heights[i] = h
		remaining -= h
		remainingFlex -= flex
	}
	return heights
}

// laneWidths computes the lipgloss Width() values for lanes based on their flex
// weights. totalWidth is the available content-width budget. The last lane
// absorbs any rounding remainder.
func laneWidths(lanes []domain.LaneRenderInfo, totalWidth int) []int {
	if len(lanes) == 0 {
		return nil
	}

	if totalWidth < len(lanes)*12 {
		totalWidth = len(lanes) * 12
	}

	// Sum flex with a floor of 1 per lane so a zero-flex lane still counts and
	// remainingFlex stays consistent with the per-iteration decrement below.
	totalFlex := 0
	for _, lane := range lanes {
		flex := lane.Flex
		if flex < 1 {
			flex = 1
		}
		totalFlex += flex
	}

	widths := make([]int, len(lanes))
	remainingWidth := totalWidth
	remainingFlex := totalFlex
	for i, lane := range lanes {
		flex := lane.Flex
		if flex < 1 {
			flex = 1
		}
		var width int
		switch {
		case i == len(lanes)-1:
			width = remainingWidth // last lane absorbs the rounding remainder
		case remainingFlex > 0:
			width = remainingWidth * flex / remainingFlex
		default:
			width = remainingWidth / max(1, len(lanes)-i)
		}
		// Floor EVERY lane to the 12-col minimum — the last lane and zero-flex
		// lanes included. Earlier lanes can be clamped up past their proportional
		// share, leaving the remainder short (even negative under lopsided flex);
		// without a uniform floor the last lane, or a zero-flex middle lane,
		// collapsed to <=0 and corrupted the render plus the mouse rects derived
		// from it. Family-B of the layout-drift bug.
		if width < 12 {
			width = 12
		}
		widths[i] = width
		remainingWidth -= width
		remainingFlex -= flex
	}

	return widths
}
