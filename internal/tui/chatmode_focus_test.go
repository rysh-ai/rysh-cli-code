// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"testing"

	"github.com/charmbracelet/bubbles/textinput"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

// twoPaneModel builds a Model with two panes (in two lanes) and a chosen active
// pane, mirroring the minimal structure paneModel builds for one pane.
func twoPaneModel(active string, a, b domain.PaneSnapshot) Model {
	m := Model{
		width:  120,
		height: 40,
		mode:   modeNormal,
		snapshot: domain.WorkspaceSnapshot{
			ActivePaneID: active,
			Tabs: []domain.TabSnapshot{{
				ID: "t1",
				Lanes: []domain.LaneSnapshot{
					{ID: "l1", PaneGroups: []domain.PaneGroupSnapshot{{ID: "g1", ActivePaneID: a.ID, Panes: []domain.PaneSnapshot{a}}}},
					{ID: "l2", PaneGroups: []domain.PaneGroupSnapshot{{ID: "g2", ActivePaneID: b.ID, Panes: []domain.PaneSnapshot{b}}}},
				},
			}},
		},
		inputs:         map[string]textinput.Model{},
		paneInputModes: map[string]string{},
	}
	return m
}

// defaultModes is the real per-pane default enabled set (see actors/pane.go).
var defaultModes = []string{"shell", "prompt", "rysh", "chat"}

// TestChatModePersistsAcrossFocus reproduces the reported bug: a pane in chat
// mode must keep rendering its chat conversation — and keep its input mode —
// when another pane (here a raw/interactive "claude" pane) is focused.
func TestChatModePersistsAcrossFocus(t *testing.T) {
	paneA := domain.PaneSnapshot{
		ID:           "A",
		EnabledModes: defaultModes,
		Output:       "shell merged output for A",
		ChatOutput:   "you: hi\nagent: hello there",
	}
	// Pane B runs `claude`: an interactive alt-screen program → RawMode.
	paneB := domain.PaneSnapshot{
		ID:           "B",
		EnabledModes: defaultModes,
		RawMode:      true,
		Output:       "$ claude",
	}

	// Pane A starts active and in chat mode.
	m := twoPaneModel("A", paneA, paneB)
	m.paneInputModes["A"] = "chat"
	m.paneInputModes["B"] = "shell"
	m.syncPaneInputs() // runs the enabled-modes reconciliation

	if got := m.paneInputModeFor("A"); got != "chat" {
		t.Fatalf("precondition: A should be chat, got %q", got)
	}

	// User clicks pane B → focus moves to B (the raw claude pane).
	m.snapshot.ActivePaneID = "B"
	m.snapshot.Tabs[0].Lanes[1].PaneGroups[0].ActivePaneID = "B"
	m.syncPaneInputs()

	// The input MODE of A must survive the focus change.
	if got := m.paneInputModeFor("A"); got != "chat" {
		t.Errorf("after focusing B, A's input mode = %q, want chat", got)
	}

	// The CONTENT of the now-inactive chat pane A must still be its chat
	// conversation, NOT the merged shell Output (the actual visible symptom).
	pa := m.findPaneInSnapshot("A")
	if pa == nil {
		t.Fatal("pane A missing from snapshot")
	}
	if got := m.paneOutputForMode(pa); got != paneA.ChatOutput {
		t.Errorf("inactive chat pane A renders %q, want chat output %q", got, paneA.ChatOutput)
	}

	// The newly-active raw pane B renders its own shell output.
	pb := m.findPaneInSnapshot("B")
	if got := m.paneOutputForMode(pb); got != paneB.Output {
		t.Errorf("active pane B renders %q, want %q", got, paneB.Output)
	}

	// Click back on A → focus returns; mode and content still chat.
	m.snapshot.ActivePaneID = "A"
	m.snapshot.Tabs[0].Lanes[1].PaneGroups[0].ActivePaneID = "B"
	m.syncPaneInputs()
	if got := m.paneInputModeFor("A"); got != "chat" {
		t.Errorf("after refocusing A, mode = %q, want chat", got)
	}
	pa = m.findPaneInSnapshot("A")
	if got := m.paneOutputForMode(pa); got != paneA.ChatOutput {
		t.Errorf("refocused chat pane A renders %q, want chat output", got)
	}
}

// TestInactiveModesRenderOwnBuffer checks prompt and rysh panes are also stable
// across focus, not just chat.
func TestInactiveModesRenderOwnBuffer(t *testing.T) {
	a := domain.PaneSnapshot{ID: "A", EnabledModes: defaultModes, Output: "merged-A", AIOutput: "ai-answer-A"}
	b := domain.PaneSnapshot{ID: "B", EnabledModes: defaultModes, Output: "merged-B", RyshOutput: "rysh-out-B"}

	m := twoPaneModel("B", a, b) // B is active
	m.paneInputModes["A"] = "prompt"
	m.paneInputModes["B"] = "rysh"
	m.syncPaneInputs()

	pa := m.findPaneInSnapshot("A")
	if got := m.paneOutputForMode(pa); got != a.AIOutput {
		t.Errorf("inactive prompt pane A = %q, want %q", got, a.AIOutput)
	}
	pb := m.findPaneInSnapshot("B")
	if got := m.paneOutputForMode(pb); got != b.RyshOutput {
		t.Errorf("active rysh pane B = %q, want %q", got, b.RyshOutput)
	}
}
