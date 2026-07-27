package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

func init() {
	// Force a color profile so lipgloss emits ANSI styling (including reverse
	// video for selections) in the test environment, which otherwise has no TTY.
	lipgloss.SetColorProfile(termenv.TrueColor)
}

// buildTestModel constructs a minimal Model with a single tab/lane/group/pane
// whose output is the given lines. No NATS bus is required.
func buildTestModel(width, height int, output string) Model {
	paneID := "pane-1"
	pane := domain.PaneSnapshot{
		ID:           paneID,
		Title:        "p1",
		Output:       output,
		Status:       "ready",
		ProviderName: "claude",
		Flex:         1,
	}
	tab := domain.TabSnapshot{
		ID:           "tab-1",
		Title:        "t1",
		ActivePaneID: paneID,
		Lanes: []domain.LaneSnapshot{
			{
				ID:           "lane-1",
				Flex:         1,
				ActivePaneID: paneID,
				PaneGroups: []domain.PaneGroupSnapshot{
					{ID: "g-1", RowFlex: 1, ActivePaneID: paneID, Panes: []domain.PaneSnapshot{pane}},
				},
			},
		},
	}
	m := Model{
		snapshot: domain.WorkspaceSnapshot{
			Tabs:         []domain.TabSnapshot{tab},
			ActiveTabID:  "tab-1",
			ActivePaneID: paneID,
		},
		mode:              modeNormal,
		width:             width,
		height:            height,
		inputs:            map[string]textinput.Model{},
		paneInputModes:    map[string]string{},
		panePastedText:    map[string]string{},
		paneHistoryIdx:    map[string]int{},
		paneHistorySaved:  map[string]string{},
		paneScrollOffsets: map[string]int{},
		pipelineOutputs:   map[string]string{},
		attentionState:    map[string]*attentionInfo{},
		attentionLastBell: nil,
	}
	m.recomputePaneRects()
	return m
}

// screenColSubstr returns the visible substring of an ANSI-stripped screen line
// spanning the cell columns [start, start+width).
func screenColSubstr(plainLine string, start, width int) string {
	runes := []rune(plainLine)
	if start >= len(runes) {
		return ""
	}
	end := start + width
	if end > len(runes) {
		end = len(runes)
	}
	return string(runes[start:end])
}

// buildTestModelMulti builds a model with N lanes (columns), each with one pane.
func buildTestModelMulti(width, height int, outputs []string) Model {
	tab := domain.TabSnapshot{ID: "tab-1", Title: "t1"}
	activeID := ""
	for i, out := range outputs {
		pid := "pane-" + string(rune('a'+i))
		if i == 0 {
			activeID = pid
			tab.ActivePaneID = pid
		}
		pane := domain.PaneSnapshot{ID: pid, Title: pid, Output: out, Status: "ready", ProviderName: "claude", Flex: 1}
		tab.Lanes = append(tab.Lanes, domain.LaneSnapshot{
			ID:           "lane-" + string(rune('a'+i)),
			Flex:         1,
			ActivePaneID: pid,
			PaneGroups:   []domain.PaneGroupSnapshot{{ID: "g-" + string(rune('a'+i)), RowFlex: 1, ActivePaneID: pid, Panes: []domain.PaneSnapshot{pane}}},
		})
	}
	m := Model{
		snapshot:          domain.WorkspaceSnapshot{Tabs: []domain.TabSnapshot{tab}, ActiveTabID: "tab-1", ActivePaneID: activeID},
		mode:              modeNormal,
		width:             width,
		height:            height,
		inputs:            map[string]textinput.Model{},
		paneInputModes:    map[string]string{},
		panePastedText:    map[string]string{},
		paneHistoryIdx:    map[string]int{},
		paneHistorySaved:  map[string]string{},
		paneScrollOffsets: map[string]int{},
		pipelineOutputs:   map[string]string{},
		attentionState:    map[string]*attentionInfo{},
	}
	m.recomputePaneRects()
	return m
}

// reverseRange returns the [start,end) cell-column range of reverse-video
// (selection-highlighted) cells in a single rendered (ANSI-containing) screen
// line. Returns (-1,-1) if no reverse cells are present.
func reverseRange(line string) (int, int) {
	var col, start, end int
	start, end = -1, -1
	reverse := false
	i := 0
	for i < len(line) {
		if line[i] == 0x1b {
			// consume CSI ... m
			j := i + 1
			if j < len(line) && line[j] == '[' {
				j++
				params := ""
				for j < len(line) && line[j] != 'm' && ((line[j] >= '0' && line[j] <= '9') || line[j] == ';') {
					params += string(line[j])
					j++
				}
				if j < len(line) && line[j] == 'm' {
					for _, p := range strings.Split(params, ";") {
						switch p {
						case "7":
							reverse = true
						case "0", "27", "":
							reverse = false
						}
					}
					i = j + 1
					continue
				}
			}
			i++
			continue
		}
		// printable cell
		_, size := decodeRune(line[i:])
		if reverse {
			if start == -1 {
				start = col
			}
			end = col + 1
		}
		col++
		i += size
	}
	return start, end
}

func decodeRune(s string) (rune, int) {
	for i, r := range s {
		_ = i
		// width 1 assumption for test data (ASCII)
		return r, len(string(r))
	}
	return 0, 1
}

// TestSelectionHighlightMatchesMouse simulates a multi-line drag selection and
// verifies the rendered reverse-video region matches the mouse start/end.
func TestSelectionHighlightMatchesMouse(t *testing.T) {
	output := "line0aaaa\nline1bbbb\nline2cccc\nline3dddd\nline4eeee\nline5ffff"
	m := buildTestModelMulti(120, 30, []string{output, output})

	// Select within the SECOND lane's pane.
	r := m.paneRects[1]

	// Mouse press at content (sx,sy)=(2,3) → output line "line1..." char index.
	// Drag to content (ex,ey)=(5,5).
	sx, sy := 2, 3
	ex, ey := 5, 5
	m.selection = mouseSelection{
		active:   true,
		dragging: true,
		dragged:  true,
		paneID:   r.paneID,
		startX:   sx,
		startY:   sy,
		endX:     ex,
		endY:     ey,
	}

	view := m.View()
	screenLines := strings.Split(view, "\n")

	for i := 0; i < r.h; i++ {
		screenRow := r.y + i
		if screenRow >= len(screenLines) {
			break
		}
		gotStart, gotEnd := reverseRange(screenLines[screenRow])

		switch {
		case i < sy || i > ey:
			if gotStart != -1 {
				t.Errorf("row %d: expected no highlight, got cols [%d,%d)", i, gotStart, gotEnd)
			}
		case i == sy:
			wantStart := r.x + sx
			if gotStart != wantStart {
				t.Errorf("start row %d: highlight begins at col %d, want %d", i, gotStart, wantStart)
			}
		case i == ey:
			wantStart := r.x
			wantEnd := r.x + ex + 1
			if gotStart != wantStart || gotEnd != wantEnd {
				t.Errorf("end row %d: highlight [%d,%d), want [%d,%d)", i, gotStart, gotEnd, wantStart, wantEnd)
			}
		default: // middle row
			if gotStart != r.x {
				t.Errorf("middle row %d: highlight begins at col %d, want %d", i, gotStart, r.x)
			}
		}
	}
}

// addTabs appends n filler tabs so the header tab bar wraps to multiple rows.
func addTabs(m *Model, n int, title string) {
	for i := 0; i < n; i++ {
		m.snapshot.Tabs = append(m.snapshot.Tabs, domain.TabSnapshot{ID: "filler", Title: title})
	}
	m.recomputePaneRects()
}

// TestPaneRectAlignsWithWrappedHeader is the regression test for the core bug:
// when the tab bar wraps to more than one text row, the pane rectangles (used
// for mouse hit-testing) must start where the panes are actually rendered, not
// at a hardcoded header height.
func TestPaneRectAlignsWithWrappedHeader(t *testing.T) {
	for _, nTabs := range []int{0, 5, 20, 40} {
		m := buildTestModelMulti(60, 30, []string{"line0aaaa\nline1bbbb\nline2cccc"})
		addTabs(&m, nTabs, "tab-extra-name")

		if len(m.paneRects) == 0 {
			t.Fatalf("nTabs=%d: no pane rects", nTabs)
		}
		r := m.paneRects[0]

		// Find where the pane's meta line actually renders on screen.
		lines := strings.Split(m.View(), "\n")
		metaRow := -1
		want := paneMetaText(*m.findPaneInSnapshot(r.paneID))
		for i, l := range lines {
			if strings.Contains(ansi.Strip(l), want) {
				metaRow = i
				break
			}
		}
		if metaRow == -1 {
			t.Fatalf("nTabs=%d: meta line %q not found on screen", nTabs, want)
		}
		if metaRow != r.y {
			t.Errorf("nTabs=%d: pane meta renders at screen row %d but mouse mapping uses r.y=%d (off by %d)",
				nTabs, metaRow, r.y, metaRow-r.y)
		}
	}
}

// TestSelectionHighlightWithWrappedHeader verifies the highlight still lands on
// the mouse-selected rows when the header wraps.
func TestSelectionHighlightWithWrappedHeader(t *testing.T) {
	output := "line0aaaa\nline1bbbb\nline2cccc\nline3dddd\nline4eeee\nline5ffff"
	m := buildTestModelMulti(120, 30, []string{output})
	addTabs(&m, 60, "a-fairly-long-tab-name") // force header to wrap

	r := m.paneRects[0]
	sx, sy := 1, 2 // output line "line0aaaa"
	ex, ey := 4, 4 // output line "line2cccc"
	m.selection = mouseSelection{active: true, dragging: true, dragged: true, paneID: r.paneID, startX: sx, startY: sy, endX: ex, endY: ey}

	lines := strings.Split(m.View(), "\n")
	for i := sy; i <= ey; i++ {
		screenRow := r.y + i
		if screenRow >= len(lines) {
			t.Fatalf("row %d off screen", i)
		}
		gotStart, gotEnd := reverseRange(lines[screenRow])
		switch i {
		case sy:
			if gotStart != r.x+sx {
				t.Errorf("start row %d: highlight at col %d, want %d", i, gotStart, r.x+sx)
			}
		case ey:
			if gotStart != r.x || gotEnd != r.x+ex+1 {
				t.Errorf("end row %d: highlight [%d,%d), want [%d,%d)", i, gotStart, gotEnd, r.x, r.x+ex+1)
			}
		default:
			if gotStart != r.x {
				t.Errorf("mid row %d: highlight at col %d, want %d", i, gotStart, r.x)
			}
		}
	}
}

// TestSelectionLineMappingMatchesScreen verifies that, for every content row in
// a pane, the line that the selection coordinate-mapping uses (buildDisplayLines)
// equals what is actually rendered at that screen row+column. A mismatch means a
// drag at a given screen position highlights the wrong text.
func TestSelectionLineMappingMatchesScreen(t *testing.T) {
	cases := map[string]string{
		"short_lines": "alpha\nbravo\ncharlie\ndelta\necho\nfoxtrot",
		"long_lines":  "this is a very long line that will certainly exceed the pane width and need to wrap onto multiple visual rows\nshort\nanother extremely long line of words that keeps going and going past the available column budget for sure",
	}

	for name, output := range cases {
		t.Run(name, func(t *testing.T) {
			m := buildTestModel(100, 30, output)

			// Render the full screen with no selection active.
			view := m.View()
			screenLines := strings.Split(view, "\n")
			plain := make([]string, len(screenLines))
			for i, l := range screenLines {
				plain[i] = ansi.Strip(l)
			}

			if len(m.paneRects) != 1 {
				t.Fatalf("expected 1 pane rect, got %d", len(m.paneRects))
			}
			r := m.paneRects[0]

			pane := m.findPaneInSnapshot(r.paneID)
			if pane == nil {
				t.Fatalf("pane %s not found in snapshot", r.paneID)
			}
			tab := m.activeTab()
			textWidth := max(1, r.w)
			displayLines := buildDisplayLines(*pane, *tab, textWidth, r.h, "", 0, "")

			for i := 0; i < r.h; i++ {
				screenRow := r.y + i
				if screenRow >= len(plain) {
					t.Fatalf("row %d: screen has only %d rows (pane rect y=%d h=%d)", i, len(plain), r.y, r.h)
				}
				gotOnScreen := screenColSubstr(plain[screenRow], r.x, r.w)
				if i >= len(displayLines) {
					t.Fatalf("row %d: buildDisplayLines has only %d lines", i, len(displayLines))
				}
				wantFromMapping := displayLines[i]
				// Normalize trailing spaces for comparison (padding differences).
				if strings.TrimRight(gotOnScreen, " ") != strings.TrimRight(wantFromMapping, " ") {
					t.Errorf("row %d (screen y=%d, x=%d, w=%d):\n  on-screen : %q\n  mapping   : %q",
						i, screenRow, r.x, r.w, gotOnScreen, wantFromMapping)
				}
			}
		})
	}
}
