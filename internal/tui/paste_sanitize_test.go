// SPDX-License-Identifier: Apache-2.0

package tui

import "testing"

// TestSanitizePastedText verifies that clipboard payloads carrying the control
// characters which break the pane layout (tabs, CR, and raw escape sequences)
// are neutralized before entering a chat/AI message buffer, while newlines and
// ordinary unicode text are preserved.
func TestSanitizePastedText(t *testing.T) {
	// Tab expansion, CRLF normalization, and stripping of an erase-line
	// (\x1b[2K) + cursor-forward (\x1b[10C) sequence and a BEL (\x07).
	got := sanitizePastedText("col1\tcol2\r\nline2\x1b[2K\x1b[10Cend\x07")
	want := "col1    col2\nline2end"
	if got != want {
		t.Fatalf("sanitizePastedText mismatch:\n got %q\nwant %q", got, want)
	}

	// No control characters may survive except newline — otherwise the render
	// path could still be desynced or the cursor moved.
	for _, r := range got {
		if r == '\n' {
			continue
		}
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			t.Fatalf("residual control char %#x in sanitized output %q", r, got)
		}
	}

	// A lone CR is turned into a newline, not dropped.
	if out := sanitizePastedText("a\rb"); out != "a\nb" {
		t.Fatalf("lone CR handling: got %q, want %q", out, "a\nb")
	}

	// Multi-line pastes and non-ASCII text pass through untouched.
	if out := sanitizePastedText("héllo\nwörld"); out != "héllo\nwörld" {
		t.Fatalf("unicode/newline mangled: got %q", out)
	}

	// Plain single-line text is unchanged.
	if out := sanitizePastedText("just a normal message"); out != "just a normal message" {
		t.Fatalf("plain text altered: got %q", out)
	}
}
