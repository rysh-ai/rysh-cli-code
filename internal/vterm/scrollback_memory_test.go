// SPDX-License-Identifier: Apache-2.0

package vterm

import (
	"strings"
	"testing"
)

// scrollbackGlyphs is the number of glyphs the emulator holds in scrollback —
// the quantity that costs memory (16 bytes each).
func scrollbackGlyphs(v *VTerm) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	n := 0
	for _, ln := range v.term.Scrollback() {
		n += len(ln)
	}
	return n
}

// TestScrollbackTrimsErasedLines pins the memory bound for inline TUIs.
//
// Claude Code erases every row it paints (ESC[2K) and vt10x's clear() fills
// erased cells with ' ', not 0. The scrollback capture used to trim only Char 0
// cells, so every evicted row was stored at full terminal width: at 242 columns
// a 2000-line ring held ~484k glyphs (~7.7MB) per pane even when each row
// carried a few dozen characters. With 30 claude panes that is ~230MB of
// blank cells in the daemon.
func TestScrollbackTrimsErasedLines(t *testing.T) {
	const cols, lines = 242, 500
	text := "⏺ Read internal/actors/pane.go"
	vt := New(10, cols)
	var b strings.Builder
	for i := 0; i < lines; i++ {
		b.WriteString("\x1b[2K\x1b[38;5;174m")
		b.WriteString(text)
		b.WriteString("\x1b[0m\r\n")
	}
	if _, err := vt.Write([]byte(b.String())); err != nil {
		t.Fatal(err)
	}
	n := vt.ScrollbackLen()
	if n < lines-10 {
		t.Fatalf("ScrollbackLen = %d, want ~%d", n, lines)
	}
	per := scrollbackGlyphs(vt) / n
	if want := len([]rune(text)); per > want {
		t.Fatalf("scrollback stores %d glyphs per row, want <= %d (the text): erased trailing cells are being retained (terminal width is %d)", per, want, cols)
	}
}

// TestScrollbackTrimKeepsVisibleTrailingCells guards the other side: a trailing
// space with a background colour or reverse video is visible, and must survive.
func TestScrollbackTrimKeepsVisibleTrailingCells(t *testing.T) {
	vt := New(4, 80)
	// "> " followed by a reverse-video space (Claude Code's input cursor), then
	// a red-background run of spaces.
	in := "\x1b[2K> \x1b[7m \x1b[0m\r\n" +
		"\x1b[2Kx\x1b[41m   \x1b[0m\r\n"
	if _, err := vt.Write([]byte(in)); err != nil {
		t.Fatal(err)
	}
	writeLines(t, vt, 8)
	got := strings.Join(vt.ScrollbackANSI(), "\n")
	if !strings.Contains(got, "> \x1b[0;7m \x1b[0m") {
		t.Errorf("reverse-video trailing space was trimmed:\n%q", got)
	}
	if !strings.Contains(got, "x\x1b[0;41m   \x1b[0m") {
		t.Errorf("background-coloured trailing spaces were trimmed:\n%q", got)
	}
}
