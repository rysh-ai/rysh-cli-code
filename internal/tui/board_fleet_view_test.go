// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
	"github.com/rysh-ai/rysh-cli-code/internal/fleetboard"
	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// The fleet board in the board view (design 030 §4.3/§9.2, wave W4): a
// read-only dashboard, one row per fleet, opened by default; the raw post
// stream stays one key away.

func buildFleetBoardModel(t *testing.T) Model {
	t.Helper()
	m := buildBoardModel(board.New(0))
	p := &m.snapshot.Tabs[0].Lanes[0].PaneGroups[0].Panes[0]
	p.Meta = map[string]string{boardIDMetaKey: msgpkg.FleetBoardID}
	m.boards[msgpkg.FleetBoardID] = board.New(0)
	now := time.Now().UnixMilli()
	m.fleetBoard = &fleetboard.Reply{Now: now, Rows: []fleetboard.ReplyRow{{
		Row:    fleetboard.Row{Fleet: "epic-a", State: "up", Members: 3, LastActivity: now},
		Health: "active", Summary: "MILESTONE: M1 shipped",
	}}}
	return m
}

func fbKey(r rune) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}} }

// The W4 headline (design §9.5): the reserved board's view enters composing
// on `i` today. It must not; it says the board is read-only instead.
func TestW4TheFleetBoardIsReadOnly(t *testing.T) {
	m := buildFleetBoardModel(t)
	snap := m.focusedPaneSnapshot()
	for _, k := range []tea.KeyMsg{fbKey('i'), {Type: tea.KeyEnter}} {
		handled, _ := m.updateBoardPaneInput(k)
		st := m.boardViewFor(snap.ID)
		if !handled || st.composing {
			t.Fatalf("%q: handled=%v composing=%v; want handled and not composing", k.String(), handled, st.composing)
		}
		if !strings.Contains(st.sendStatus, "read-only") {
			t.Fatalf("%q: status %q does not say the fleet board is read-only", k.String(), st.sendStatus)
		}
	}
}

func layoutText(m Model) string {
	snap := m.focusedPaneSnapshot()
	lay := m.boardPanelLayout(*snap, 110, 30)
	return lay.header + "\n" + strings.Join(lay.rows, "\n") + "\n" + lay.status
}

func TestW4TheFleetBoardOpensOnTheDashboard(t *testing.T) {
	m := buildFleetBoardModel(t)
	out := layoutText(m)
	for _, want := range []string{"FLEET BOARD", "read-only", "epic-a", "active", "MILESTONE: M1 shipped"} {
		if !strings.Contains(out, want) {
			t.Errorf("dashboard lacks %q:\n%s", want, out)
		}
	}

	// s shows the raw stream (the audit view); d brings the dashboard back.
	m.updateBoardPaneInput(fbKey('s'))
	if out := layoutText(m); strings.Contains(out, "MILESTONE: M1 shipped") {
		t.Errorf("after s the dashboard is still drawn:\n%s", out)
	}
	if handled, _ := m.updateBoardPaneInput(fbKey('d')); !handled {
		t.Fatal("d was not handled on the fleet board")
	}
	if out := layoutText(m); !strings.Contains(out, "MILESTONE: M1 shipped") {
		t.Errorf("after d the dashboard is not drawn:\n%s", out)
	}
}

func TestW4AnEmptyDashboardSaysWhy(t *testing.T) {
	m := buildFleetBoardModel(t)
	m.fleetBoard = nil
	if out := layoutText(m); !strings.Contains(out, "no fleet rows yet") {
		t.Errorf("an empty dashboard does not explain itself:\n%s", out)
	}
}

// Other boards are unchanged: i composes, d is not a key there.
func TestW4OtherBoardsAreUnchanged(t *testing.T) {
	m := buildBoardModel(board.New(0))
	if handled, _ := m.updateBoardPaneInput(fbKey('d')); handled {
		t.Error("d was claimed on an ordinary board")
	}
	m.updateBoardPaneInput(fbKey('i'))
	if !m.boardViewFor(m.focusedPaneSnapshot().ID).composing {
		t.Error("i no longer composes on an ordinary board")
	}
}

func TestW4KeyHelpOnTheFleetBoard(t *testing.T) {
	m := buildFleetBoardModel(t)
	help := m.boardKeyHelp()
	if !strings.Contains(help, "d dashboard") || !strings.Contains(help, "read-only") {
		t.Errorf("fleet board key help: %q", help)
	}
	if strings.Contains(help, "i write to the board claude") {
		t.Errorf("fleet board key help still offers the composer: %q", help)
	}
}

// Unverified fleet-board posts are dimmed when drawn (W4); only those rows.
func TestW4UnverifiedRowsAreDimmed(t *testing.T) {
	if !boardRowIsUnverified(boardRowRef{post: &msgpkg.MsgBoardPost{Unverified: true}}) {
		t.Error("an unverified post's row is not dimmed")
	}
	if boardRowIsUnverified(boardRowRef{post: &msgpkg.MsgBoardPost{}}) || boardRowIsUnverified(boardRowRef{}) {
		t.Error("a verified post's row, or a blank row, is dimmed")
	}
}
