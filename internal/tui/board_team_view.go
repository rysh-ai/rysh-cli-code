// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"fmt"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

// boardTeamLayout is boardPanelLayout for the team view (key t): the fleet
// status table — who is progressing, idle or blocked — and the dependency tree
// under it, always drawn. It is a status screen, not a stream, so t opens it at
// the top rather than pinned to its last row.
func (m Model) boardTeamLayout(pane domain.PaneSnapshot, boardID string, st *boardViewState, contentWidth, maxLines int) boardLayout {
	f := m.boardFlow(boardID)
	// The tree may take what the table leaves, but never less than a few
	// rows: its root line is the answer to "who is the bottleneck".
	depRows := max(3, maxLines-len(board.TeamRows(f))-4)
	lines := board.RenderTeam(f, time.Now().UnixMilli(), depRows)
	for i, l := range lines {
		lines[i] = sanitiseBoardInline(l)
	}

	st.lastRows = len(lines)
	st.lastVisible = maxLines
	st.scrollOffset = clampBoardScroll(st.scrollOffset, st)
	// Same offsets as every view (0 = the last row, up = toward the top);
	// the t key opens it at the top, where the table starts.
	start := maxBoardScroll(len(lines), maxLines) - st.scrollOffset
	end := min(len(lines), start+maxLines)

	lay := boardLayout{rows: make([]string, 0, maxLines)}
	for i := start; i < end; i++ {
		lay.rows = append(lay.rows, truncBoardLine(lines[i], contentWidth))
	}
	for len(lay.rows) < maxLines {
		lay.rows = append(lay.rows, "")
	}
	lay.refs = make([]boardRowRef, len(lay.rows))
	lay.all = lay.rows
	lay.allRefs = lay.refs
	lay.header = truncBoardLine(m.boardHeaderText(boardID, st), contentWidth)
	lay.input = truncBoardLine(m.boardInputLine(pane.ID), contentWidth)
	lay.status = truncBoardLine(m.boardStatusLine(st), contentWidth)
	return lay
}

// boardTeamHeader is the view part of the header line in team view.
func boardTeamHeader(f board.Flow) string {
	members, blocked := board.TeamCounts(f)
	b := "none blocked"
	if blocked > 0 {
		b = fmt.Sprintf("%d blocked", blocked)
	}
	return fmt.Sprintf("showing team · %s · %s · keys s p a f t", plural(members, "member"), b)
}
