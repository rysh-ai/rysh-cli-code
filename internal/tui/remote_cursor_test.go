package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// TestStampEdgeCursor verifies the off-screen-cursor marker keeps a visible
// reverse-video cursor clamped to the right edge at exactly contentCols width.
// This is what keeps a shared interactive pane (e.g. Claude Code) showing a
// cursor when the source pane is wider than the subscriber and the cursor sits
// at/beyond the visible width, where plain ansi.Truncate would drop it.
func TestStampEdgeCursor(t *testing.T) {
	long := "\x1b[0m" + strings.Repeat("x", 100) + "\x1b[0m"
	cases := []struct {
		name string
		line string
		cols int
		want int // expected visible width
	}{
		{"wide line cols20", long, 20, 20},
		{"wide line cols8", long, 8, 8},
		{"wide line cols2", long, 2, 2},
		{"degenerate cols1", long, 1, 1},
		{"short line padded to edge", "\x1b[0mhi\x1b[0m", 10, 10},
	}
	for _, c := range cases {
		out := stampEdgeCursor(c.line, c.cols)
		if !strings.Contains(out, "\x1b[7m") {
			t.Errorf("%s: missing reverse-video cursor marker: %q", c.name, out)
		}
		if w := ansi.StringWidth(out); w != c.want {
			t.Errorf("%s: visible width = %d, want %d: %q", c.name, w, c.want, out)
		}
	}
}
