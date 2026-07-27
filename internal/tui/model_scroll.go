package tui

// ---------------------------------------------------------------------------
// Scroll helpers
// ---------------------------------------------------------------------------

// scrollOffset returns the scroll offset for a pane (0 = at bottom).
func (m Model) scrollOffset(paneID string) int {
	if m.paneScrollOffsets == nil {
		return 0
	}
	return m.paneScrollOffsets[paneID]
}

// scrollToBottom resets the scroll offset for a pane to 0 (follow tail).
func (m *Model) scrollToBottom(paneID string) {
	if m.paneScrollOffsets != nil {
		m.paneScrollOffsets[paneID] = 0
	}
}

// scrollActivePaneUp scrolls the active pane toward history by the given number
// of rows, clamped to the maximum scrollable range.
func (m *Model) scrollActivePaneUp(lines int) {
	id := m.snapshot.ActivePaneID
	if m.findPaneInSnapshot(id) == nil {
		return
	}
	visibleLines := m.paneVisibleLines(id)
	totalLines := m.paneRowCount(id)
	maxScroll := totalLines - visibleLines
	if maxScroll < 0 {
		maxScroll = 0
	}
	offset := m.scrollOffset(id) + lines
	if offset > maxScroll {
		offset = maxScroll
	}
	m.paneScrollOffsets[id] = offset
}

// scrollActivePaneDown scrolls the active pane toward the present by the given
// number of rows, stopping at 0 (the tail).
func (m *Model) scrollActivePaneDown(lines int) {
	id := m.snapshot.ActivePaneID
	offset := m.scrollOffset(id) - lines
	if offset < 0 {
		offset = 0
	}
	m.paneScrollOffsets[id] = offset
}

// scrollActivePaneToTop scrolls the active pane all the way to the top.
func (m *Model) scrollActivePaneToTop() {
	id := m.snapshot.ActivePaneID
	if m.findPaneInSnapshot(id) == nil {
		return
	}
	visibleLines := m.paneVisibleLines(id)
	totalLines := m.paneRowCount(id)
	maxScroll := totalLines - visibleLines
	if maxScroll < 0 {
		maxScroll = 0
	}
	m.paneScrollOffsets[id] = maxScroll
}

// paneVisibleLines returns the number of on-screen output rows that fit in the
// pane's visible area, based on the pane rect height and overhead rows (meta,
// blank, input, blank).
func (m Model) paneVisibleLines(paneID string) int {
	for _, r := range m.paneRects {
		if r.paneID == paneID {
			// Subtract overhead: meta line, blank, input prompt, blank = 4 rows.
			v := r.h - 4
			if v < 1 {
				v = 1
			}
			return v
		}
	}
	return 10 // fallback
}

// paneContentWidth returns the wrapped content text width for a pane (border and
// padding excluded), matching the width the renderer wraps output to. Returns 0
// when the pane has no known rectangle yet, in which case callers treat output
// as un-wrapped (one row per logical line).
func (m Model) paneContentWidth(paneID string) int {
	for _, r := range m.paneRects {
		if r.paneID == paneID {
			return r.w
		}
	}
	return 0
}

// paneRowCount returns the number of on-screen rows the pane's active-mode output
// occupies once wrapped to the pane's content width. This is the same row count
// the renderer produces, so scroll clamping and rendering stay in agreement.
func (m Model) paneRowCount(paneID string) int {
	pane := m.findPaneInSnapshot(paneID)
	if pane == nil {
		return 0
	}
	output := m.paneOutputForMode(pane)
	if output == "" {
		return 0
	}
	return len(wrapRows(output, m.paneContentWidth(paneID)))
}
