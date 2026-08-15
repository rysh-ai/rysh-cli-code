// SPDX-License-Identifier: Apache-2.0

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
func (w *WorkspaceActor) openAgentsBoardPane(out *strings.Builder, curPane, boardID string) {
	boardID = msg.NormalizeBoardID(boardID)

	// THE SINGLETON IS PER BOARD, NOT PER SESSION (design 028). Re-opening the
	// same board reuses its pane; opening a different one opens a second pane,
	// because those are two views of two different things.
	//
	// The live snapshot is what decides, not the map alone: a pane closed by
	// hand leaves an id behind that would otherwise report a board as open
	// when nothing renders it.
	if existing := w.boardPaneFor(boardID); existing != "" {
		fmt.Fprintf(out, "board %s: already open in pane %s\n", boardLabel(boardID), shortID(existing))
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
		// BORN with its board id. Not a follow-up MsgPaneSetMeta: that races
		// the pane actor's own subscription, and a board pane that lost the
		// write would render the session board while claiming to be a fleet's.
		Meta: map[string]string{paneMetaBoardID: boardID},
	})
	w.resCounts.panes++
	if w.boardPanes == nil {
		w.boardPanes = map[string]string{}
	}
	w.boardPanes[boardID] = paneID
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
	w.announceExistingPanesToBoard(boardID)

	fmt.Fprintf(out, "board %s: opened agents-board in pane %s (%s)\n",
		boardLabel(boardID), shortID(paneID), alias)
}

// boardPaneFor returns the live pane rendering a board, or "".
//
// It CHECKS THE SNAPSHOT rather than trusting the map: a pane the human closed
// leaves its id behind, and a stale id reported as "already open" is a board
// nobody can reopen. The map is a cache of a fact the panes own — the same
// reasoning workspace.go gives for not persisting it.
func (w *WorkspaceActor) boardPaneFor(boardID string) string {
	boardID = msg.NormalizeBoardID(boardID)
	if id := w.boardPanes[boardID]; id != "" {
		// paneSnapshotByID, NOT findPaneSnapshot: the latter only searches the
		// ACTIVE tab, and design 028 §6.8 puts each fleet in a tab of its own.
		// A board pane one tab away would otherwise look closed, and `##board
		// open` would stack a duplicate view onto a board already on screen.
		if w.paneSnapshotByID(id) != nil {
			return id
		}
		delete(w.boardPanes, boardID)
	}
	// Re-learn from the panes themselves. After a daemon restart the map is
	// empty while the board panes are back with their meta intact, and without
	// this every `##board open` would stack a second pane onto a board that is
	// already on screen.
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
					if ps.PaneType != domain.PaneTypeAgentsBoard {
						continue
					}
					if msg.NormalizeBoardID(ps.Meta[paneMetaBoardID]) != boardID {
						continue
					}
					if w.boardPanes == nil {
						w.boardPanes = map[string]string{}
					}
					w.boardPanes[boardID] = ps.ID
					return ps.ID
				}
			}
		}
	}
	return ""
}

// announceExistingPanesToBoard publishes one registration per pane that could
// host an agent, on behalf of panes that started before anything was listening.
//
// Re-announcing is safe and idempotent: the store keys the roster by pane id and
// last announcement wins (board.Store.Register), so a pane that also announced
// itself is replaced, not duplicated.
// A pane is announced to ITS OWN board (design 028): the roster of a fleet's
// board is that fleet's panes, and seeding every board with every pane in the
// session would make each fleet's roster a session-wide list that happens to be
// rendered in several places.
func (w *WorkspaceActor) announceExistingPanesToBoard(boardID string) {
	boardID = msg.NormalizeBoardID(boardID)
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
					if msg.NormalizeBoardID(ps.Meta[paneMetaBoardID]) != boardID {
						continue
					}
					// Through BoardPersona, never the raw given-name — the same
					// rule the post path and the pane producer use.
					_ = msg.SendBoardRegister(w.pub, boardID, msg.NewBoardRegister(
						ps.ID, msg.BoardPersona(ps.GivenName, ps.Title, ps.ID), now))
				}
			}
		}
	}
}
