// SPDX-License-Identifier: Apache-2.0

package tui

// Regression tests for the full-terminal relay escalation gate in syncRawMode.
//
// Reported bug: in a fresh single-pane session, running a plain command
// (`cat <file>`) made the pane RawMode — computeInteractive() reports ANY
// foreground child as interactive so the TUI can render the live VT screen
// in-pane — and syncRawMode escalated the sole pane to the full-terminal
// PTYRelay. The relay entered the real alt screen and wrote the raw PTY burst
// straight to os.Stdout over the TUI; because rawReadLoop's narrow interactive
// flag (alt screen / hidden cursor) never rose for `cat`, relay.exit was never
// published and the terminal was left obscured and unpredictable.
//
// The gate: only a pane with a genuine full-screen signal (snapshot FullScreen,
// i.e. alt screen or hidden cursor) may take over the real terminal.

import (
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

// escModel builds a single non-stacked active pane in modeNormal / shell input
// mode — the exact fresh-session repro condition for the escalation decision.
func escModel(t *testing.T, pane domain.PaneSnapshot) Model {
	t.Helper()
	b := startTestBus(t)
	m := paneModel(pane)
	m.snapshot.ActiveTabID = "t1" // activeTab() must resolve for the sole-pane check
	m.mode = modeNormal
	m.bus = b
	m.pub = b.Publisher()
	m.syncPaneInputs()
	return m
}

// TestPlainCommandNeverEscalatesToRelay: RawMode without FullScreen (a plain
// foreground command such as cat/ls/make) must stay on in-pane VT rendering —
// no relay cmd, no relayPaneID — even as the sole non-stacked pane.
func TestPlainCommandNeverEscalatesToRelay(t *testing.T) {
	m := escModel(t, domain.PaneSnapshot{ID: "p1", RawMode: true}) // cat: no alt screen, cursor visible
	cmd := m.syncRawMode()
	if cmd != nil {
		t.Fatal("plain foreground command must not start the full-terminal relay")
	}
	if m.relayPaneID != "" {
		t.Errorf("relayPaneID must stay empty for a plain command, got %q", m.relayPaneID)
	}
	if m.fullscreenPaneID != "" {
		t.Errorf("fullscreenPaneID must stay empty for a plain command, got %q", m.fullscreenPaneID)
	}
	if m.mode != modeRaw {
		t.Errorf("plain command should still render in-pane (modeRaw), got %v", m.mode)
	}
}

// TestFullScreenAppEscalatesToRelay: a genuine full-screen app (vim, claude —
// alt screen or hidden cursor → snapshot FullScreen) as the sole non-stacked
// pane still escalates to the relay.
func TestFullScreenAppEscalatesToRelay(t *testing.T) {
	m := escModel(t, domain.PaneSnapshot{ID: "p1", RawMode: true, FullScreen: true})
	cmd := m.syncRawMode()
	if cmd == nil {
		t.Fatal("full-screen app as sole pane must start the full-terminal relay")
	}
	if m.relayPaneID != "p1" {
		t.Errorf("relayPaneID = %q, want %q", m.relayPaneID, "p1")
	}
}

// TestNativeFullScreenNeverEscalates guards the pre-existing rule alongside the
// new gate: ##native panes keep Bubble Tea on the keyboard even when full-screen.
func TestNativeFullScreenNeverEscalates(t *testing.T) {
	m := escModel(t, domain.PaneSnapshot{ID: "p1", RawMode: true, FullScreen: true, NativeMode: true})
	if cmd := m.syncRawMode(); cmd != nil {
		t.Fatal("##native pane must never start the full-terminal relay")
	}
	if m.relayPaneID != "" {
		t.Errorf("relayPaneID must stay empty for a native pane, got %q", m.relayPaneID)
	}
}
