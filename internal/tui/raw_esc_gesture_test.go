// SPDX-License-Identifier: Apache-2.0

package tui

// Tests for the double-Esc "switch modes" gesture over an AUTO-DETECTED
// interactive (raw) pane — e.g. the Claude app running in a pane. Unlike the
// ##native gesture, the Esc is forwarded to the app immediately (no hold) and two
// Esc presses within rawEscWindow cycle the pane's input mode.

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

// rawModel builds a Model whose active pane is a local auto-detected interactive
// pane (RawMode, not ##native), in modeRaw and shell input mode.
func rawModel() Model {
	m := paneModel(domain.PaneSnapshot{ID: "p1", RawMode: true})
	m.mode = modeRaw
	m.syncPaneInputs()
	return m
}

// keyAltEsc is what bubbletea emits when a fast Esc double-tap lands both 0x1b
// bytes in one read: it decodes "\x1b\x1b" as a single Escape+Alt KeyMsg.
func keyAltEsc() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyEscape, Alt: true} }

// TestAltEscIsBubbleteaDoubleEsc documents the coalescing this fix targets: a
// KeyEscape with Alt stringifies to "alt+esc", NOT "esc", so the old counter that
// only matched "esc" never advanced on a fast double-tap.
func TestAltEscIsBubbleteaDoubleEsc(t *testing.T) {
	if got := keyAltEsc().String(); got != "alt+esc" {
		t.Fatalf("coalesced double-esc must stringify to alt+esc, got %q", got)
	}
}

// TestRawAltEscCoalescedCyclesMode is the regression test for the reported bug:
// in an interactive (raw) pane a fast double-Esc arrives as one alt+esc KeyMsg and
// must still fire the mode-switch gesture in a single shot.
func TestRawAltEscCoalescedCyclesMode(t *testing.T) {
	m := rawModel()
	handled, _ := (&m).handleRawEscGesture(keyAltEsc())
	if !handled {
		t.Fatal("coalesced alt+esc must be handled as the double-Esc gesture")
	}
	if m.rawEscCount != 0 {
		t.Errorf("counter should be reset after the gesture, got %d", m.rawEscCount)
	}
	if m.mode != modeNormal {
		t.Errorf("coalesced double esc should drop to modeNormal, got %v", m.mode)
	}
	if got := m.activeInputMode(); got != "prompt" {
		t.Errorf("coalesced double esc should cycle shell→prompt, got %q", got)
	}
}

// TestNativeAltEscCoalescedExits guards the ##native pane path: a coalesced
// alt+esc leaves native mode for prompt (AI) mode.
func TestNativeAltEscCoalescedExits(t *testing.T) {
	m := paneModel(domain.PaneSnapshot{ID: "p1", RawMode: true, NativeMode: true})
	m.mode = modeRaw
	m.syncPaneInputs()
	handled, mm, _ := m.handleNativeRawKey(keyAltEsc())
	if !handled {
		t.Fatal("coalesced alt+esc must be handled on a native pane")
	}
	res := asModel(t, mm)
	if got := res.activeInputMode(); got != "prompt" {
		t.Errorf("native alt+esc should exit to prompt mode, got %q", got)
	}
	if res.mode != modeNormal {
		t.Errorf("native alt+esc should drop to modeNormal, got %v", res.mode)
	}
}

// TestIsCoalescedDoubleEsc pins the relay's byte-level double-Esc classifier: a
// run of ESC bytes is the gesture, anything mixed with other bytes (escape
// sequences, alt+char) is not.
func TestIsCoalescedDoubleEsc(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want bool
	}{
		{"single esc", []byte{0x1b}, false},
		{"double esc", []byte{0x1b, 0x1b}, true},
		{"triple esc", []byte{0x1b, 0x1b, 0x1b}, true},
		{"empty", nil, false},
		{"arrow up (esc [ A)", []byte{0x1b, '[', 'A'}, false},
		{"alt+char (esc x)", []byte{0x1b, 'x'}, false},
		{"plain rune", []byte{'a'}, false},
		{"esc then rune", []byte{0x1b, 'a'}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isCoalescedDoubleEsc(c.data); got != c.want {
				t.Errorf("isCoalescedDoubleEsc(%v) = %v, want %v", c.data, got, c.want)
			}
		})
	}
}

func TestRawFirstEscForwardsAndArms(t *testing.T) {
	m := rawModel()
	handled, cmd := (&m).handleRawEscGesture(tea.KeyMsg{Type: tea.KeyEsc})
	if !handled {
		t.Fatal("first esc must be handled")
	}
	if m.rawEscCount != 1 {
		t.Errorf("first esc should arm the counter, got %d", m.rawEscCount)
	}
	if cmd == nil {
		t.Error("first esc should schedule the reset tick")
	}
	if m.mode != modeRaw {
		t.Errorf("mode changed on first esc: %v", m.mode)
	}
	if got := m.activeInputMode(); got != "shell" {
		t.Errorf("first esc should not change input mode, got %q", got)
	}
}

func TestRawDoubleEscCyclesMode(t *testing.T) {
	m := rawModel()
	if _, _ = (&m).handleRawEscGesture(tea.KeyMsg{Type: tea.KeyEsc}); m.rawEscCount != 1 {
		t.Fatalf("after first esc count=%d", m.rawEscCount)
	}
	handled, _ := (&m).handleRawEscGesture(tea.KeyMsg{Type: tea.KeyEsc})
	if !handled {
		t.Fatal("second esc must be handled")
	}
	if m.rawEscCount != 0 {
		t.Errorf("counter should reset after the gesture, got %d", m.rawEscCount)
	}
	if m.mode != modeNormal {
		t.Errorf("double esc should drop to modeNormal, got %v", m.mode)
	}
	if got := m.activeInputMode(); got != "prompt" {
		t.Errorf("double esc should cycle shell→prompt, got %q", got)
	}
}

func TestRawNonEscResetsCounter(t *testing.T) {
	m := rawModel()
	(&m).handleRawEscGesture(tea.KeyMsg{Type: tea.KeyEsc})
	if m.rawEscCount != 1 {
		t.Fatalf("expected armed counter, got %d", m.rawEscCount)
	}
	// A non-Esc key is not consumed here (the caller forwards it) and clears the
	// counter, so only CONSECUTIVE escapes count.
	handled, _ := (&m).handleRawEscGesture(keyRunes("a"))
	if handled {
		t.Error("plain key must not be consumed by the esc gesture")
	}
	if m.rawEscCount != 0 {
		t.Errorf("non-esc key should reset the counter, got %d", m.rawEscCount)
	}
	// A following esc is only the first of a new pair (does NOT fire the gesture).
	(&m).handleRawEscGesture(tea.KeyMsg{Type: tea.KeyEsc})
	if m.mode != modeRaw {
		t.Errorf("esc after reset must not switch modes, got %v", m.mode)
	}
}

func TestRawEscResetTick(t *testing.T) {
	m := rawModel()
	(&m).handleRawEscGesture(tea.KeyMsg{Type: tea.KeyEsc})
	seq := m.rawEscSeq
	// Matching tick clears the counter.
	mm, _ := m.handleRawEscReset(rawEscResetMsg{seq: seq})
	if res := asModel(t, mm); res.rawEscCount != 0 {
		t.Errorf("matching reset tick should clear the counter, got %d", res.rawEscCount)
	}
	// A stale tick (old seq) must NOT clear a newer count.
	m = rawModel()
	(&m).handleRawEscGesture(tea.KeyMsg{Type: tea.KeyEsc})
	mm2, _ := m.handleRawEscReset(rawEscResetMsg{seq: m.rawEscSeq - 1})
	if res := asModel(t, mm2); res.rawEscCount != 1 {
		t.Error("stale reset tick must not clear a newer count")
	}
}

func TestPaneShowsLiveApp(t *testing.T) {
	// Active raw pane in shell mode → shows the live app.
	m := rawModel()
	pane := m.findPaneInSnapshot("p1")
	if !m.paneShowsLiveApp(pane) {
		t.Error("active raw pane in shell mode should show the live app")
	}
	// Switch to a non-shell input mode → the live view is dropped for the buffer.
	m.setActiveInputMode("prompt")
	if m.paneShowsLiveApp(pane) {
		t.Error("active raw pane in prompt mode should NOT show the live app")
	}
	// A non-raw pane never shows a live app.
	m.setActiveInputMode("shell")
	plain := &domain.PaneSnapshot{ID: "p1"}
	if m.paneShowsLiveApp(plain) {
		t.Error("non-raw pane should not show a live app")
	}
	// A BACKGROUND raw pane always shows its live app, regardless of input mode.
	bg := &domain.PaneSnapshot{ID: "other", RawMode: true}
	if !m.paneShowsLiveApp(bg) {
		t.Error("background raw pane should show its live app")
	}
}

// TestRawGestureIgnoresNativeAndRemote guards the dispatch predicate: the general
// gesture applies only to local, non-native raw panes.
func TestRawGestureAppliesOnlyToLocalRaw(t *testing.T) {
	// Native pane: handled by the native gesture, not this one.
	native := paneModel(domain.PaneSnapshot{ID: "p1", RawMode: true, NativeMode: true})
	if native.activePaneLocalRaw() {
		t.Error("##native pane must not be treated as a plain local raw pane")
	}
	// Remote interactive share: not a local raw pane.
	remote := paneModel(domain.PaneSnapshot{ID: "p1", RemoteInteractive: true})
	if remote.activePaneLocalRaw() {
		t.Error("remote interactive pane must not be treated as a local raw pane")
	}
	// Plain auto-detected interactive pane: yes.
	local := paneModel(domain.PaneSnapshot{ID: "p1", RawMode: true})
	if !local.activePaneLocalRaw() {
		t.Error("local auto-detected raw pane should be a local raw pane")
	}
}
