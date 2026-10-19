// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Design 051: a board pane lists STATUS messages by default, PROMPT messages
// (commands agents send each other, with their acks) on p, and everything on a.

func boardCmdPost(from, to, id, text string, ts int64) *msgpkg.MsgBoardPost {
	p := msgpkg.NewBoardPost(from, "p-"+from[:4], msgpkg.BoardKindCmd, text, ts)
	p.ToPaneID, p.ToPersona, p.CmdID = to, "p-"+to[:4], id
	p.ThreadID = msgpkg.CmdThreadID(from, id)
	return p
}

func boardViewModel(t *testing.T) Model {
	t.Helper()
	store := board.New(0)
	store.Apply(boardRootPost(boardPaneA, "wkr-a", "STATUS: parser done", 1, 100))
	store.Apply(boardCmdPost(boardPaneA, boardPaneB, "msg-0001", "PROMPT: build the codec", 200))
	ack := msgpkg.NewBoardPost(boardPaneB, "wkr-b", msgpkg.BoardKindAck, "ACK: taken", 300)
	ack.CmdID, ack.ThreadID = "msg-0001", msgpkg.CmdThreadID(boardPaneA, "msg-0001")
	store.Apply(ack)
	return buildBoardModel(store)
}

func boardViewText(m Model) string {
	pane := domain.PaneSnapshot{ID: boardPaneID, PaneType: domain.PaneTypeAgentsBoard}
	lay := m.boardPanelLayout(pane, 120, 30)
	return lay.header + "\n" + strings.Join(lay.rows, "\n")
}

func pressBoardKey(t *testing.T, m *Model, r rune) {
	t.Helper()
	if handled, _ := m.updateBoardPaneInput(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}); !handled {
		t.Fatalf("key %q was not claimed by the board pane", r)
	}
}

func TestBoardViewDefaultsToStatus(t *testing.T) {
	m := boardViewModel(t)
	got := boardViewText(m)
	if !strings.Contains(got, "STATUS: parser done") {
		t.Errorf("status view must list status posts:\n%s", got)
	}
	if strings.Contains(got, "PROMPT: build the codec") || strings.Contains(got, "ACK: taken") {
		t.Errorf("default (status) view must not list prompt threads:\n%s", got)
	}
	if !strings.Contains(got, "showing status · 1 status · 1 prompt · keys s p a") {
		t.Errorf("header must name the view and count both kinds:\n%s", got)
	}
}

func TestBoardViewKeysSwitch(t *testing.T) {
	m := boardViewModel(t)

	pressBoardKey(t, &m, 'p')
	got := boardViewText(m)
	if !strings.Contains(got, "PROMPT: build the codec") || !strings.Contains(got, "ACK: taken") {
		t.Errorf("prompt view must list the command AND its ack (the thread is the unit):\n%s", got)
	}
	if strings.Contains(got, "STATUS: parser done") {
		t.Errorf("prompt view must not list status posts:\n%s", got)
	}
	if !strings.Contains(got, "→ p-bbbb · msg-0001  [cmd]") || !strings.Contains(got, "wkr-b · msg-0001  [ack]") {
		t.Errorf("a prompt row must name its recipient and command id, an ack its command:\n%s", got)
	}

	pressBoardKey(t, &m, 'a')
	got = boardViewText(m)
	for _, want := range []string{"STATUS: parser done", "PROMPT: build the codec", "ACK: taken", "showing all"} {
		if !strings.Contains(got, want) {
			t.Errorf("all view missing %q:\n%s", want, got)
		}
	}

	pressBoardKey(t, &m, 's')
	if got = boardViewText(m); strings.Contains(got, "PROMPT: build the codec") {
		t.Errorf("s must return to the status view:\n%s", got)
	}
}

func TestBoardViewEmptyViewSaysSo(t *testing.T) {
	store := board.New(0)
	store.Apply(boardCmdPost(boardPaneA, boardPaneB, "msg-0001", "PROMPT only", 200))
	m := buildBoardModel(store)
	got := boardViewText(m)
	if !strings.Contains(got, "No status messages on this board yet") || !strings.Contains(got, "press a for all") {
		t.Errorf("an empty view must say the board has other traffic and how to see it:\n%s", got)
	}
}

// While composing a prompt for the board claude, s/p/a are LETTERS.
func TestBoardViewKeysAreLettersWhileComposing(t *testing.T) {
	m := boardViewModel(t)
	pressBoardKey(t, &m, 'i')
	for _, r := range "spa" {
		m.updateBoardPaneInput(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if v := m.boardViewFor(boardPaneID).boardView(); v != board.ViewStatus {
		t.Errorf("typing s/p/a while composing changed the view to %q", v)
	}
	if got := m.inputs[boardPaneID].Value(); got != "spa" {
		t.Errorf("composer = %q, want the letters typed", got)
	}
}
