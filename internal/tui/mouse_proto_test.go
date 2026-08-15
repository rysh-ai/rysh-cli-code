// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rysh-ai/rysh-cli-code/internal/vterm"
)

// TestMouseEncodingFollowsChildProtocol is the regression guard for mouse
// events showing up as literal text ("<65;50;54M") in a child's input line:
// rysh used to encode every forwarded event as SGR regardless of what the child
// enabled, so a program on \x1b[?1000h alone received a sequence it could not
// parse.
func TestMouseEncodingFollowsChildProtocol(t *testing.T) {
	wheel := tea.MouseMsg{Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress}
	press := tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionPress}
	release := tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionRelease}
	motion := tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionMotion}

	tests := []struct {
		name  string
		msg   tea.MouseMsg
		x, y  int
		proto string
		sgr   bool
		want  string
	}{
		{"sgr press", press, 5, 7, vterm.MouseButton, true, "\x1b[<0;5;7M"},
		{"sgr release", release, 5, 7, vterm.MouseButton, true, "\x1b[<0;5;7m"},
		{"sgr motion", motion, 5, 7, vterm.MouseButton, true, "\x1b[<32;5;7M"},
		{"sgr wheel", wheel, 50, 54, vterm.MouseButton, true, "\x1b[<65;50;54M"},

		// Legacy X10 encoding: \x1b[M then button+32, x+32, y+32.
		{"legacy press", press, 5, 7, vterm.MouseNormal, false, "\x1b[M \x25\x27"},
		{"legacy release is button 3", release, 5, 7, vterm.MouseNormal, false, "\x1b[M#\x25\x27"},
		{"legacy wheel", wheel, 1, 1, vterm.MouseNormal, false, "\x1b[Ma!!"},

		// Events the child's tracking mode does not cover.
		{"no tracking", press, 5, 7, vterm.MouseOff, true, ""},
		{"normal mode drops motion", motion, 5, 7, vterm.MouseNormal, false, ""},
		{"x10 drops release", release, 5, 7, vterm.MouseX10, false, ""},
		{"x10 drops motion", motion, 5, 7, vterm.MouseX10, false, ""},
		{"x10 drops wheel", wheel, 5, 7, vterm.MouseX10, false, ""},
		{"x10 press", press, 5, 7, vterm.MouseX10, false, "\x1b[M \x25\x27"},

		// The legacy encoding is one byte per field and cannot express these.
		{"legacy drops out-of-range column", press, 224, 7, vterm.MouseNormal, false, ""},
		{"legacy drops out-of-range row", press, 5, 224, vterm.MouseNormal, false, ""},
		{"sgr handles far coordinates", press, 224, 300, vterm.MouseButton, true, "\x1b[<0;224;300M"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(mouseToPTYBytes(tt.msg, tt.x, tt.y, tt.proto, tt.sgr))
			if got != tt.want {
				t.Errorf("mouseToPTYBytes() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestMouseEncodingModifiers checks the modifier bits ride along in the modes
// that have room for them, and are dropped in X10 which does not.
func TestMouseEncodingModifiers(t *testing.T) {
	msg := tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionPress, Shift: true, Ctrl: true}

	if got, want := string(mouseToPTYBytes(msg, 1, 1, vterm.MouseButton, true)), "\x1b[<20;1;1M"; got != want {
		t.Errorf("sgr with shift+ctrl = %q, want %q", got, want)
	}
	if got, want := string(mouseToPTYBytes(msg, 1, 1, vterm.MouseX10, false)), "\x1b[M !!"; got != want {
		t.Errorf("x10 with shift+ctrl = %q, want %q (X10 encodes no modifiers)", got, want)
	}
}

// TestMouseReportFilterStripsStaleReports covers the relay's stdin guard: while
// a relay runs the terminal has been told to stop reporting, so anything
// mouse-shaped still arriving is stale and must not be typed into the child.
func TestMouseReportFilterStripsStaleReports(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain typing untouched", "ssf", "ssf"},
		{"sgr report dropped", "\x1b[<65;50;54M", ""},
		{"sgr release dropped", "\x1b[<0;5;7m", ""},
		{"legacy report dropped", "\x1b[M !!", ""},
		{"report between keystrokes", "a\x1b[<65;50;54Mb", "ab"},
		{"burst of wheel reports", "\x1b[<65;1;1M\x1b[<65;2;2M\x1b[<64;3;3M", ""},
		{"arrow key survives", "\x1b[A", "\x1b[A"},
		{"lone esc survives", "\x1b", "\x1b"},
		{"csi with a letter is not a report", "\x1b[<3A", "\x1b[<3A"},
		{"legacy report with high bytes", "\x1b[M\x60\xc8\xc8", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var f mouseReportFilter
			if got := string(f.filter([]byte(tt.in))); got != tt.want {
				t.Errorf("filter(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestMouseReportFilterAcrossReads checks a report split across two reads — the
// shape a laggy link produces, and the reason the leaked text arrives in
// fragments like ";47;39M".
func TestMouseReportFilterAcrossReads(t *testing.T) {
	var f mouseReportFilter

	if got := string(f.filter([]byte("x\x1b[<65;47"))); got != "x" {
		t.Errorf("first read = %q, want %q (the partial report must be held back)", got, "x")
	}
	if got := string(f.filter([]byte(";39My"))); got != "y" {
		t.Errorf("second read = %q, want %q", got, "y")
	}

	// Legacy reports split mid-coordinate too.
	if got := string(f.filter([]byte("\x1b[M "))); got != "" {
		t.Errorf("partial legacy read = %q, want empty", got)
	}
	if got := string(f.filter([]byte("!!z"))); got != "z" {
		t.Errorf("legacy completion read = %q, want %q", got, "z")
	}
}

// TestMouseReportFilterReleasesUnterminatedPrefix guards against a malformed
// prefix stalling input forever: past the cap the held bytes are passed through
// rather than accumulated.
func TestMouseReportFilterReleasesUnterminatedPrefix(t *testing.T) {
	// An unterminated run longer than the cap is not a mouse report; it goes
	// through untouched rather than being swallowed.
	var f mouseReportFilter
	overlong := "\x1b[<" + "1;1;1;1;1;1;1;1;1;1;1;1;1"
	if len(overlong) <= maxPendingMouseReport {
		t.Fatalf("test input must exceed the hold cap, got %d bytes", len(overlong))
	}
	if got := string(f.filter([]byte(overlong))); got != overlong {
		t.Errorf("filter(overlong) = %q, want it passed through", got)
	}

	// A prefix held from an earlier read is released once it grows past the cap,
	// so a malformed prefix can never stall input indefinitely.
	f = mouseReportFilter{}
	head, tail := "\x1b[<1;1;1;1;1", ";1;1;1;1;1;1;1;1"
	if got := string(f.filter([]byte(head))); got != "" {
		t.Fatalf("first read = %q, want empty (within the hold cap)", got)
	}
	if got := string(f.filter([]byte(tail))); got != head+tail {
		t.Errorf("second read = %q, want the held prefix flushed through", got)
	}
	if len(f.pending) != 0 {
		t.Errorf("pending = %q, want empty after the flush", f.pending)
	}
}
