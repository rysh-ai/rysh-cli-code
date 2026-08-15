// SPDX-License-Identifier: Apache-2.0

package tui

// model_native.go — TUI side of ##native pass-through shell mode (Phase 6 of
// bash-shell-mode).
//
// A native pane is permanently raw: bash owns readline/completion/history/
// PS1 with full fidelity, and rysh forwards every keystroke. The one gesture
// rysh keeps is double-Esc — the universal "switch modes" chord — which
// exits native mode into prompt (AI) mode. Because a lone Esc is meaningful
// to the shell (readline Meta prefix, vi-mode), the first Esc is HELD for
// nativeEscWindow: a second Esc within the window triggers the exit, and on
// timeout the held Esc is forwarded to the PTY. This is the only added
// latency native mode introduces, and only on the Esc key.

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"

	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// nativeEscWindow is how long the first Esc in a native pane is held while
// waiting for a possible second Esc (the exit gesture).
const nativeEscWindow = 300 * time.Millisecond

// nativeEscTimeoutMsg fires when the hold window elapses; seq guards against
// a stale tick releasing a newer hold.
type nativeEscTimeoutMsg struct{ seq int }

// activePaneNative reports whether the active pane is in ##native mode.
func (m Model) activePaneNative() bool {
	pane := m.findPaneInSnapshot(m.snapshot.ActivePaneID)
	return pane != nil && pane.NativeMode
}

// rawEscWindow is how long after an Esc a second Esc still counts as the
// double-Esc "switch modes" gesture over an auto-detected interactive pane
// (claude, vim, …). Unlike a ##native pane, the Esc is NOT held: it is forwarded
// to the app immediately (zero added latency on a single Esc), and the gesture is
// recognised by counting two Esc presses within this window. The extra Esc the
// app receives on a deliberate mode switch is harmless.
const rawEscWindow = 400 * time.Millisecond

// rawEscResetMsg clears the consecutive-Esc counter when the window elapses; seq
// guards against a stale tick zeroing a newer count.
type rawEscResetMsg struct{ seq int }

// isCoalescedDoubleEsc reports whether a single raw terminal read is two or more
// consecutive ESC (0x1b) bytes and nothing else — a fast Esc double-tap that the
// kernel delivered in one read(). It is the byte-level equivalent of bubbletea's
// "alt+esc" coalescing and drives the PTY relay's double-Esc "switch modes"
// gesture. A read that mixes ESC with other bytes — an escape SEQUENCE such as an
// arrow key (ESC [ A) or Alt+char (ESC x) — is not a double-tap and returns false,
// so those still forward to the interactive app untouched.
func isCoalescedDoubleEsc(data []byte) bool {
	if len(data) < 2 {
		return false
	}
	for _, b := range data {
		if b != 0x1b {
			return false
		}
	}
	return true
}

// activePaneLocalRaw reports whether the active pane is a LOCAL auto-detected
// interactive (raw) pane — an alt-screen app (claude, vim, htop, less, …) running
// in the pane's own shell PTY, as opposed to a ##native pane or a remote/mirror
// interactive share. These are the panes the general double-Esc mode-switch
// gesture applies to.
func (m Model) activePaneLocalRaw() bool {
	pane := m.findPaneInSnapshot(m.snapshot.ActivePaneID)
	return pane != nil && pane.RawMode && !pane.RemoteInteractive && !pane.NativeMode
}

// handleRawEscGesture keeps the universal double-Esc "switch modes" chord working
// over an auto-detected interactive pane (claude, vim, less, …) in modeRaw. Every
// Esc is forwarded to the app immediately so its own Esc handling is unaffected
// (no hold latency); independently, two Esc presses within rawEscWindow fire the
// mode-switch gesture. Any non-Esc key resets the counter so only CONSECUTIVE
// escapes count. Returns handled=false for a non-Esc key (the caller forwards it
// on to the raw-mode key switch); it mutates the receiver in place, so the caller
// must invoke it on an addressable Model (&m).
func (m *Model) handleRawEscGesture(msg tea.KeyMsg) (bool, tea.Cmd) {
	s := msg.String()
	// A deliberate fast double-tap of Esc almost always lands both 0x1b bytes in a
	// SINGLE terminal read, which bubbletea decodes as one "alt+esc" KeyMsg
	// ("\x1b\x1b" → Escape+Alt) rather than two "esc" messages. That coalesced form
	// IS the double-Esc gesture, so fire it immediately. Forward both Esc bytes to
	// the app first so its own Esc handling matches the two-separate-press path.
	// Without this the counter never reaches 2 and the mode switch never happens.
	if s == "alt+esc" {
		m.rawEscCount = 0
		m.forwardRawBytes([]byte{0x1b, 0x1b})
		return true, m.cycleRawInputMode()
	}
	if s != "esc" {
		m.rawEscCount = 0
		return false, nil
	}
	// Forward the Esc to the interactive app right away — a single Esc must reach
	// e.g. vim/claude with no delay (forwardRawBytes is a nil-pub-safe publish).
	m.forwardRawBytes([]byte{0x1b})
	m.rawEscCount++
	if m.rawEscCount >= 2 {
		m.rawEscCount = 0
		return true, m.cycleRawInputMode()
	}
	m.rawEscSeq++
	seq := m.rawEscSeq
	return true, tea.Tick(rawEscWindow, func(time.Time) tea.Msg {
		return rawEscResetMsg{seq: seq}
	})
}

// handleRawEscReset zeroes the consecutive-Esc counter when the window elapses
// with no second Esc (a lone Esc that already reached the app).
func (m Model) handleRawEscReset(msg rawEscResetMsg) (tea.Model, tea.Cmd) {
	if msg.seq == m.rawEscSeq {
		m.rawEscCount = 0
	}
	return m, nil
}

// cycleRawInputMode advances the active raw pane to the next input mode — the
// double-Esc gesture over an interactive app. Leaving shell mode drops the live
// raw view (paneShowsLiveApp gates the active pane's live view on shell mode), so
// the pane shows the new mode's rysh buffer while the app keeps running in the
// shell PTY; cycling back to shell re-enters the live view. Mirrors the
// normal-mode double-Esc cycle. Mutates the receiver in place.
func (m *Model) cycleRawInputMode() tea.Cmd {
	if activeTab := m.activeTab(); activeTab != nil && activeTab.PipelineActive {
		m.sendMsg(&msgpkg.MsgTogglePipelineMode{})
	} else {
		next := m.nextEnabledMode(m.activeInputMode())
		m.setActiveInputMode(next)
		m.historyReset()
	}
	// Drop out of the live-app view now; syncRawMode keeps it out while the input
	// mode is not shell, and re-enters it (or the relay) when it returns to shell.
	m.mode = modeNormal
	m.syncPaneInputFocus()
	if m.pub == nil {
		return nil
	}
	return m.refreshCmd()
}

// handleNativeRawKey intercepts keys in modeRaw for native panes. Returns
// handled=true when the key was consumed (Esc hold / exit gesture); all
// other keys first flush a held Esc so quick Esc+key sequences (vi users)
// still reach the shell in order.
func (m Model) handleNativeRawKey(msg tea.KeyMsg) (bool, tea.Model, tea.Cmd) {
	// A fast Esc double-tap coalesces into one "alt+esc" KeyMsg (bubbletea decodes
	// "\x1b\x1b" as Escape+Alt). That is the exit gesture: leave native mode for AI
	// mode without holding or forwarding either Esc to the shell.
	if msg.String() == "alt+esc" {
		m.nativeEscPending = false
		mm, cmd := m.exitNativeMode()
		return true, mm, cmd
	}
	if msg.String() == "esc" {
		if m.nativeEscPending {
			// Second Esc within the window: leave native mode for AI mode.
			m.nativeEscPending = false
			mm, cmd := m.exitNativeMode()
			return true, mm, cmd
		}
		m.nativeEscPending = true
		m.nativeEscSeq++
		seq := m.nativeEscSeq
		return true, m, tea.Tick(nativeEscWindow, func(time.Time) tea.Msg {
			return nativeEscTimeoutMsg{seq: seq}
		})
	}
	if m.nativeEscPending {
		// Another key while an Esc is held: flush the Esc first so the PTY
		// sees the original byte order (Esc could be a Meta prefix).
		m.nativeEscPending = false
		m.forwardRawBytes([]byte{0x1b})
		if m.pub == nil {
			return true, m, nil // headless (tests): nothing to forward to
		}
		mm, cmd := m.forwardRawKey(msg)
		return true, mm, cmd
	}
	return false, m, nil
}

// handleNativeEscTimeout releases a held Esc to the PTY when no second Esc
// arrived within the window.
func (m Model) handleNativeEscTimeout(msg nativeEscTimeoutMsg) (tea.Model, tea.Cmd) {
	if m.nativeEscPending && msg.seq == m.nativeEscSeq {
		m.nativeEscPending = false
		m.forwardRawBytes([]byte{0x1b})
	}
	return m, nil
}

// exitNativeMode turns ##native off for the active pane and lands the user
// in prompt (AI) mode — "leave the terminal, talk to the agent". Cycling
// input modes back to shell returns to the rysh-managed line; ##native
// re-enters pass-through.
func (m Model) exitNativeMode() (tea.Model, tea.Cmd) {
	paneID := m.snapshot.ActivePaneID
	if paneID == "" {
		return m, nil
	}
	if m.pub != nil {
		_ = m.pub.Send(msgpkg.T("pane", paneID, "inbox"),
			&msgpkg.MsgPaneNativeMode{PaneID: paneID, Action: "off"})
	}
	m.setActiveInputMode("prompt")
	// Snap the TUI back to normal mode immediately; the pane-side raw flag
	// clears on the next snapshot (handleNativeMode resets it synchronously).
	m.mode = modeNormal
	m.syncPaneInputFocus()
	if m.pub == nil {
		return m, nil // headless (tests): nothing to refresh against
	}
	return m, m.refreshCmd()
}
