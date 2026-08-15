// SPDX-License-Identifier: Apache-2.0

package tui

// Phase 6 (bash-shell-mode): native pass-through mode tests — the double-Esc
// exit gesture with the Esc hold window.

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

func nativeModel() Model {
	m := paneModel(domain.PaneSnapshot{ID: "p1", NativeMode: true, RawMode: true})
	m.mode = modeRaw
	m.syncPaneInputs()
	return m
}

func TestNativeFirstEscIsHeld(t *testing.T) {
	m := nativeModel()
	handled, mm, cmd := m.handleNativeRawKey(tea.KeyMsg{Type: tea.KeyEsc})
	if !handled {
		t.Fatal("first esc must be handled (held)")
	}
	res := asModel(t, mm)
	if !res.nativeEscPending {
		t.Error("first esc should arm the hold")
	}
	if cmd == nil {
		t.Error("first esc should schedule the timeout tick")
	}
	if res.mode != modeRaw {
		t.Errorf("mode changed on first esc: %v", res.mode)
	}
}

func TestNativeDoubleEscExits(t *testing.T) {
	m := nativeModel()
	_, mm, _ := m.handleNativeRawKey(tea.KeyMsg{Type: tea.KeyEsc})
	m = asModel(t, mm)
	handled, mm2, _ := m.handleNativeRawKey(tea.KeyMsg{Type: tea.KeyEsc})
	if !handled {
		t.Fatal("second esc must be handled")
	}
	res := asModel(t, mm2)
	if res.nativeEscPending {
		t.Error("hold should be released")
	}
	if res.mode != modeNormal {
		t.Errorf("double esc should return to modeNormal, got %v", res.mode)
	}
	if got := res.paneInputModes["p1"]; got != "prompt" {
		t.Errorf("double esc should land in prompt mode, got %q", got)
	}
}

func TestNativeEscTimeoutReleases(t *testing.T) {
	m := nativeModel()
	_, mm, _ := m.handleNativeRawKey(tea.KeyMsg{Type: tea.KeyEsc})
	m = asModel(t, mm)
	seq := m.nativeEscSeq
	mm2, _ := m.handleNativeEscTimeout(nativeEscTimeoutMsg{seq: seq})
	res := asModel(t, mm2)
	if res.nativeEscPending {
		t.Error("timeout should release the hold")
	}
	// A stale tick (old seq) must not clear a newer hold.
	m = nativeModel()
	_, mm3, _ := m.handleNativeRawKey(tea.KeyMsg{Type: tea.KeyEsc})
	m = asModel(t, mm3)
	mm4, _ := m.handleNativeEscTimeout(nativeEscTimeoutMsg{seq: m.nativeEscSeq - 1})
	if res2 := asModel(t, mm4); !res2.nativeEscPending {
		t.Error("stale tick must not release a newer hold")
	}
}

func TestNativeNonEscPassesThrough(t *testing.T) {
	m := nativeModel()
	// Without a pending esc, ordinary keys are not consumed here (they fall
	// through to forwardRawKey in the caller).
	handled, _, _ := m.handleNativeRawKey(keyRunes("a"))
	if handled {
		t.Error("plain key must not be consumed when no esc is held")
	}
	// With a pending esc, the key IS consumed (esc flush + forward).
	_, mm, _ := m.handleNativeRawKey(tea.KeyMsg{Type: tea.KeyEsc})
	m = asModel(t, mm)
	handled2, mm2, _ := m.handleNativeRawKey(keyRunes("b"))
	if !handled2 {
		t.Error("key after held esc must be consumed (flush + forward)")
	}
	if res := asModel(t, mm2); res.nativeEscPending {
		t.Error("held esc should be flushed by the following key")
	}
}
