// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/vterm"
)

// TestClearStaleMouseModesOnChildExit is the regression guard for mouse events
// leaking into the next program that runs in a pane. The emulator outlives the
// programs it renders, so tracking enabled by a child that died without
// disabling it would otherwise make the pane look mouse-aware forever, and the
// TUI would forward wheel/click events into whatever came next.
func TestClearStaleMouseModesOnChildExit(t *testing.T) {
	const shellPgid = 4242

	newPane := func() *PaneActor {
		p := &PaneActor{id: "pane-1", shellPgid: shellPgid, vtermEmu: vterm.New(24, 80)}
		// A TUI runs and is killed before it can send the matching resets.
		if _, err := p.vtermEmu.Write([]byte("\x1b[?1002h\x1b[?1006h")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if !p.vtermEmu.IsMouseEnabled() {
			t.Fatalf("setup: tracking should be on")
		}
		return p
	}

	t.Run("foreground back at the shell clears it", func(t *testing.T) {
		p := newPane()
		p.clearStaleMouseModes(shellPgid)
		if p.vtermEmu.IsMouseEnabled() {
			t.Errorf("tracking still on after the child exited")
		}
	})

	t.Run("another foreground child keeps it", func(t *testing.T) {
		// A program that shells out (vim's :!cmd) runs in a new process group,
		// never the pane's shell pgid — it must keep its tracking across the
		// excursion.
		p := newPane()
		p.clearStaleMouseModes(shellPgid + 1)
		if !p.vtermEmu.IsMouseEnabled() {
			t.Errorf("tracking cleared while another foreground child was running")
		}
	})

	t.Run("unknown foreground keeps it", func(t *testing.T) {
		p := newPane()
		p.clearStaleMouseModes(-1)
		if !p.vtermEmu.IsMouseEnabled() {
			t.Errorf("tracking cleared on an unreadable foreground group")
		}
	})

	t.Run("no shell pgid recorded is a no-op", func(t *testing.T) {
		p := newPane()
		p.shellPgid = 0
		p.clearStaleMouseModes(0)
		if !p.vtermEmu.IsMouseEnabled() {
			t.Errorf("tracking cleared without a known shell pgid")
		}
	})
}
