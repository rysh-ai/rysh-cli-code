// SPDX-License-Identifier: Apache-2.0

package tui

import (
	tea "github.com/charmbracelet/bubbletea"
	"testing"
)

func TestFaceTimePrefixStartsLocalPicker(t *testing.T) {
	m := readlineModel(nil)
	m.mode = modePrefix
	model, cmd := m.updatePrefixMode(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f")})
	if asModel(t, model).mode != modeNormal || cmd == nil {
		t.Fatal("Ctrl+O F must suspend the TUI for the local picker")
	}
	// Do not run the command: it requests real Contacts permission on a local Mac.
}
