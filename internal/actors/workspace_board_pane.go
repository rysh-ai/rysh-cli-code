package actors

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// agents-board pane creation (design 025).
//
// A founder ruling made the board a PaneType rather than an input mode, so
// opening it means creating a pane — modelled on startReplayPane
// (workspace_replay.go), the other shell-less pane that renders published
// content instead of a PTY.

// openAgentsBoardPane creates the read-only agents-board pane: a new pane group
// at the bottom of the invoking pane's lane, PaneType "agents-board", no shell
// ever started (domain.IsShelllessPaneType).
//
// Focus stays on the invoking pane, like every other pane-creating "##"
// command. That is not a stylistic choice here: the board exists to be watched
// while agents work, and yanking focus to it is the same defect the agent
// posting path was built to avoid (design 025 §4.1 hazard 2).
//
// Opening twice reuses the existing pane rather than stacking duplicates — a
// board is a singleton view of one session, and a second one would silently
// halve the room each gets on screen.
func (w *WorkspaceActor) openAgentsBoardPane(out *strings.Builder, curPane string) {
	if w.boardPaneID != "" && w.findPaneSnapshot(w.boardPaneID) != nil {
		fmt.Fprintf(out, "board: already open in pane %s\n", shortID(w.boardPaneID))
		return
	}

	tab := w.resolveOriginTab(curPane)
	if tab == nil {
		w.failRysh("board: no active tab")
		fmt.Fprintf(out, "board: no active tab\n")
		return
	}
	laneID := w.resolveLaneInTab(tab, "")
	if laneID == "" {
		w.failRysh("board: no active lane")
		fmt.Fprintf(out, "board: no active lane\n")
		return
	}
	if err := w.checkLimits(1); err != nil {
		w.failRysh("%v", err)
		fmt.Fprintf(out, "board: %v\n", err)
		return
	}

	alias := w.generateUniqueAlias()
	paneID := uuid.NewString()
	groupID := uuid.NewString()
	_ = w.pub.Send(msg.T("tab", tab.id, "inbox"), &msg.MsgTabCreatePaneGroupInLane{
		LaneID:   laneID,
		Title:    alias,
		GroupID:  groupID,
		PaneID:   paneID,
		PaneType: domain.PaneTypeAgentsBoard,
	})
	w.resCounts.panes++
	w.boardPaneID = paneID
	w.restoreFocusAfterCreate(curPane)
	w.persistToKV()

	// Seed the roster with the panes that already exist. Without this the
	// roster is systematically WRONG rather than merely incomplete, and it is
	// worth being precise about why: the only board subscriber lives in the TUI
	// (tui/board_view.go), so a registration published while no board is
	// attached is heard by nobody and persisted by nobody. Panes announce
	// themselves at startup — which is almost always BEFORE any board exists.
	// Pane-startup registration therefore covers only panes born after the
	// board opened; this covers the ones born before it. Together they are the
	// whole roster.
	//
	// A roster that silently undercounts is worse than no roster, by the same
	// argument that a stale name is worse than no name: it looks authoritative.
	w.announceExistingPanesToBoard()

	fmt.Fprintf(out, "board: opened agents-board in pane %s (%s)\n", shortID(paneID), alias)
}

// announceExistingPanesToBoard publishes one registration per pane that could
// host an agent, on behalf of panes that started before anything was listening.
//
// Re-announcing is safe and idempotent: the store keys the roster by pane id and
// last announcement wins (board.Store.Register), so a pane that also announced
// itself is replaced, not duplicated.
func (w *WorkspaceActor) announceExistingPanesToBoard() {
	now := time.Now().UnixMilli()
	for _, t := range w.tabs {
		if t == nil {
			continue
		}
		ts := w.queryTabSnapshot(t.id)
		if ts == nil {
			continue
		}
		for _, lane := range ts.Lanes {
			for _, g := range lane.PaneGroups {
				for i := range g.Panes {
					ps := &g.Panes[i]
					if !paneCanHostAnAgent(ps.PaneType) {
						continue
					}
					// Through BoardPersona, never the raw given-name — the same
					// rule the post path and the pane producer use.
					_ = msg.SendBoardRegister(w.pub, msg.NewBoardRegister(
						ps.ID, msg.BoardPersona(ps.GivenName, ps.Title, ps.ID), now))
				}
			}
		}
	}
}
