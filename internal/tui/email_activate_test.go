// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

// E17 M1 / design 020 §2.1 — sticky activation of the email view.
//
// ##humanoid register-output sends MsgPaneEnableMode and MsgPaneActivateMode
// back to back. The activate push can reach the TUI before the enable has
// propagated into the snapshot; applyActivateMode then stashes the request in
// pendingActivate and syncPaneInputs applies it on the first snapshot that
// reports the mode enabled (b218efb). Before these tests that path had its own
// fix commit and no test (design 020 §3).

// activateModel builds a Model with one pane and the per-pane maps the real
// constructor (NewModel) initialises.
func activateModel(pane domain.PaneSnapshot) Model {
	m := paneModel(pane)
	m.paneInputModes = map[string]string{}
	m.pendingActivate = map[string]string{}
	m.emailViews = map[string]*emailViewState{}
	return m
}

// setPane replaces the model's only pane, as a new snapshot tick would.
func setPane(m *Model, pane domain.PaneSnapshot) {
	m.snapshot.Tabs[0].Lanes[0].PaneGroups[0].Panes[0] = pane
}

// TestActivateEmailImmediateWhenEnabled: the enable already landed, so the
// pane flips to email at once, nothing is left pending, and the first inbox
// fetch is fired.
func TestActivateEmailImmediateWhenEnabled(t *testing.T) {
	pane := domain.PaneSnapshot{ID: "p-mail", RegisteredHumanoid: "mailbot", EnabledModes: []string{"shell", "email"}}
	m := activateModel(pane)

	cmd := m.applyActivateMode(activateModeItem{paneID: pane.ID, mode: "email"})

	if got := m.paneInputModes[pane.ID]; got != "email" {
		t.Fatalf("pane mode = %q, want \"email\" (mode was already enabled)", got)
	}
	if want, ok := m.pendingActivate[pane.ID]; ok {
		t.Fatalf("pendingActivate[%q] = %q, want no pending entry after an immediate flip", pane.ID, want)
	}
	if cmd == nil {
		t.Fatal("no inbox fetch fired on activation: the list would be empty when the pane shows")
	}
	if ev := m.emailViews[pane.ID]; ev == nil || ev.humanoid != "mailbot" {
		t.Fatalf("email view state = %+v, want one seeded with humanoid \"mailbot\"", ev)
	}
}

// TestActivateEmailStickyUntilModeEnabled is the race b218efb fixed: the
// activate push beats the enable. The request must survive snapshots that
// still lack the mode, and apply on the first one that has it.
func TestActivateEmailStickyUntilModeEnabled(t *testing.T) {
	pane := domain.PaneSnapshot{ID: "p-mail", RegisteredHumanoid: "mailbot", EnabledModes: []string{"shell"}}
	m := activateModel(pane)

	cmd := m.applyActivateMode(activateModeItem{paneID: pane.ID, mode: "email"})

	if got := m.paneInputModes[pane.ID]; got == "email" {
		t.Fatal("pane flipped to email before the mode was enabled")
	}
	if got := m.pendingActivate[pane.ID]; got != "email" {
		t.Fatalf("pendingActivate[%q] = %q, want \"email\": the early activate was dropped (the original nothing-shows-up bug)", pane.ID, got)
	}
	if cmd == nil {
		t.Fatal("deferred activation fired no inbox fetch: the fetch must not wait for the flip")
	}

	// A snapshot tick that still lacks the mode: stay pending, stay on shell.
	m.syncPaneInputs()
	if got := m.paneInputModes[pane.ID]; got != "shell" {
		t.Fatalf("after a tick without email enabled: mode = %q, want \"shell\"", got)
	}
	if got := m.pendingActivate[pane.ID]; got != "email" {
		t.Fatalf("after a tick without email enabled: pending = %q, want \"email\" kept", got)
	}

	// The enable propagates: the next tick applies the stashed activation.
	pane.EnabledModes = []string{"shell", "email"}
	setPane(&m, pane)
	m.syncPaneInputs()
	if got := m.paneInputModes[pane.ID]; got != "email" {
		t.Fatalf("after the enable propagated: mode = %q, want \"email\"", got)
	}
	if _, ok := m.pendingActivate[pane.ID]; ok {
		t.Fatal("pending activation not drained after it was applied")
	}

	// Later ticks leave the user's mode alone: the drain is one-shot.
	m.paneInputModes[pane.ID] = "shell" // user cycled away
	m.syncPaneInputs()
	if got := m.paneInputModes[pane.ID]; got != "shell" {
		t.Fatalf("a drained activation re-applied itself: mode = %q, want the user's \"shell\"", got)
	}
}

// TestActivateUnknownPaneIsIgnored: an activate push for a pane this front-end
// has never seen neither panics nor leaves a pending entry behind.
func TestActivateUnknownPaneIsIgnored(t *testing.T) {
	m := activateModel(domain.PaneSnapshot{ID: "p-other"})

	if cmd := m.applyActivateMode(activateModeItem{paneID: "p-missing", mode: "email"}); cmd != nil {
		t.Fatal("activation for an unknown pane fired a command")
	}
	if len(m.pendingActivate) != 0 {
		t.Fatalf("pendingActivate = %v, want empty for an unknown pane", m.pendingActivate)
	}
}

// TestPendingActivatePrunedWhenPaneVanishes: a pane that closes before its
// mode is ever enabled must not leave its stashed activation behind.
// syncPaneInputs prunes every other per-pane map for vanished panes; a
// pendingActivate entry was only ever deleted by being applied, so one that
// never applied lived for the rest of the session (the emailViews leak shape).
func TestPendingActivatePrunedWhenPaneVanishes(t *testing.T) {
	gone := domain.PaneSnapshot{ID: "p-gone", RegisteredHumanoid: "mailbot", EnabledModes: []string{"shell"}}
	m := activateModel(gone)
	m.applyActivateMode(activateModeItem{paneID: gone.ID, mode: "email"})
	if m.pendingActivate[gone.ID] != "email" {
		t.Fatalf("setup: pendingActivate = %v, want an entry for %q", m.pendingActivate, gone.ID)
	}

	// The pane closes before the enable ever propagated.
	setPane(&m, domain.PaneSnapshot{ID: "p-stay"})
	m.syncPaneInputs()

	if want, ok := m.pendingActivate[gone.ID]; ok {
		t.Fatalf("pendingActivate[%q] = %q survived a snapshot without that pane: stashed activations leak for the life of the session", gone.ID, want)
	}
}
