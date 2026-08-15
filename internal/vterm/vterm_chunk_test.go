// SPDX-License-Identifier: Apache-2.0

package vterm

import (
	"strings"
	"testing"
)

// buildInteractiveStream returns a top/vim-like byte stream that mixes
// alt-screen entry, absolute cursor positioning, SGR colour runs, line clears,
// cursor hide/show, DEC private modes, and a multibyte UTF-8 glyph — the kinds of
// sequences whose mis-parse (when split across a Write boundary) drifts the
// cursor and scrambles an interactive mirror.
func buildInteractiveStream() []byte {
	var b strings.Builder
	b.WriteString("\x1b[?1049h") // enter alt screen
	b.WriteString("\x1b[?25l")   // hide cursor
	b.WriteString("\x1b[H\x1b[2J")
	b.WriteString("\x1b[1;1HProcesses: 1297 total, 3 running")
	b.WriteString("\x1b[2;1H\x1b[7mLoad Avg: 1.23, 4.02, 4.06\x1b[0m  CPU 15.2% user")
	b.WriteString("\x1b[3;1HSharedLibs: 1228M resident, 190M data — café ☕ │ box")
	b.WriteString("\x1b[4;1H\x1b[1;32mPID\x1b[0m  COMMAND      %CPU")
	b.WriteString("\x1b[5;1H87800  ry           29.0")
	b.WriteString("\x1b[6;1H\x1b[Ktrailing-after-clear")
	// Re-position and overwrite (incremental update — the pattern that drifts).
	b.WriteString("\x1b[2;11H4.99, 3.10, 2.00")
	b.WriteString("\x1b[1;12H9999")
	b.WriteString("\x1b[?25h") // show cursor
	b.WriteString("\x1b[7;3H")
	return []byte(b.String())
}

// chunkAtBoundaries splits p at the given absolute offsets (adversarially chosen
// to land mid-escape-sequence and mid-UTF-8).
func chunkAtBoundaries(p []byte, offsets []int) [][]byte {
	var out [][]byte
	prev := 0
	for _, off := range offsets {
		if off <= prev || off >= len(p) {
			continue
		}
		out = append(out, p[prev:off])
		prev = off
	}
	out = append(out, p[prev:])
	return out
}

func renderChunked(t *testing.T, rows, cols int, chunks [][]byte) []string {
	t.Helper()
	vt := New(rows, cols)
	for _, c := range chunks {
		if _, err := vt.Write(c); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	return vt.Render()
}

// TestWriteChunkBoundaryInvariance is the regression guard for the mirror-scramble
// root cause: feeding identical bytes in different Write() chunkings must produce
// an identical rendered screen. A source emulator is fed PTY-read chunks while a
// share mirror is fed coalesced frame chunks — before the pending-sequence buffer
// in Write, a sequence split at a frame boundary mis-parsed and the mirror drifted.
func TestWriteChunkBoundaryInvariance(t *testing.T) {
	const rows, cols = 24, 80
	stream := buildInteractiveStream()

	// Reference: the whole stream in a single Write.
	want := renderChunked(t, rows, cols, [][]byte{stream})

	cases := []struct {
		name   string
		chunks [][]byte
	}{
		{
			name:   "byte-by-byte (every sequence split)",
			chunks: chunkAtBoundaries(stream, allOffsets(len(stream))),
		},
		{
			name: "split mid-CSI and mid-UTF-8",
			// Offsets chosen to land inside escape sequences and the café/☕ glyphs.
			chunks: chunkAtBoundaries(stream, []int{3, 9, 14, 40, 70, 95, 130, 180}),
		},
		{
			name:   "two-byte chunks",
			chunks: fixedChunks(stream, 2),
		},
		{
			name:   "seven-byte chunks",
			chunks: fixedChunks(stream, 7),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderChunked(t, rows, cols, tc.chunks)
			if len(got) != len(want) {
				t.Fatalf("row count = %d, want %d", len(got), len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("row %d differs under %q chunking:\n got = %q\nwant = %q",
						i, tc.name, got[i], want[i])
				}
			}
		})
	}
}

func allOffsets(n int) []int {
	out := make([]int, 0, n)
	for i := 1; i < n; i++ {
		out = append(out, i)
	}
	return out
}

func fixedChunks(p []byte, size int) [][]byte {
	var out [][]byte
	for i := 0; i < len(p); i += size {
		end := i + size
		if end > len(p) {
			end = len(p)
		}
		out = append(out, p[i:end])
	}
	return out
}

// TestSplitTrailingIncompleteEscape covers the helper directly.
func TestSplitTrailingIncompleteEscape(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int // -1 means "== len(in)"
	}{
		{"no esc", "plain text", -1},
		{"complete CSI", "\x1b[2J", -1},
		{"complete CSI then text", "\x1b[2JABC", -1},
		{"incomplete CSI no final", "\x1b[12;3", 0},
		{"lone trailing ESC", "abc\x1b", 3},
		{"text then ESC[", "hello\x1b[", 5},
		{"complete then incomplete", "\x1b[2J\x1b[?25", 4},
		{"incomplete OSC", "\x1b]0;title", 0},
		{"complete OSC BEL", "\x1b]0;title\x07", -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			want := tc.want
			if want == -1 {
				want = len(tc.in)
			}
			if got := splitTrailingIncompleteEscape([]byte(tc.in)); got != want {
				t.Errorf("splitTrailingIncompleteEscape(%q) = %d, want %d", tc.in, got, want)
			}
		})
	}
}

// TestSplitTrailingIncompleteUTF8 covers the multibyte helper directly.
func TestSplitTrailingIncompleteUTF8(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		want int // -1 means "== len(in)"
	}{
		{"ascii", []byte("abc"), -1},
		{"complete 2-byte", []byte("a\xc3\xa9"), -1},    // é
		{"complete 3-byte", []byte("\xe2\x98\x95"), -1}, // ☕
		{"incomplete 2-byte lead", []byte("ab\xc3"), 2}, // lead, no cont
		{"incomplete 3-byte one cont", []byte("\xe2\x98"), 0},
		{"incomplete 4-byte lead", []byte("x\xf0\x9f"), 1}, // emoji lead + 1 cont
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			want := tc.want
			if want == -1 {
				want = len(tc.in)
			}
			if got := splitTrailingIncompleteUTF8(tc.in); got != want {
				t.Errorf("splitTrailingIncompleteUTF8(%q) = %d, want %d", tc.in, got, want)
			}
		})
	}
}
