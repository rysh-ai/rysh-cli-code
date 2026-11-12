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

// TestLateFullScreenSignalEscalatesToRelay (video story 043, 2026-11-08): vim
// raises the pane's RawMode (a foreground child) a moment before its
// alt-screen signal, so the first snapshot puts the TUI in in-pane raw mode
// with the frame drawn. When the full-screen signal arrives in a later
// snapshot, the sole non-stacked pane must move to the relay then, not only
// after the user leaves raw mode (Ctrl+O, q), so the first view matches the
// design either way.
func TestLateFullScreenSignalEscalatesToRelay(t *testing.T) {
	m := escModel(t, domain.PaneSnapshot{ID: "p1", RawMode: true})
	if cmd := m.syncRawMode(); cmd != nil || m.mode != modeRaw {
		t.Fatalf("setup: a plain foreground child stays in-pane (cmd %v, mode %v)", cmd != nil, m.mode)
	}
	m.snapshot.Tabs[0].Lanes[0].PaneGroups[0].Panes[0].FullScreen = true
	cmd := m.syncRawMode()
	if cmd == nil || m.relayPaneID != "p1" {
		t.Fatalf("a full-screen signal arriving in raw mode must start the relay (cmd %v, relayPaneID %q)", cmd != nil, m.relayPaneID)
	}

	// A ##native pane still never escalates, early or late.
	n := escModel(t, domain.PaneSnapshot{ID: "p1", RawMode: true, NativeMode: true})
	_ = n.syncRawMode()
	n.snapshot.Tabs[0].Lanes[0].PaneGroups[0].Panes[0].FullScreen = true
	if cmd := n.syncRawMode(); cmd != nil || n.relayPaneID != "" {
		t.Fatal("a ##native pane must never start the relay")
	}
}
