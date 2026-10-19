// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rysh-ai/rysh-cli-code/internal/bus"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// F-71: a listener command blocks on a shared channel; every one returned
// that nothing consumes is a goroutine parked for the rest of the session,
// and a consumed one that is never re-armed stops the feed for good.
// Pre-loading the channel makes each returned listener return at once, so the
// messages a command produces count the listeners it armed.

func countMsgs[T any](t *testing.T, cmd tea.Cmd) int {
	t.Helper()
	n := 0
	for _, msg := range runCmd(t, cmd) {
		if _, ok := msg.(T); ok {
			n++
		}
	}
	return n
}

func TestApprovalForAPaneOutsideTheSnapshotReArmsTheListener(t *testing.T) {
	m := Model{bus: &bus.Bus{}, approvalCh: make(chan approvalRequestMsg, 4)}
	m.approvalCh <- approvalRequestMsg{}

	_, cmd := m.Update(approvalRequestMsg{PaneID: "humanoid-x", Request: &msgpkg.MsgApprovalRequest{RequestID: "r"}})
	if got := countMsgs[approvalRequestMsg](t, cmd); got != 1 {
		t.Fatalf("armed %d approval listeners, want 1 — with none, no approval ever reaches this TUI again", got)
	}
}

func TestApprovalForASnapshotPaneWaitsForTheAnswer(t *testing.T) {
	m := Model{
		bus:        &bus.Bus{},
		approvalCh: make(chan approvalRequestMsg, 4),
		snapshot: domain.WorkspaceSnapshot{Tabs: []domain.TabSnapshot{{ID: "t", Lanes: []domain.LaneSnapshot{{
			ID: "l", PaneGroups: []domain.PaneGroupSnapshot{{ID: "g", Panes: []domain.PaneSnapshot{{ID: "p"}}}},
		}}}}},
	}
	m.approvalCh <- approvalRequestMsg{}

	_, cmd := m.Update(approvalRequestMsg{PaneID: "p", Request: &msgpkg.MsgApprovalRequest{RequestID: "r"}})
	if got := countMsgs[approvalRequestMsg](t, cmd); got != 0 {
		t.Fatalf("armed %d listeners while an approval is open; the answer re-arms", got)
	}
}

func TestClosingTheLLMPickerDoesNotArmASecondListener(t *testing.T) {
	// The open handler already re-armed; every close path armed another.
	m := Model{bus: &bus.Bus{}, llmPickerCh: make(chan *msgpkg.MsgLLMPickerOpen, 4)}
	m.llmPickerCh <- nil
	m.llmPicker = &llmPickerState{step: pickerStepSecret}

	_, cmd := m.updateLLMPickerSecret(tea.KeyMsg{Type: tea.KeyEsc})
	if got := countMsgs[llmPickerOpenMsg](t, cmd); got != 0 {
		t.Fatalf("closing the picker armed %d extra listeners, want 0", got)
	}
}
