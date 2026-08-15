// SPDX-License-Identifier: Apache-2.0

package tui

import (
	tea "github.com/charmbracelet/bubbletea"

	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Dedicated replay pane controls (design 006 v2). A replay pane is a
// shell-less, read-only pane (PaneSnapshot.PaneType == "replay") whose content
// is the recorded playback; while it is focused, the TUI claims a small set of
// keys as playback controls and publishes MsgReplayControl to the workspace,
// which drives the controlled player. The live position badge ("REPLAY
// 01:23/04:56 ×2") arrives through the normal pane status path
// (MsgPaneStatusUpdate → PaneSnapshot.Status → paneMetaText), so rendering
// needs no replay-specific code.

// replaySeekStepMs is how far a ←/→ keypress seeks, in milliseconds. Mirrors
// the workspace's replaySeekStep.
const replaySeekStepMs = 10_000

// updateReplayPaneInput handles keystrokes while the focused pane is a
// dedicated replay pane. Returns (true, cmd) when the key was claimed as a
// playback control, (false, nil) otherwise. Only the control keys (space,
// ←/→, +/-, q) and enter are claimed — multiplexer chords (ctrl+o/t/w/…),
// [ / ] tab switching and esc all pass through, so the user is never trapped
// in the pane.
func (m *Model) updateReplayPaneInput(msg tea.KeyMsg) (bool, tea.Cmd) {
	snap := m.focusedPaneSnapshot()
	if snap == nil || snap.PaneType != "replay" {
		return false, nil
	}
	ctrl := func(action string, deltaMs int64) {
		m.sendMsg(&msgpkg.MsgReplayControl{PaneID: snap.ID, Action: action, DeltaMs: deltaMs})
	}
	switch msg.String() {
	case " ", "space":
		ctrl("pause", 0)
		return true, nil
	case "left":
		ctrl("seek", -replaySeekStepMs)
		return true, nil
	case "right":
		ctrl("seek", replaySeekStepMs)
		return true, nil
	case "+", "=":
		ctrl("faster", 0)
		return true, nil
	case "-", "_":
		ctrl("slower", 0)
		return true, nil
	case "q":
		// Close the replay pane. The workspace stops the playback when the
		// pane's actor stops (MsgPaneStopped), so no separate stop is needed.
		m.sendMsg(&msgpkg.MsgClosePane{})
		return true, m.refreshCmd()
	case "enter":
		// Swallowed: a replay pane has no shell to submit to — letting enter
		// through would print a "shell is not available" error into the
		// read-only playback.
		return true, nil
	}
	return false, nil
}
