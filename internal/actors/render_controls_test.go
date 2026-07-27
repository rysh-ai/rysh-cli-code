package actors

import (
	"strings"
	"testing"
)

// TestControlCharsNeverReachTheTerminal covers the `cat a logfile` corruption:
// the escape-sequence regexes only match SEQUENCES, so a lone control byte in a
// file used to travel all the way to the user's real terminal (buildPanePanel
// renders pane.Output verbatim so colours survive). SO (0x0e) switches the
// terminal to the line-drawing charset GLOBALLY, which is why panes that never
// ran the command were corrupted too.
func TestControlCharsNeverReachTheTerminal(t *testing.T) {
	dangerous := []struct {
		name string
		b    byte
	}{
		{"NUL", 0x00}, {"VT", 0x0b}, {"FF", 0x0c},
		{"SO alt-charset", 0x0e}, {"SI", 0x0f},
		{"CAN", 0x18}, {"SUB", 0x1a}, {"DEL", 0x7f},
	}
	for _, d := range dangerous {
		in := "2026-07-22 INFO request" + string(rune(d.b)) + "handled\n"
		for _, tc := range []struct{ path, got string }{
			{"stripAnsiEscapes", stripAnsiEscapes(in)},
			{"sanitizeShellChunk", sanitizeShellChunk(in)},
		} {
			if strings.ContainsRune(tc.got, rune(d.b)) {
				t.Errorf("%s: %s (0x%02x) survived into the pane buffer: %q",
					tc.path, d.name, d.b, tc.got)
			}
		}
	}
}

// Line structure and column alignment must survive — dropping \t would wreck
// ls/git output, and dropping \n would destroy the buffer entirely.
func TestNewlineAndTabPreserved(t *testing.T) {
	in := "a\tb\nc\td\n"
	for _, tc := range []struct{ path, got string }{
		{"stripAnsiEscapes", stripAnsiEscapes(in)},
		{"sanitizeShellChunk", sanitizeShellChunk(in)},
	} {
		if tc.got != in {
			t.Errorf("%s: mangled tabs/newlines: got %q, want %q", tc.path, tc.got, in)
		}
	}
}

// Colour output is the whole point of sanitizeShellChunk — the control filter
// must step over preserved SGR sequences rather than eating their ESC.
func TestSGRColoursStillPreserved(t *testing.T) {
	in := "\x1b[31mred\x1b[0m plain\n"
	if got := sanitizeShellChunk(in); got != in {
		t.Errorf("SGR colours lost: got %q, want %q", got, in)
	}
	// ...while the plain path still strips them.
	if got := stripAnsiEscapes(in); got != "red plain\n" {
		t.Errorf("plain path: got %q, want %q", got, "red plain\n")
	}
}

// Multi-byte UTF-8 must not be shredded: C1 controls are tested as runes, so
// continuation bytes (0x80-0xBF) have to survive intact.
func TestUTF8SurvivesControlFilter(t *testing.T) {
	in := "héllo → 世界 ✓\n"
	for _, tc := range []struct{ path, got string }{
		{"stripAnsiEscapes", stripAnsiEscapes(in)},
		{"sanitizeShellChunk", sanitizeShellChunk(in)},
	} {
		if tc.got != in {
			t.Errorf("%s: UTF-8 corrupted: got %q, want %q", tc.path, tc.got, in)
		}
	}
	// A real C1 control (U+0085 NEL) is still dropped.
	if got := stripAnsiEscapes("ab"); strings.ContainsRune(got, 0x85) {
		t.Errorf("C1 control survived: %q", got)
	}
}

// TestSplitTrailingPartial covers the two destructive chunk tails.
func TestSplitTrailingPartial(t *testing.T) {
	cases := []struct{ name, in, emit, hold string }{
		// The PTY maps \n to \r\n; a read landing between them must not let
		// collapseCarriageReturns treat the CR as an in-place line rewrite.
		{"trailing CR of a CRLF pair", "line one\r\nline two\r", "line one\r\nline two", "\r"},
		{"no trailing CR", "line one\nline two", "line one\nline two", ""},
		// A lone ESC must not be emitted, or it recombines with the next
		// chunk's bytes into a live escape sequence in the buffer.
		{"bare trailing ESC", "log tail\x1b", "log tail", "\x1b"},
		{"incomplete CSI", "log tail\x1b[2", "log tail", "\x1b[2"},
		{"complete CSI stays", "log \x1b[2Jtail", "log \x1b[2Jtail", ""},
		{"complete SGR stays", "\x1b[31mred", "\x1b[31mred", ""},
		{"incomplete OSC", "x\x1b]0;title", "x", "\x1b]0;title"},
		{"complete OSC stays", "x\x1b]0;t\x07y", "x\x1b]0;t\x07y", ""},
		{"empty", "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			emit, hold := splitTrailingPartial(c.in)
			if emit != c.emit || hold != c.hold {
				t.Fatalf("splitTrailingPartial(%q) = (%q, %q), want (%q, %q)",
					c.in, emit, hold, c.emit, c.hold)
			}
		})
	}
}

// A pathological run of ESCs must not buffer without bound.
func TestSplitTrailingPartialBounded(t *testing.T) {
	in := "text" + strings.Repeat("\x1b", 500)
	_, hold := splitTrailingPartial(in)
	if len(hold) > 64 {
		t.Fatalf("held tail unbounded: %d bytes", len(hold))
	}
}

// TestCRLFSplitKeepsBothLines is the end-to-end regression for the silent line
// loss: with the tail carried across reads, no output line disappears when a
// read boundary falls inside a \r\n pair.
func TestCRLFSplitKeepsBothLines(t *testing.T) {
	// One PTY read ends between the \r and the \n of "line two"'s CRLF.
	chunkA, chunkB := "line one\r\nline two\r", "\nline three\r\n"

	var out strings.Builder
	var pending string
	for _, raw := range []string{chunkA, chunkB} {
		text := pending + raw
		text, pending = splitTrailingPartial(text)
		out.WriteString(sanitizeShellChunk(text))
	}

	got := out.String()
	for _, want := range []string{"line one", "line two", "line three"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q was silently dropped at the chunk boundary; got %q", want, got)
		}
	}
}

// A split escape must never reassemble into a live sequence in the buffer:
// "log tail\x1b" + "[2Jmore" used to become a real erase-display aimed at the
// user's terminal.
func TestSplitEscapeNeverReassemblesLive(t *testing.T) {
	var out strings.Builder
	var pending string
	for _, raw := range []string{"log tail\x1b", "[2Jmore log\n"} {
		text := pending + raw
		text, pending = splitTrailingPartial(text)
		out.WriteString(sanitizeShellChunk(text))
	}
	if got := out.String(); strings.Contains(got, "\x1b[2J") {
		t.Fatalf("split escape reassembled into a live sequence: %q", got)
	}
}
