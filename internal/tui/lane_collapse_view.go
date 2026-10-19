// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// collapsedLaneWidth is a folded lane's lipgloss Width: one column of content
// between its two border columns, so a strip is 3 cells wide on screen — a
// line, with the lane's number and name running down it. laneWidths gives it
// exactly this and shares everything else among the open lanes.
const collapsedLaneWidth = 1

// laneStrip is the screen geometry of one folded lane, for the mouse: a click
// anywhere on it opens the lane (recomputePaneRects fills these beside
// paneRects; a folded lane has no pane rects).
type laneStrip struct {
	laneID string
	x, w   int // screen columns [x, x+w)
	y, h   int // screen rows [y, y+h), borders included
}

// renderCollapsedLane draws lane idx (0-based) as a strip contentHeight rows
// tall plus its two border rows — the same outer height as an open column, so
// JoinHorizontal keeps every column aligned.
//
// The border takes the focus colour when the active pane lives in this lane
// (focus can land in a folded lane by id, e.g. ##focus), and the attention
// colour while any pane in it is calling for attention — a folded lane must
// still be able to say "look at me".
func (m Model) renderCollapsedLane(lane domain.LaneRenderInfo, idx, contentHeight int) string {
	contentHeight = max(1, contentHeight)
	active := false
	attn := false
	var attnPriority msgpkg.AttentionPriority
	for _, p := range lane.VisiblePanes {
		if p.ID == m.snapshot.ActivePaneID {
			active = true
		}
		if a, ok := m.attentionState[p.ID]; ok && a.Count > 0 {
			attn = true
			attnPriority = a.Priority
		}
	}

	border := lipgloss.Color("240")
	text := lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	switch {
	case attn && m.attentionBlink:
		border = m.attentionBorderColor(attnPriority)
	case active:
		border = m.activePaneBorderColor()
		text = text.Foreground(m.activePaneBorderColor()).Bold(true)
	}

	rows := verticalLabel(collapsedLaneLabel(lane, idx), contentHeight)
	for i, r := range rows {
		rows[i] = text.Render(r)
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(border).
		Width(collapsedLaneWidth).
		Height(contentHeight).
		Render(strings.Join(rows, "\n"))
}

// collapsedLaneLabel is what runs down a strip: the lane's 1-based position
// (what `##lane expand <n>` takes) and its name.
func collapsedLaneLabel(lane domain.LaneRenderInfo, idx int) string {
	name := strings.TrimSpace(lane.Name)
	if name == "" {
		return fmt.Sprintf("%d", idx+1)
	}
	return fmt.Sprintf("%d %s", idx+1, name)
}

// verticalLabel lays s out one cell per row, at most height rows; a label
// that does not fit ends in "…". Runes wider than one cell (CJK, most emoji)
// and control characters are dropped: the strip is one column wide and a
// two-cell rune would push its right border out of line.
func verticalLabel(s string, height int) []string {
	var cells []string
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue
		}
		c := string(r)
		if ansi.StringWidth(c) != 1 {
			continue
		}
		cells = append(cells, c)
	}
	if len(cells) > height {
		cells = append(cells[:max(0, height-1)], "…")
	}
	return cells
}

// laneStripAt returns the folded lane under screen cell (x, y), or "".
func (m Model) laneStripAt(x, y int) string {
	for _, s := range m.laneStrips {
		if x >= s.x && x < s.x+s.w && y >= s.y && y < s.y+s.h {
			return s.laneID
		}
	}
	return ""
}

// laneFocusTarget is the pane to focus when a folded lane opens from a click:
// the lane's expanded (front) pane of its first stack, else its first pane.
func laneFocusTarget(lane domain.LaneRenderInfo) string {
	for _, p := range lane.VisiblePanes {
		if !p.StackCollapsed {
			return p.ID
		}
	}
	if len(lane.VisiblePanes) > 0 {
		return lane.VisiblePanes[0].ID
	}
	return ""
}
