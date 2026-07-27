package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// TestWrapRows verifies that output is split into on-screen rows that respect
// the content width, preserve blank lines, and keep the right-align sentinel on
// the first row of its logical line.
func TestWrapRows(t *testing.T) {
	// A logical line wider than the width must split into multiple rows, each no
	// wider than the limit.
	long := strings.Repeat("x", 25)
	rows := wrapRows(long, 10)
	if len(rows) != 3 { // 25 / 10 -> 3 rows (10, 10, 5)
		t.Fatalf("expected 3 wrapped rows, got %d: %v", len(rows), rows)
	}
	for _, r := range rows {
		if ansi.StringWidth(r) > 10 {
			t.Fatalf("row exceeds width 10: %q (w=%d)", r, ansi.StringWidth(r))
		}
	}

	// Blank logical lines are preserved as a single blank row each.
	rows = wrapRows("a\n\nb", 10)
	if len(rows) != 3 || rows[1] != "" {
		t.Fatalf("expected blank line preserved, got %v", rows)
	}

	// The right-align sentinel stays on the (single) row of a short line.
	rows = wrapRows("\x02hint", 10)
	if len(rows) != 1 || !strings.HasPrefix(rows[0], "\x02") {
		t.Fatalf("expected sentinel preserved on first row, got %v", rows)
	}

	// width <= 0 falls back to one row per logical line (no wrapping).
	rows = wrapRows("aaaa\nbbbb", 0)
	if len(rows) != 2 {
		t.Fatalf("expected 2 unwrapped rows, got %d: %v", len(rows), rows)
	}
}

// TestVisibleRowWindow verifies the bottom-anchored windowing used by both the
// renderer and the scroll clamps.
func TestVisibleRowWindow(t *testing.T) {
	rows := make([]string, 10)
	for i := range rows {
		rows[i] = string(rune('0' + i))
	}

	// Tail (offset 0) shows the last maxLines rows.
	got := visibleRowWindow(rows, 4, 0)
	if strings.Join(got, "") != "6789" {
		t.Fatalf("offset 0: expected 6789, got %q", strings.Join(got, ""))
	}

	// Scrolled up by 2 rows.
	got = visibleRowWindow(rows, 4, 2)
	if strings.Join(got, "") != "4567" {
		t.Fatalf("offset 2: expected 4567, got %q", strings.Join(got, ""))
	}

	// Over-scroll clamps to the top.
	got = visibleRowWindow(rows, 4, 100)
	if strings.Join(got, "") != "0123" {
		t.Fatalf("over-scroll: expected 0123, got %q", strings.Join(got, ""))
	}

	// Budget larger than content returns everything unchanged.
	got = visibleRowWindow(rows, 20, 0)
	if len(got) != 10 {
		t.Fatalf("budget>=len: expected all 10 rows, got %d", len(got))
	}
}

// TestPaneRowCountCountsWrappedRows guards against the original bug: scroll math
// counted raw newline-delimited lines, so wrapped output could not be scrolled
// back fully. The count must reflect on-screen rows.
func TestPaneRowCountCountsWrappedRows(t *testing.T) {
	// Build once to learn the pane's content width, then build again with output
	// sized to that width so each logical line wraps into exactly three rows.
	probe := buildTestModel(120, 30, "seed")
	cw := probe.paneContentWidth("pane-1")
	if cw <= 0 {
		t.Fatalf("expected positive content width, got %d", cw)
	}

	line := strings.Repeat("z", cw*3) // wraps into 3 rows
	output := line + "\n" + line      // 2 raw lines -> 6 on-screen rows
	m := buildTestModel(120, 30, output)

	if got := m.paneRowCount("pane-1"); got != 6 {
		t.Fatalf("expected 6 wrapped rows, got %d (raw line count is 2)", got)
	}
}

// TestBuildDisplayLinesBoundedByHeight guards against the overflow bug: wrapped
// output used to be sliced as raw lines and then wrapped, which could emit more
// rows than the pane height. The visible block must equal the pane height
// exactly so it never spills past the border.
func TestBuildDisplayLinesBoundedByHeight(t *testing.T) {
	probe := buildTestModel(120, 30, "seed")
	cw := probe.paneContentWidth("pane-1")
	height := probe.paneVisibleLines("pane-1") + 4 // pane rect height

	// Many long lines that each wrap several times — far more rows than fit.
	long := strings.Repeat("w", cw*4)
	output := strings.TrimRight(strings.Repeat(long+"\n", 40), "\n")

	pane := probe.findPaneInSnapshot("pane-1")
	pane.Output = output
	tab := probe.snapshot.Tabs[0]

	lines := buildDisplayLines(*pane, tab, cw, height, "", 0, "shell")
	if len(lines) != height {
		t.Fatalf("expected exactly %d display rows, got %d", height, len(lines))
	}
}

// TestScrollClampUsesWrappedRows verifies that scrolling to the top lands on the
// maximum offset measured in wrapped rows, not raw lines.
func TestScrollClampUsesWrappedRows(t *testing.T) {
	probe := buildTestModel(120, 30, "seed")
	cw := probe.paneContentWidth("pane-1")

	line := strings.Repeat("q", cw*2)                                // 2 rows per logical line
	output := strings.TrimRight(strings.Repeat(line+"\n", 30), "\n") // 30 lines -> 60 rows
	m := buildTestModel(120, 30, output)

	totalRows := m.paneRowCount("pane-1")
	visible := m.paneVisibleLines("pane-1")
	wantMax := totalRows - visible
	if wantMax < 0 {
		wantMax = 0
	}

	m.scrollActivePaneToTop()
	if got := m.scrollOffset("pane-1"); got != wantMax {
		t.Fatalf("scroll-to-top offset = %d, want %d (total rows %d, visible %d)", got, wantMax, totalRows, visible)
	}

	// Scrolling down past the tail clamps to 0.
	m.scrollActivePaneDown(totalRows + 100)
	if got := m.scrollOffset("pane-1"); got != 0 {
		t.Fatalf("scroll past tail: offset = %d, want 0", got)
	}
}

// TestKeystrokeSnapsScrollToBottom guards the scroll-on-keystroke behaviour:
// typing into a scrolled-up pane's prompt returns the view to the tail, so the
// next command's output is always in view (the "scroll does not follow the
// last command" bug). Input-mode switches share the per-pane offset across
// different-length buffers, so they snap to the tail too.
func TestKeystrokeSnapsScrollToBottom(t *testing.T) {
	output := strings.TrimRight(strings.Repeat("line\n", 100), "\n")
	m := buildTestModel(120, 30, output)
	m.inputs["pane-1"] = textinput.New()

	m.scrollActivePaneUp(10)
	if m.scrollOffset("pane-1") == 0 {
		t.Fatalf("precondition: expected a scrolled-up pane")
	}

	mm, _ := m.updateActivePaneInput(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	m = mm.(Model)
	if got := m.scrollOffset("pane-1"); got != 0 {
		t.Fatalf("keystroke: offset = %d, want 0 (snap to tail)", got)
	}

	m.scrollActivePaneUp(10)
	m.setActiveInputMode("prompt")
	if got := m.scrollOffset("pane-1"); got != 0 {
		t.Fatalf("mode switch: offset = %d, want 0 (snap to tail)", got)
	}
}
