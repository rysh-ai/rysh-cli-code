// SPDX-License-Identifier: Apache-2.0

package vterm

import (
	"bytes"
	"strings"
	"testing"
)

func TestNewWithWriterRespondsToCPR(t *testing.T) {
	var out bytes.Buffer
	vt := NewWithWriter(24, 80, &out)

	if _, err := vt.Write([]byte("\x1b[6n")); err != nil {
		t.Fatalf("Write CPR query: %v", err)
	}

	if got, want := out.String(), "\x1b[1;1R"; got != want {
		t.Fatalf("CPR response = %q, want %q", got, want)
	}
}

func TestStripUnsupportedCSI(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "plain text unchanged",
			in:   "hello world",
			want: "hello world",
		},
		{
			name: "empty input",
			in:   "",
			want: "",
		},
		{
			name: "kitty keyboard push ESC[>1u stripped",
			in:   "\x1b[>1u",
			want: "",
		},
		{
			name: "kitty keyboard pop ESC[<u stripped",
			in:   "\x1b[<u",
			want: "",
		},
		{
			name: "xterm modifyOtherKeys ESC[>4;2m stripped",
			in:   "\x1b[>4;2m",
			want: "",
		},
		{
			name: "terminal id ESC[=0c stripped",
			in:   "\x1b[=0c",
			want: "",
		},
		{
			name: "DEC private mode ESC[?25l preserved",
			in:   "\x1b[?25l",
			want: "\x1b[?25l",
		},
		{
			name: "DEC alt screen ESC[?1049h preserved",
			in:   "\x1b[?1049h",
			want: "\x1b[?1049h",
		},
		{
			name: "standard SGR ESC[1;31m preserved",
			in:   "\x1b[1;31m",
			want: "\x1b[1;31m",
		},
		{
			name: "standard cursor restore ESC[u preserved",
			in:   "\x1b[u",
			want: "\x1b[u",
		},
		{
			name: "mixed: text + kitty push + text",
			in:   "before\x1b[>1uafter",
			want: "beforeafter",
		},
		{
			name: "multiple unsupported sequences stripped",
			in:   "\x1b[<u\x1b[>1u\x1b[>4;2m",
			want: "",
		},
		{
			name: "unsupported interleaved with supported",
			in:   "\x1b[?25l\x1b[>1u\x1b[1;31mhello\x1b[<u\x1b[0m",
			want: "\x1b[?25l\x1b[1;31mhello\x1b[0m",
		},
		{
			name: "cursor save/restore preserved",
			in:   "\x1b[s\x1b[u",
			want: "\x1b[s\x1b[u",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(stripUnsupportedCSI([]byte(tt.in)))
			if got != tt.want {
				t.Errorf("stripUnsupportedCSI(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestKittyKeyboardPushDoesNotMoveCursor(t *testing.T) {
	// Regression test: ESC[>1u (Kitty keyboard push) was being misinterpreted
	// by vt10x as ESC[u (SCORC — restore cursor), resetting the cursor to 0,0.
	// After the CSI filter, the cursor should stay where it was positioned.
	vt := New(24, 80)

	// Seed cursor to the bottom row via newlines (row 23 of a 24-row terminal).
	nl := make([]byte, 23)
	for i := range nl {
		nl[i] = '\n'
	}
	vt.Write(nl)

	row, _ := vt.CursorPos()
	if row != 23 {
		t.Fatalf("after seed: cursor row = %d, want 23", row)
	}

	// Send the Kitty keyboard push sequence that used to reset the cursor.
	vt.Write([]byte("\x1b[>1u"))

	row, _ = vt.CursorPos()
	if row != 23 {
		t.Fatalf("after ESC[>1u: cursor row = %d, want 23 (cursor moved!)", row)
	}
}

func TestKittyKeyboardPopDoesNotMoveCursor(t *testing.T) {
	vt := New(24, 80)

	// Position cursor at row 10.
	nl := make([]byte, 10)
	for i := range nl {
		nl[i] = '\n'
	}
	vt.Write(nl)

	row, _ := vt.CursorPos()
	if row != 10 {
		t.Fatalf("after positioning: cursor row = %d, want 10", row)
	}

	// ESC[<u (Kitty keyboard pop) should be stripped.
	vt.Write([]byte("\x1b[<u"))

	row, _ = vt.CursorPos()
	if row != 10 {
		t.Fatalf("after ESC[<u: cursor row = %d, want 10 (cursor moved!)", row)
	}
}

func TestRepaintRoundTrip(t *testing.T) {
	// A source VTerm holds the live screen of an interactive app. Repaint() must
	// produce a byte stream that, when fed into a fresh subscriber VTerm of the
	// same size, reconstructs identical screen contents and cursor position.
	// This is the mechanism that resumes a shared interactive session for a
	// subscriber that (re)joins after the app already went interactive.
	src := New(24, 80)

	// Draw a few lines, then place the cursor at an explicit position (row 3,
	// col 10 — 1-based ESC sequence, so 0-based row 2, col 9).
	src.Write([]byte("first line\r\nsecond line\r\n  indented third\r\n"))
	src.Write([]byte("\x1b[3;10H"))

	wantRows := src.Render()
	wantRow, wantCol := src.CursorPos()

	// Feed the repaint into a fresh, empty VTerm of the same dimensions.
	dst := New(24, 80)
	if _, err := dst.Write(src.Repaint()); err != nil {
		t.Fatalf("Write repaint: %v", err)
	}

	gotRows := dst.Render()
	if len(gotRows) != len(wantRows) {
		t.Fatalf("row count = %d, want %d", len(gotRows), len(wantRows))
	}
	for i := range wantRows {
		if gotRows[i] != wantRows[i] {
			t.Errorf("row %d = %q, want %q", i, gotRows[i], wantRows[i])
		}
	}

	gotRow, gotCol := dst.CursorPos()
	if gotRow != wantRow || gotCol != wantCol {
		t.Errorf("cursor = (%d,%d), want (%d,%d)", gotRow, gotCol, wantRow, wantCol)
	}
}

func TestWriteReturnOriginalLength(t *testing.T) {
	vt := New(24, 80)

	// Input contains a filtered sequence — returned length should still be
	// the original byte count.
	input := []byte("hello\x1b[>1uworld")
	n, err := vt.Write(input)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(input) {
		t.Errorf("Write returned %d, want %d (original length)", n, len(input))
	}
}

// TestAltScreenRedundantResetDoesNotReenter guards against a vt10x quirk: it
// toggles its alternate-screen flag with XOR, so a reset (ESC[?1049l / ?1047l /
// ?47l) received when the alt screen is already inactive spuriously ENTERS the
// alt buffer. Interactive programs commonly emit several alt resets on teardown
// (e.g. terminfo rmcup → ?1047l then ?1049l); the redundant one would leave the
// pane stuck in raw mode showing the shell prompt after the program exits.
func TestAltScreenRedundantResetDoesNotReenter(t *testing.T) {
	v := New(24, 80)
	if v.IsAltScreen() {
		t.Fatal("fresh VTerm should not be in alt screen")
	}
	v.Write([]byte("\x1b[?1049h")) // enter alt screen
	if !v.IsAltScreen() {
		t.Fatal("expected alt screen after ESC[?1049h")
	}
	v.Write([]byte("\x1b[?1049l")) // genuine exit
	if v.IsAltScreen() {
		t.Fatal("expected non-alt after ESC[?1049l")
	}
	// Redundant resets that arrive after the genuine exit must be no-ops.
	v.Write([]byte("\x1b[?1047l"))
	if v.IsAltScreen() {
		t.Fatal("redundant ESC[?1047l must NOT re-enter alt screen")
	}
	v.Write([]byte("\x1b[?1049l"))
	if v.IsAltScreen() {
		t.Fatal("redundant ESC[?1049l must NOT re-enter alt screen")
	}
}

// TestRenderANSIWithCursorDrawsBlockCursor verifies that the cursor cell is
// rendered with reverse video (so vim's cursor is visible in VT-in-pane mode),
// that plain RenderANSI does not draw it, and that a hidden cursor is not drawn.
func TestRenderANSIWithCursorDrawsBlockCursor(t *testing.T) {
	v := New(3, 10)
	v.Write([]byte("\x1b[1;1Hhi")) // write "hi" on row 0 (cursor ends at col 2)
	v.Write([]byte("\x1b[1;1H"))   // move cursor back onto 'h' (row 0, col 0)

	withCursor := v.RenderANSIWithCursor()
	if len(withCursor) == 0 || !strings.Contains(withCursor[0], "\x1b[0;7m") {
		t.Fatalf("expected reverse-video cursor (ESC[7m) on row 0, got %q", withCursor)
	}

	plain := v.RenderANSI()
	if strings.Contains(plain[0], "\x1b[0;7m") {
		t.Fatalf("RenderANSI must not draw a cursor, got %q", plain[0])
	}

	// Hidden cursor (ESC[?25l) must not be drawn.
	v.Write([]byte("\x1b[?25l"))
	hidden := v.RenderANSIWithCursor()
	if strings.Contains(hidden[0], "\x1b[0;7m") {
		t.Fatalf("hidden cursor must not be drawn, got %q", hidden[0])
	}
}

// TestRenderANSIFaintAttribute verifies the SGR faint/dim attribute (ESC[2m) is
// preserved through vt10x parsing and re-emitted by the renderer, and that a
// reset clears it without bleeding onto following text. Ink-based apps such as
// Claude Code render autosuggestion "ghost text" with dimColor (ESC[2m); before
// this was handled, the suggestion rendered at full intensity and looked like
// real input.
func TestRenderANSIFaintAttribute(t *testing.T) {
	v := New(2, 20)
	// "dim" is faint; the reset then a normal "X".
	v.Write([]byte("\x1b[2mdim\x1b[0mX"))

	row := v.RenderANSI()[0]
	// The dim run opens the row with reset+faint (ESC[0;2m). Without faint
	// support it would render as plain default text (ESC[0m).
	if !strings.HasPrefix(row, "\x1b[0;2m") {
		t.Fatalf("expected dim run to open with reset+faint (ESC[0;2m), got %q", row)
	}
	// 'X' follows ESC[0m, so faint must not bleed onto it.
	di := strings.Index(row, "dim")
	xi := strings.Index(row, "X")
	if di < 0 || xi <= di || !strings.Contains(row[di:xi], "\x1b[0m") {
		t.Fatalf("expected a reset between dim text and X (no faint bleed), got %q", row)
	}
}

// TestFaintClearedByNormalIntensity verifies SGR 22 (normal intensity) turns
// faint back off, matching ECMA-48 (22 clears both bold and faint).
func TestFaintClearedByNormalIntensity(t *testing.T) {
	v := New(2, 20)
	v.Write([]byte("\x1b[2mA\x1b[22mB"))
	row := v.RenderANSI()[0]
	// 'A' is faint, 'B' is not. After "A" there must be a non-faint run for B.
	ai := strings.Index(row, "A")
	bi := strings.Index(row, "B")
	if ai < 0 || bi <= ai {
		t.Fatalf("unexpected layout: %q", row)
	}
	// The B run is reset-prefixed and carries no faint param.
	if !strings.Contains(row[ai:bi], "\x1b[0m") {
		t.Fatalf("expected a reset before B (faint cleared by SGR 22), got %q", row)
	}
}
