// SPDX-License-Identifier: Apache-2.0

package vterm

import "testing"

// TestMouseProtocolReportsModeAndEncoding checks that the emulator reports both
// halves of what a child asked for: which events it wants and in which
// encoding. Callers synthesizing events need both — a child on \x1b[?1000h
// alone cannot parse an SGR report.
func TestMouseProtocolReportsModeAndEncoding(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		wantMode  string
		wantSGR   bool
		wantOnFwd bool // IsMouseEnabled
	}{
		{"nothing enabled", "", MouseOff, false, false},
		{"x10", "\x1b[?9h", MouseX10, false, true},
		{"normal, legacy encoding", "\x1b[?1000h", MouseNormal, false, true},
		{"button motion + sgr", "\x1b[?1002h\x1b[?1006h", MouseButton, true, true},
		{"any motion + sgr", "\x1b[?1003h\x1b[?1006h", MouseAny, true, true},
		{"sgr alone is not tracking", "\x1b[?1006h", MouseOff, true, false},
		{"tracking off leaves sgr set", "\x1b[?1002h\x1b[?1006h\x1b[?1002l", MouseOff, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vt := New(24, 80)
			if _, err := vt.Write([]byte(tt.in)); err != nil {
				t.Fatalf("Write: %v", err)
			}
			mode, sgr := vt.MouseProtocol()
			if mode != tt.wantMode || sgr != tt.wantSGR {
				t.Errorf("MouseProtocol() = (%q, %v), want (%q, %v)", mode, sgr, tt.wantMode, tt.wantSGR)
			}
			if got := vt.IsMouseEnabled(); got != tt.wantOnFwd {
				t.Errorf("IsMouseEnabled() = %v, want %v", got, tt.wantOnFwd)
			}
		})
	}
}

// TestResetMouseModesClearsTrackingLeftBehind is the regression guard for mouse
// events leaking into the wrong program: the emulator outlives the programs
// that run in the pane, so tracking a dead child enabled must not survive it.
func TestResetMouseModesClearsTrackingLeftBehind(t *testing.T) {
	vt := New(24, 80)
	// A TUI starts, enables tracking, draws, and is killed before it can send
	// the matching resets.
	if _, err := vt.Write([]byte("\x1b[?1002h\x1b[?1006h\x1b[?1049hhello")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !vt.IsMouseEnabled() {
		t.Fatalf("tracking should be on while the program runs")
	}

	vt.ResetMouseModes()

	if vt.IsMouseEnabled() {
		t.Errorf("IsMouseEnabled() still true after ResetMouseModes")
	}
	if mode, sgr := vt.MouseProtocol(); mode != MouseOff || sgr {
		t.Errorf("MouseProtocol() = (%q, %v), want (%q, false)", mode, sgr, MouseOff)
	}
	// The reset touches modes only: screen contents and the alt-screen state
	// belong to whatever runs next and must be left alone.
	if !vt.IsAltScreen() {
		t.Errorf("ResetMouseModes must not leave the alternate screen")
	}

	// A later program that does want the mouse gets it back.
	if _, err := vt.Write([]byte("\x1b[?1000h")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if mode, _ := vt.MouseProtocol(); mode != MouseNormal {
		t.Errorf("MouseProtocol() = %q after re-enable, want %q", mode, MouseNormal)
	}
}
