// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Two boards on one screen (design 028, `E-39`).
//
// The view is where the promise is finally kept or broken: a session may record
// twenty-five boards correctly and still be useless if two panes render the same
// stream, or if a pane renders a stream without saying whose it is.

// buildTwoBoardModel puts two agents-board panes side by side in one lane, one
// bound to epic-07 and one to epic-08 through the `board.id` meta the workspace
// writes at pane birth.
func buildTwoBoardModel() Model {
	pane := func(id, boardID string) domain.PaneSnapshot {
		p := domain.PaneSnapshot{ID: id, Title: "agents-board", PaneType: domain.PaneTypeAgentsBoard, Flex: 1}
		if boardID != "" {
			p.Meta = map[string]string{boardIDMetaKey: boardID}
		}
		return p
	}
	panes := []domain.PaneSnapshot{
		pane("board-07", "epic-07"),
		pane("board-08", "epic-08"),
		pane("board-session", ""),
	}
	tab := domain.TabSnapshot{
		ID: "tab-1", Title: "t1", ActivePaneID: "board-07",
		Lanes: []domain.LaneSnapshot{{
			ID: "lane-1", Flex: 1, ActivePaneID: "board-07",
			PaneGroups: []domain.PaneGroupSnapshot{
				{ID: "g-1", RowFlex: 1, ActivePaneID: "board-07", Panes: panes},
			},
		}},
	}
	return Model{
		snapshot: domain.WorkspaceSnapshot{
			Tabs: []domain.TabSnapshot{tab}, ActiveTabID: "tab-1", ActivePaneID: "board-07",
		},
		mode:       modeNormal,
		width:      120,
		height:     40,
		boards:     map[string]*board.Store{},
		boardViews: map[string]*boardViewState{},
	}
}

func multiPost(paneID, persona, text string, ts int64) *msgpkg.MsgBoardPost {
	return msgpkg.NewBoardPost(paneID, persona, msgpkg.BoardKindMilestone, text, ts)
}

// TestEachBoardPaneRendersItsOwnStream is the acceptance criterion for the view
// half of E-39.
func TestEachBoardPaneRendersItsOwnStream(t *testing.T) {
	m := buildTwoBoardModel()

	m.applyBoardEvent(board.Event{Kind: board.EventPost, Board: "epic-07",
		Post: multiPost(boardPaneA, "wkr-07", "seven shipped", 100)})
	m.applyBoardEvent(board.Event{Kind: board.EventPost, Board: "epic-08",
		Post: multiPost(boardPaneB, "wkr-08", "eight shipped", 101)})
	m.applyBoardEvent(board.Event{Kind: board.EventPost, Board: msgpkg.DefaultBoardID,
		Post: multiPost(boardPaneA, "human", "session note", 102)})

	seven := strings.Join(m.boardRows("epic-07", 100), "\n")
	eight := strings.Join(m.boardRows("epic-08", 100), "\n")
	session := strings.Join(m.boardRows(msgpkg.DefaultBoardID, 100), "\n")

	if !strings.Contains(seven, "seven shipped") {
		t.Errorf("epic-07 did not render its own post:\n%s", seven)
	}
	for _, foreign := range []string{"eight shipped", "session note"} {
		if strings.Contains(seven, foreign) {
			t.Errorf("epic-07 rendered %q, which was posted to another board:\n%s", foreign, seven)
		}
	}
	if !strings.Contains(eight, "eight shipped") || strings.Contains(eight, "seven shipped") {
		t.Errorf("epic-08 rendered the wrong stream:\n%s", eight)
	}
	if !strings.Contains(session, "session note") || strings.Contains(session, "shipped") {
		t.Errorf("the session board rendered the wrong stream:\n%s", session)
	}
}

// TestABoardPaneResolvesItsBoardFromMeta — the binding is on the pane, and a
// pane without it reads the session board. That fallback is what keeps every
// board pane created before 028 working after a restart.
func TestABoardPaneResolvesItsBoardFromMeta(t *testing.T) {
	m := buildTwoBoardModel()

	for _, c := range []struct{ pane, want string }{
		{"board-07", "epic-07"},
		{"board-08", "epic-08"},
		{"board-session", msgpkg.DefaultBoardID},
		{"no-such-pane", msgpkg.DefaultBoardID},
	} {
		if got := m.boardIDForPane(c.pane); got != c.want {
			t.Errorf("boardIDForPane(%q) = %q, want %q", c.pane, got, c.want)
		}
	}
}

// TestTheBoardHeaderNamesItsBoard. With one board "AGENTS-BOARD" was enough;
// with several, an unlabelled pane is a screenshot nobody can act on, and two
// boards side by side are indistinguishable.
func TestTheBoardHeaderNamesItsBoard(t *testing.T) {
	m := buildTwoBoardModel()
	st := &boardViewState{}

	if got := m.boardHeaderText("epic-07", st); !strings.Contains(got, "epic-07") {
		t.Errorf("header = %q, want it to name epic-07", got)
	}
	if got := m.boardHeaderText(msgpkg.DefaultBoardID, st); !strings.Contains(got, "session") {
		t.Errorf("header = %q, want it to name the session board", got)
	}
}

// TestRosterIsSeededPerBoard. A fleet's roster is that fleet's panes. Seeding
// every board from every pane would give each fleet a roster of the whole
// session — the "looks authoritative and is wrong" failure the roster rules
// already exist to prevent.
func TestRosterIsSeededPerBoard(t *testing.T) {
	m := buildTwoBoardModel()
	// Two agent panes, one on each board, alongside the board panes themselves.
	agents := []domain.PaneSnapshot{
		{ID: "a7", GivenName: "wkr-07", PaneType: domain.PaneTypeNormal,
			Meta: map[string]string{boardIDMetaKey: "epic-07"}},
		{ID: "a8", GivenName: "wkr-08", PaneType: domain.PaneTypeNormal,
			Meta: map[string]string{boardIDMetaKey: "epic-08"}},
		{ID: "ah", GivenName: "human", PaneType: domain.PaneTypeNormal},
	}
	groups := m.snapshot.Tabs[0].Lanes[0].PaneGroups
	groups = append(groups, domain.PaneGroupSnapshot{ID: "g-2", RowFlex: 1, Panes: agents})
	m.snapshot.Tabs[0].Lanes[0].PaneGroups = groups

	m.seedRosterFromSnapshot()

	for _, c := range []struct {
		board string
		want  string
	}{{"epic-07", "a7"}, {"epic-08", "a8"}, {msgpkg.DefaultBoardID, "ah"}} {
		roster := m.boardStore(c.board).Roster()
		if len(roster) != 1 {
			t.Fatalf("board %q roster = %v, want exactly its own pane", c.board, roster)
		}
		if roster[0].PaneID != c.want {
			t.Errorf("board %q roster holds %q, want %q", c.board, roster[0].PaneID, c.want)
		}
	}
}

// TestBoardMetaKeyMatchesTheWorkspace.
//
// internal/tui must not import internal/actors, so the meta key is spelled
// twice. That is the seam this pins: the workspace writes `board.id` at pane
// birth and the view reads it, and a rename on one side alone would leave every
// board pane silently rendering the session board.
func TestBoardMetaKeyMatchesTheWorkspace(t *testing.T) {
	if boardIDMetaKey != "board.id" {
		t.Fatalf("boardIDMetaKey = %q; the workspace writes \"board.id\" "+
			"(actors.paneMetaBoardID) and the two must agree", boardIDMetaKey)
	}
}
