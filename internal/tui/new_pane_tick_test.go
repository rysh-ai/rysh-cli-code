// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rysh-ai/rysh-cli-code/internal/bus"
)

// Creating a pane (Ctrl+N, prefix `n`) schedules a follow-up refresh 100ms
// later. It used to deliver that as a tickMsg — and the tickMsg handler
// re-arms itself every 250ms, forever. So each pane ever created added one
// more permanent heartbeat chain: a TUI that had created N panes repainted
// 4×(N+1) times a second, and a session kept for weeks grew slower with every
// pane it had ever opened, even after closing them.
func TestNewPaneRefreshDoesNotStartATickChain(t *testing.T) {
	m := Model{bus: &bus.Bus{}}

	for _, msg := range runCmd(t, m.newPaneTickCmd()) {
		if _, isTick := msg.(tickMsg); isTick {
			t.Fatal("the new-pane follow-up refresh was delivered as a tickMsg, which re-arms " +
				"a permanent 250ms chain — one extra chain per pane ever created")
		}
	}

	// The one-shot message it does send must not re-arm a tick either.
	_, cmd := m.Update(newPaneRefreshMsg{})
	for _, msg := range runCmd(t, cmd) {
		if _, isTick := msg.(tickMsg); isTick {
			t.Fatal("handling newPaneRefreshMsg scheduled a tickMsg")
		}
	}
}

// runCmd executes cmd, flattening batches, and returns every message produced.
func runCmd(t *testing.T, cmd tea.Cmd) []tea.Msg {
	t.Helper()
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			out = append(out, runCmd(t, c)...)
		}
		return out
	}
	return []tea.Msg{msg}
}
