// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/atotto/clipboard"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// normalized returns the selection bounds with start always before end.
func (s mouseSelection) normalized() (startRow, startCol, endRow, endCol int) {
	if s.startY < s.endY || (s.startY == s.endY && s.startX <= s.endX) {
		return s.startY, s.startX, s.endY, s.endX
	}
	return s.endY, s.endX, s.startY, s.startX
}

// ---------------------------------------------------------------------------
// Mouse selection
// ---------------------------------------------------------------------------

// recomputePaneRects calculates the screen rectangle for each visible pane's
// text content area (inside border + padding). These rects are used for mouse
// hit-testing.
func (m *Model) recomputePaneRects() {
	tab := m.activeTab()
	if tab == nil {
		m.paneRects = nil
		return
	}

	// The body is laid out directly below the header in View(). The header
	// height is NOT a constant: the tab bar wraps to additional rows when the
	// tab labels exceed the terminal width. Hardcoding it (the old behaviour)
	// shifts every pane rectangle upward whenever the header wraps, so mouse
	// hit-testing — and therefore text selection — maps to the wrong rows.
	// Measure the actual rendered header instead so the rects match the screen.
	headerHeight := lipgloss.Height(m.renderHeader())

	// Fullscreen mode: single pane rect fills the whole body.
	if m.fullscreenPaneID != "" {
		for _, pane := range tab.FlatPanes() {
			if pane.ID == m.fullscreenPaneID {
				fsWidth := m.fullscreenWidth()
				fsHeight := m.bodyHeight()
				m.paneRects = []paneRect{{
					paneID: pane.ID,
					// bodyXOffset (vertical tab column + outer Padding(0,1))
					// + border + inner padding.
					x: m.bodyXOffset() + 2,
					y: headerHeight + 1,
					w: fsWidth - 2,
					h: fsHeight,
				}}
				return
			}
		}
		// Fullscreen pane gone — fall through to normal layout.
	}

	lanes := tab.FlatLanes()
	colWidths := laneWidths(lanes, m.paneAvailWidth(len(lanes)))
	totalHeight := m.bodyHeight()

	var rects []paneRect
	xOffset := m.bodyXOffset() // vertical tab column + body outer Padding(0,1) left

	for c, lane := range lanes {
		colWidth := colWidths[c]
		col := lane.VisiblePanes
		if len(col) == 0 {
			xOffset += colWidth + 2
			continue
		}

		// Separate expanded and collapsed panes for height calculation.
		var expandedPanes []domain.PaneSnapshot
		collapsedCount := 0
		for _, pane := range col {
			if pane.StackCollapsed {
				collapsedCount++
			} else {
				expandedPanes = append(expandedPanes, pane)
			}
		}
		availH := totalHeight - 2*max(0, len(expandedPanes)-1) - collapsedCount
		heights := flexPaneHeights(expandedPanes, availH)

		yOffset := headerHeight + 1 // +1 for the top border of the first pane in the column
		expandedIdx := 0

		for _, pane := range col {
			if pane.StackCollapsed {
				// Collapsed panes occupy 1 row — just a title line.
				rects = append(rects, paneRect{
					paneID: pane.ID,
					x:      xOffset,
					y:      yOffset,
					w:      colWidth,
					h:      1,
				})
				yOffset += 1
				continue
			}
			h := heights[expandedIdx]
			expandedIdx++
			rects = append(rects, paneRect{
				paneID: pane.ID,
				x:      xOffset + 2, // +1 border left +1 padding left
				y:      yOffset,
				w:      colWidth - 2, // Width - leftPad - rightPad
				h:      h,
			})
			// Next pane starts after: content(h) + bottom border(1) + top border of next(1).
			yOffset += h + 2
		}

		xOffset += colWidth + 2 // rendered width = Width + 2 border chars
	}

	m.paneRects = rects
}

// handleMouse processes mouse events: click-to-focus, drag-to-select,
// copy-on-release, and mouse-wheel scroll.
func (m Model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	switch msg.Action {
	case tea.MouseActionPress:
		// Mouse wheel scroll.
		if msg.Button == tea.MouseButtonWheelUp || msg.Button == tea.MouseButtonWheelDown {
			paneID, _, _ := m.hitTestPane(msg.X, msg.Y)
			if paneID == "" {
				return m, nil
			}
			if m.findPaneInSnapshot(paneID) == nil {
				return m, nil
			}
			const scrollStep = 3
			visibleLines := m.paneVisibleLines(paneID)
			totalLines := m.paneRowCount(paneID)
			maxScroll := totalLines - visibleLines
			if maxScroll < 0 {
				maxScroll = 0
			}
			if m.paneScrollOffsets == nil {
				m.paneScrollOffsets = make(map[string]int)
			}
			offset := m.paneScrollOffsets[paneID]
			if msg.Button == tea.MouseButtonWheelUp {
				offset += scrollStep
				if offset > maxScroll {
					offset = maxScroll
				}
			} else {
				offset -= scrollStep
				if offset < 0 {
					offset = 0
				}
			}
			m.paneScrollOffsets[paneID] = offset
			return m, nil
		}
		// Right-click: copy the active selection to clipboard (if any).
		if msg.Button == tea.MouseButtonRight {
			if m.selection.active {
				cmd := m.copySelectionToClipboard()
				return m, cmd
			}
			return m, nil
		}
		if msg.Button != tea.MouseButtonLeft {
			return m, nil
		}
		paneID, localX, localY := m.hitTestPane(msg.X, msg.Y)
		if paneID == "" {
			m.selection = mouseSelection{}
			return m, nil
		}
		// Focus the clicked pane if it is not already active.
		var cmd tea.Cmd
		if paneID != m.snapshot.ActivePaneID {
			m.sendMsg(&msgpkg.MsgFocusPaneByID{ID: paneID})
			cmd = m.refreshCmd()
		}
		m.selection = mouseSelection{
			active:   false,
			dragging: true,
			paneID:   paneID,
			startX:   localX,
			startY:   localY,
			endX:     localX,
			endY:     localY,
		}
		return m, cmd

	case tea.MouseActionMotion:
		if !m.selection.dragging {
			return m, nil
		}
		m.selection.active = true
		m.selection.dragged = true
		localX, localY := m.clampToPane(msg.X, msg.Y, m.selection.paneID)
		m.selection.endX = localX
		m.selection.endY = localY
		return m, nil

	case tea.MouseActionRelease:
		if !m.selection.dragging {
			return m, nil
		}
		m.selection.dragging = false

		// Always update end coordinates from the release position.
		// Some terminals don't send motion events during drag (only
		// press + release), so we must capture the final position here
		// to support multi-line selection on those terminals.
		localX, localY := m.clampToPane(msg.X, msg.Y, m.selection.paneID)
		m.selection.endX = localX
		m.selection.endY = localY

		// Treat it as a drag if we got motion events OR if the release
		// position differs from the press position.
		positionMoved := localX != m.selection.startX || localY != m.selection.startY
		if m.selection.dragged || positionMoved {
			m.selection.active = true
			m.selection.dragged = true
			cmd := m.copySelectionToClipboard()
			return m, cmd
		} else {
			m.selection = mouseSelection{}
		}
		return m, nil
	}
	return m, nil
}

// hitTestPane returns the pane ID and local (x, y) coordinates if the screen
// position falls within a pane's text area. Returns ("", 0, 0) on miss.
func (m Model) hitTestPane(screenX, screenY int) (string, int, int) {
	for _, r := range m.paneRects {
		if screenX >= r.x && screenX < r.x+r.w &&
			screenY >= r.y && screenY < r.y+r.h {
			return r.paneID, screenX - r.x, screenY - r.y
		}
	}
	return "", 0, 0
}

// clampToPane constrains screen coordinates to a specific pane's text area and
// returns the local coordinates.
func (m Model) clampToPane(screenX, screenY int, paneID string) (int, int) {
	for _, r := range m.paneRects {
		if r.paneID == paneID {
			localX := max(0, min(screenX-r.x, r.w-1))
			localY := max(0, min(screenY-r.y, r.h-1))
			return localX, localY
		}
	}
	return 0, 0
}

// copySelectionToClipboard extracts the selected text and copies it to the
// system clipboard. Returns a tea.Cmd that clears the flash notification
// after a short delay.
//
// The primary path is OSC 52 (see osc52.go), written to the controlling tty
// and wrapped for tmux/screen as the environment requires. OSC 52 reaches the
// user's LOCAL terminal — and thus the local clipboard — even when rysh runs
// on a remote host over SSH, where atotto/clipboard (pbcopy/xclip/wl-copy)
// would only ever reach the remote host's clipboard. atotto is kept as a
// best-effort local fallback whose failure is intentionally ignored once
// OSC 52 has been emitted.
func (m *Model) copySelectionToClipboard() tea.Cmd {
	text := m.extractSelectedText()
	if text == "" {
		return nil
	}
	out, closeOut := OSC52Output()
	err := WriteOSC52(out, os.Getenv, text)
	closeOut()
	if err != nil {
		m.flashMsg = fmt.Sprintf("clipboard error: %v", err)
	} else {
		// Best-effort local fallback; over SSH this reaches only the remote
		// host's clipboard, so ignore its error — OSC 52 already succeeded.
		_ = clipboard.WriteAll(text)
		lines := strings.Count(text, "\n") + 1
		chars := len([]rune(text))
		m.flashMsg = fmt.Sprintf("copied %d line(s), %d char(s) to clipboard", lines, chars)
	}
	m.flashExpires = time.Now().Add(3 * time.Second)
	return tea.Tick(3*time.Second, func(time.Time) tea.Msg {
		return flashClearMsg{}
	})
}

// extractSelectedText returns the plain text covered by the current selection.
func (m Model) extractSelectedText() string {
	if !m.selection.active {
		return ""
	}

	tab := m.activeTab()
	if tab == nil {
		return ""
	}

	// Derive the pane's text-area dimensions from m.paneRects — the exact same
	// geometry used to render the highlighted selection (and to hit-test the
	// mouse). Recomputing them here independently risked diverging from the
	// render (e.g. for stacked/collapsed panes or fullscreen), which made the
	// copied text differ from what was visibly highlighted.
	var rect *paneRect
	for i := range m.paneRects {
		if m.paneRects[i].paneID == m.selection.paneID {
			rect = &m.paneRects[i]
			break
		}
	}
	if rect == nil {
		return ""
	}

	// Find the matching pane snapshot, using the same FlatLanes view the
	// renderer uses so per-mode output and meta fields line up exactly.
	var pane *domain.PaneSnapshot
	for _, lane := range tab.FlatLanes() {
		for _, p := range lane.VisiblePanes {
			if p.ID == m.selection.paneID {
				found := p
				pane = &found
				break
			}
		}
		if pane != nil {
			break
		}
	}
	if pane == nil {
		return ""
	}

	height := rect.h
	textWidth := max(1, rect.w)
	// Extract using each pane's OWN input mode (not only the active pane's), so
	// copying from a background chat/prompt/rysh pane pulls from the buffer that
	// is actually displayed — mirroring buildDisplayLines in the renderer.
	fsMode := m.paneInputModeFor(pane.ID)
	displayLines := buildDisplayLines(*pane, *tab, textWidth, height, m.inputValueFor(pane.ID), m.scrollOffset(pane.ID), fsMode)

	startRow, startCol, endRow, endCol := m.selection.normalized()

	if startRow >= len(displayLines) {
		return ""
	}
	if endRow >= len(displayLines) {
		endRow = len(displayLines) - 1
	}

	if startRow == endRow {
		runes := []rune(displayLines[startRow])
		s := min(startCol, len(runes))
		e := min(endCol+1, len(runes))
		if s >= e {
			return ""
		}
		return strings.TrimRight(string(runes[s:e]), " ")
	}

	var parts []string
	first := []rune(displayLines[startRow])
	s := min(startCol, len(first))
	parts = append(parts, strings.TrimRight(string(first[s:]), " "))
	for r := startRow + 1; r < endRow; r++ {
		parts = append(parts, strings.TrimRight(displayLines[r], " "))
	}
	last := []rune(displayLines[endRow])
	e := min(endCol+1, len(last))
	parts = append(parts, strings.TrimRight(string(last[:e]), " "))

	return strings.Join(parts, "\n")
}
