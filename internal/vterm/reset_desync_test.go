package vterm

import (
	"strings"
	"testing"
)

// screen joins the rendered rows so tests can assert on visible text.
func screen(v *VTerm) string { return strings.Join(v.Render(), "\n") }

// screenFlat joins the rows with no separator, so text that the emulator wrapped
// across a row boundary can still be matched as one contiguous string.
func screenFlat(v *VTerm) string {
	var b strings.Builder
	for _, row := range v.Render() {
		b.WriteString(strings.TrimRight(row, " "))
	}
	return b.String()
}

// TestRISResyncsAltScreenLatch covers the RIS (ESC c) desync.
//
// vt10x's reset() assigns t.mode = ModeWrap directly, clearing ModeAltScreen
// without passing through setMode — so the wrapper's v.inAlt latch never saw
// it. With the two out of step, the next genuine alt-screen EXIT was forwarded
// while vt10x believed it was NOT in alt, and vt10x swaps on `!set || !alt`:
// the exit ENTERED an empty alt buffer and everything written afterwards was
// invisible. `cat` of a binary file, and reset/tput reset, emit RIS.
func TestRISResyncsAltScreenLatch(t *testing.T) {
	v := New(24, 80)

	v.Write([]byte("\x1b[?1049h"))
	if !v.IsAltScreen() {
		t.Fatal("setup: expected alt screen after ?1049h")
	}

	// RIS clears vt10x's alt bit behind the wrapper's back.
	v.Write([]byte("\x1bc"))
	if v.IsAltScreen() {
		t.Fatal("after RIS: expected alt screen cleared")
	}

	// The bug: this normal EXIT used to flip the terminal back INTO alt.
	v.Write([]byte("\x1b[?1049l"))
	if v.IsAltScreen() {
		t.Fatal("alt-screen EXIT after RIS re-entered the alt buffer (latch desync)")
	}

	// ...and output written afterwards must be visible.
	v.Write([]byte("VISIBLE AFTER RESET\n"))
	if !strings.Contains(screen(v), "VISIBLE AFTER RESET") {
		t.Fatalf("output after RIS + alt exit is invisible; screen:\n%s", screen(v))
	}
}

// RIS mid-stream (as in a binary dump) must resync just the same.
func TestRISInlineWithSurroundingOutput(t *testing.T) {
	v := New(24, 80)
	v.Write([]byte("\x1b[?1049h"))
	v.Write([]byte("junk\x1bcmore junk\n"))
	v.Write([]byte("\x1b[?1049l"))
	if v.IsAltScreen() {
		t.Fatal("inline RIS did not resync the alt latch")
	}
	v.Write([]byte("STILL VISIBLE\n"))
	if !strings.Contains(screen(v), "STILL VISIBLE") {
		t.Fatalf("output invisible after inline RIS; screen:\n%s", screen(v))
	}
}

// A normal alt enter/exit cycle must still work — the RIS handling must not
// disturb the ordinary path.
func TestAltScreenCycleStillWorks(t *testing.T) {
	v := New(24, 80)
	if v.IsAltScreen() {
		t.Fatal("fresh terminal should not be in alt screen")
	}
	v.Write([]byte("\x1b[?1049h"))
	if !v.IsAltScreen() {
		t.Fatal("enter alt failed")
	}
	v.Write([]byte("\x1b[?1049l"))
	if v.IsAltScreen() {
		t.Fatal("exit alt failed")
	}
	// A redundant exit must still be dropped rather than re-entering alt.
	v.Write([]byte("\x1b[?1049l"))
	if v.IsAltScreen() {
		t.Fatal("redundant alt reset re-entered the alt buffer")
	}
}

// TestUnterminatedPrivateCSIKeepsFollowingOutput covers the truncation bug:
// stripUnsupportedCSI skipped to j == len(p) when a private CSI (ESC[> ...)
// had no final byte in the buffer, silently discarding everything after it.
// Write's pending buffer normally hides this, but it gives up past
// maxPendingEscape (256), so a long enough run reaches the filter.
func TestUnterminatedPrivateCSIKeepsFollowingOutput(t *testing.T) {
	// Longer than maxPendingEscape so Write flushes it unterminated.
	long := "\x1b[>" + strings.Repeat("1", 400)
	v := New(24, 80)
	v.Write([]byte(long))
	v.Write([]byte("u REAL OUTPUT HERE\n"))

	// Matched against the de-wrapped screen: the 400 junk bytes fill the first
	// row, so the real output legitimately wraps. What must never happen is the
	// text being DISCARDED, which is what the old skip-to-end-of-buffer did.
	if got := screenFlat(v); !strings.Contains(got, "REAL OUTPUT HERE") {
		t.Fatalf("output after an unterminated private CSI was discarded; screen:\n%s", screen(v))
	}
}

// The filter itself must not swallow the buffer when no terminator is present.
//
// Note a CSI ends at the first byte in 0x40-0x7E, so any ordinary letter
// terminates it — a genuinely unterminated run is one made purely of parameter
// bytes (digits and ';'), which is exactly what overruns maxPendingEscape.
func TestStripUnsupportedCSIUnterminatedPassesThrough(t *testing.T) {
	// Unterminated: parameter bytes only, no final byte anywhere in the buffer.
	in := []byte("before\x1b[>" + strings.Repeat("1", 300))
	if got := string(stripUnsupportedCSI(in)); got != string(in) {
		t.Fatalf("unterminated private CSI truncated the buffer:\n got %d bytes\nwant %d bytes", len(got), len(in))
	}

	// A properly terminated private CSI is still stripped.
	if got := string(stripUnsupportedCSI([]byte("before\x1b[>1uafter"))); got != "beforeafter" {
		t.Fatalf("terminated private CSI not stripped: %q", got)
	}

	// Any final byte terminates, including an ordinary letter.
	if got := string(stripUnsupportedCSI([]byte("a\x1b[>99tb"))); got != "ab" {
		t.Fatalf("letter-terminated private CSI not stripped: %q", got)
	}
}
