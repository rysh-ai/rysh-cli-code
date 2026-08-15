// SPDX-License-Identifier: Apache-2.0

package domain

import "testing"

// TestAgentsBoardPaneIsShellless pins the guard that keeps an agents-board pane
// read-only by construction.
//
// The shell-start guard in actors/pane.go used to read `p.paneType != "replay"`.
// Any new read-only pane type added after that line was written would silently
// grow a PTY — which is exactly what agents-board (design 025) would have done,
// turning a board meant to be read-only into one a stray keystroke can type into.
func TestAgentsBoardPaneIsShellless(t *testing.T) {
	if !IsShelllessPaneType(PaneTypeAgentsBoard) {
		t.Errorf("an agents-board pane must never start a shell: "+
			"IsShelllessPaneType(%q) = false, want true", PaneTypeAgentsBoard)
	}
}

func TestShelllessPaneTypes(t *testing.T) {
	for _, tc := range []struct {
		paneType string
		want     bool
		why      string
	}{
		{PaneTypeReplay, true, "design 006 v2: recorded playback, read-only by construction"},
		{PaneTypeAgentsBoard, true, "design 025: the board renders posts, it is not a terminal"},
		{PaneTypeNormal, false, "the ordinary shell pane must keep its PTY"},
		{PaneTypeApproval, false, "handled on its own path; not claimed by this helper"},
		{"some-future-type", false, "unknown types default to having a shell, as before"},
	} {
		if got := IsShelllessPaneType(tc.paneType); got != tc.want {
			t.Errorf("IsShelllessPaneType(%q) = %v, want %v — %s",
				tc.paneType, got, tc.want, tc.why)
		}
	}
}

// TestPaneTypeNormalIsTheEmptyString guards a fact the old comment on
// PaneSnapshot.PaneType got wrong: "normal" is never written to the wire.
func TestPaneTypeNormalIsTheEmptyString(t *testing.T) {
	if PaneTypeNormal != "" {
		t.Errorf("PaneTypeNormal = %q, want \"\" — the literal \"normal\" is "+
			"never assigned anywhere in the tree", PaneTypeNormal)
	}
}
