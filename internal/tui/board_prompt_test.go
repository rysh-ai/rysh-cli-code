// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
)

// The board's input field (design 027 §5.2) — the human's door to the board
// claude.

func TestComposingIsEnteredDeliberately(t *testing.T) {
	st := board.New(0)
	m := buildBoardModel(st)

	snap := m.focusedPaneSnapshot()
	if vs := m.boardViewFor(snap.ID); vs.composing {
		t.Fatal("a board pane must not start with the input field focused")
	}

	if handled, _ := m.updateBoardPaneInput(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'i'}}); !handled {
		t.Fatal("i did not start composing")
	}
	if vs := m.boardViewFor(snap.ID); !vs.composing {
		t.Fatal("i did not set composing")
	}
}

func TestEscLeavesComposing(t *testing.T) {
	st := board.New(0)
	m := buildBoardModel(st)
	snap := m.focusedPaneSnapshot()

	m.updateBoardPaneInput(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'i'}})
	m.updateBoardPaneInput(tea.KeyMsg{Type: tea.KeyEsc})

	if vs := m.boardViewFor(snap.ID); vs.composing {
		t.Fatal("esc did not leave the input field")
	}
}

// THE TRAP TEST, restated for the composing case. TestBoardUserIsNeverTrapped
// covers a board that is not composing; this is the state the input field
// introduced, and it is the one where an always-focused field would eat the
// tab-switch keys and strand the user in a monitoring pane.
func TestAComposingBoardStillLetsTheUserOut(t *testing.T) {
	st := board.New(0)

	mustPassThrough := []tea.KeyMsg{
		{Type: tea.KeyCtrlO},
		{Type: tea.KeyCtrlP},
		{Type: tea.KeyCtrlT},
		{Type: tea.KeyCtrlW},
		{Type: tea.KeyLeft, Alt: true},
		{Type: tea.KeyRight, Alt: true},
	}
	for _, k := range mustPassThrough {
		m := buildBoardModel(st)
		m.updateBoardPaneInput(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'i'}})
		if handled, _ := m.updateBoardPaneInput(k); handled {
			t.Fatalf("a composing board swallowed %q — the user would be trapped in it", k.String())
		}
	}
}

func TestTypingReachesTheInputField(t *testing.T) {
	st := board.New(0)
	m := buildBoardModel(st)
	snap := m.focusedPaneSnapshot()

	m.updateBoardPaneInput(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'i'}})
	for _, r := range "stop all fleet" {
		m.updateBoardPaneInput(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}

	if got := m.inputs[snap.ID].Value(); got != "stop all fleet" {
		t.Fatalf("input field holds %q, want %q", got, "stop all fleet")
	}
}

// An empty enter must not send. The board claude is a bottleneck by design
// (ruling 2) and every line costs it a turn, so a stray keystroke must not
// spend one.
func TestSubmittingAnEmptyPromptSendsNothing(t *testing.T) {
	st := board.New(0)
	m := buildBoardModel(st)
	snap := m.focusedPaneSnapshot()

	if cmd := m.submitBoardPrompt(snap.ID); cmd != nil {
		t.Fatal("an empty prompt produced a send")
	}
}

// A refusal has to be visible. This is the failure this whole track is built
// around: the line disappears from the field and the human reads that as
// delivery.
func TestARefusalIsRenderedNotSwallowed(t *testing.T) {
	st := board.New(0)
	m := buildBoardModel(st)
	snap := m.focusedPaneSnapshot()

	m.applyBoardPromptResult(boardPromptResultMsg{
		paneID: snap.ID,
		ok:     false,
		detail: `no pane named "board" — the board claude is not running`,
	})

	line := m.boardStatusLine(m.boardViewFor(snap.ID))
	if !strings.Contains(line, "REFUSED") {
		t.Fatalf("a refusal rendered as %q — it must not read like a success", line)
	}
	if !strings.Contains(line, "board claude is not running") {
		t.Fatalf("the refusal lost its reason: %q", line)
	}
}

// When nothing has been sent yet the status line is where the bypass is
// advertised. A wedged board claude must not leave a human with no way to
// reach an agent (design 027 §5.4).
func TestTheBypassIsAdvertisedOnTheBoard(t *testing.T) {
	st := board.New(0)
	m := buildBoardModel(st)
	snap := m.focusedPaneSnapshot()

	line := m.boardStatusLine(m.boardViewFor(snap.ID))
	if !strings.Contains(line, "##ansa send") {
		t.Fatalf("the board never tells the human how to reach an agent directly: %q", line)
	}
}
