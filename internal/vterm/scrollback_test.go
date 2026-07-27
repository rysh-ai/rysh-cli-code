package vterm

import (
	"fmt"
	"strings"
	"testing"
)

// writeLines feeds n numbered lines into the emulator.
func writeLines(t *testing.T, vt *VTerm, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		if _, err := vt.Write([]byte(fmt.Sprintf("line%d\r\n", i))); err != nil {
			t.Fatalf("write line %d: %v", i, err)
		}
	}
}

// TestScrollbackCapturesEvictedMainScreenLines verifies that lines scrolling off
// the top of the main screen are retained, oldest first, and that the current
// screen no longer holds them.
func TestScrollbackCapturesEvictedMainScreenLines(t *testing.T) {
	vt := New(24, 80)

	writeLines(t, vt, 50) // 50 lines on a 24-row screen -> ~26 evicted

	if got := vt.ScrollbackLen(); got < 20 {
		t.Fatalf("ScrollbackLen = %d, want >= 20 evicted lines", got)
	}

	sb := vt.ScrollbackANSI()
	if len(sb) == 0 || !containsExactLine(sb[0], "line1") {
		t.Fatalf("oldest scrollback row = %q, want exactly line1", firstOr(sb))
	}

	// The live screen shows recent lines, not the evicted ones.
	screen := strings.Join(vt.RenderANSI(), "\n")
	if !containsExactLine(screen, "line50") {
		t.Fatalf("current screen should contain line50:\n%s", screen)
	}
	if containsExactLine(screen, "line1") {
		t.Fatalf("current screen should not still contain evicted line1:\n%s", screen)
	}
}

// TestScrollbackSkipsAlternateScreen verifies that full-screen alternate-screen
// programs (vim, etc.) do not pollute scrollback history.
func TestScrollbackSkipsAlternateScreen(t *testing.T) {
	vt := New(24, 80)

	if _, err := vt.Write([]byte("\x1b[?1049h")); err != nil {
		t.Fatalf("enter alt screen: %v", err)
	}
	writeLines(t, vt, 50)

	if got := vt.ScrollbackLen(); got != 0 {
		t.Fatalf("ScrollbackLen on alt screen = %d, want 0 (no capture)", got)
	}
}

// TestSetScrollbackMaxBounds verifies the ring is bounded and keeps newest lines.
func TestSetScrollbackMaxBounds(t *testing.T) {
	vt := New(24, 80)
	vt.SetScrollbackMax(10)

	writeLines(t, vt, 100)

	if got := vt.ScrollbackLen(); got != 10 {
		t.Fatalf("ScrollbackLen = %d, want capped at 10", got)
	}
	// A cap of 10 must retain the newest evicted lines, dropping the oldest.
	sb := vt.ScrollbackANSI()
	if containsExactLine(strings.Join(sb, "\n"), "line1") {
		t.Fatalf("capped scrollback should have dropped line1; oldest is %q", sb[0])
	}
}

func firstOr(s []string) string {
	if len(s) == 0 {
		return "<empty>"
	}
	return s[0]
}

// containsExactLine reports whether any ANSI-stripped line equals exactly want
// (so "line1" does not match "line10").
func containsExactLine(joined, want string) bool {
	for _, ln := range strings.Split(joined, "\n") {
		if strings.TrimSpace(stripANSITest(ln)) == want {
			return true
		}
	}
	return false
}

// stripANSITest is a minimal SGR stripper for tests.
func stripANSITest(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
