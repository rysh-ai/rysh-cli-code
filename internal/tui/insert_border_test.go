package tui

import "testing"

// TestInsertModeBorderColor verifies that the focused pane's border accent is
// green only while the pane is capturing text input (normal/raw mode), and
// falls back to the cyan active accent in navigation/command modes. This is the
// visual cue that tells the user "you are in insert mode — keystrokes land in
// this pane" without having to press Esc to find out.
func TestInsertModeBorderColor(t *testing.T) {
	cases := []struct {
		mode       inputMode
		wantInsert bool
	}{
		{modeNormal, true},
		{modeRaw, true},
		{modePane, false},
		{modeNavigate, false},
		{modeTab, false},
		{modeStack, false},
		{modeLayout, false},
		{modeResize, false},
		{modeRawScroll, false},
		{modeApproval, false},
	}

	for _, tc := range cases {
		m := Model{mode: tc.mode}
		if got := m.insertMode(); got != tc.wantInsert {
			t.Errorf("mode=%q insertMode()=%v, want %v", tc.mode, got, tc.wantInsert)
		}

		gotColor := m.activePaneBorderColor()
		wantColor := activeBorderColor
		if tc.wantInsert {
			wantColor = insertBorderColor
		}
		if gotColor != wantColor {
			t.Errorf("mode=%q activePaneBorderColor()=%v, want %v", tc.mode, gotColor, wantColor)
		}
	}

	// The two accents must be distinct, otherwise the cue is invisible.
	if insertBorderColor == activeBorderColor {
		t.Fatalf("insertBorderColor and activeBorderColor must differ")
	}
}
