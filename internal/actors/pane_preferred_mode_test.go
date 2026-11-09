// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// desktop CD3 N1: a mode the daemon asks a pane to open (the company view
// after ##config load / ry connect) is recorded as the pane's preferred mode,
// so a front-end that attaches later (the desktop app) can open it. It lasts
// until the user picks another mode or the mode is disabled.
func TestPanePreferredModeIsRecordedAndCleared(t *testing.T) {
	p := newModeTestPane(t)
	p.handleEnableModeMsg(&msg.MsgPaneEnableMode{PaneID: p.id, Mode: "company"})
	if got := p.buildSnapshot(false, false, false).PreferredMode; got != "" {
		t.Fatalf("a plain enable set preferred_mode %q", got)
	}
	p.handleEnableModeMsg(&msg.MsgPaneEnableMode{PaneID: p.id, Mode: "company", Activate: true})
	snap := p.buildSnapshot(false, false, false)
	if snap.PreferredMode != "company" {
		t.Fatalf("preferred_mode = %q, want company", snap.PreferredMode)
	}

	// It survives a restore.
	q := newModeTestPane(t)
	q.RestoreState(snap)
	if got := q.buildSnapshot(false, false, false).PreferredMode; got != "company" {
		t.Fatalf("restored preferred_mode = %q", got)
	}

	// The user switching mode clears it.
	p.setInputMode("shell")
	if got := p.buildSnapshot(false, false, false).PreferredMode; got != "" {
		t.Fatalf("a user mode switch left preferred_mode %q", got)
	}

	// Disabling the mode clears it.
	p.handleEnableModeMsg(&msg.MsgPaneEnableMode{PaneID: p.id, Mode: "company", Activate: true})
	p.handleDisableMode("company", false)
	if got := p.buildSnapshot(false, false, false).PreferredMode; got != "" {
		t.Fatalf("disabling the mode left preferred_mode %q", got)
	}

	// Activating a mode that is not valid records nothing.
	p.handleEnableModeMsg(&msg.MsgPaneEnableMode{PaneID: p.id, Mode: "nosuchmode", Activate: true})
	if got := p.buildSnapshot(false, false, false).PreferredMode; got != "" {
		t.Fatalf("an invalid mode became preferred: %q", got)
	}
}
