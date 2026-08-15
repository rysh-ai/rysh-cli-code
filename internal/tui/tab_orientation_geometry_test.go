// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

// vertical returns a copy of m with the tab bar switched to the left column.
func vertical(m Model) Model {
	m.snapshot.TabBarVertical = true
	m.recomputePaneRects()
	return m
}

// TestTabBarHorizontalGeometryUnchanged pins that the default orientation still
// computes exactly the budgets it did before the vertical tab bar existed. The
// vertical column is opt-in; nothing about the horizontal layout may move.
func TestTabBarHorizontalGeometryUnchanged(t *testing.T) {
	m := buildTestModel(120, 40, "hello")

	if got := m.tabColumnWidth(); got != 0 {
		t.Errorf("horizontal tab bar reserves %d columns; want 0", got)
	}
	if got, want := m.bodyXOffset(), 1; got != want {
		t.Errorf("bodyXOffset = %d; want %d (body Padding(0,1) only)", got, want)
	}
	if got, want := m.bodyHeight(), max(8, 40-8); got != want {
		t.Errorf("bodyHeight = %d; want %d", got, want)
	}
	if got, want := m.bodyPanelHeight(), max(10, 40-6); got != want {
		t.Errorf("bodyPanelHeight = %d; want %d", got, want)
	}
	for _, n := range []int{1, 2, 3, 5} {
		if got, want := m.paneAvailWidth(n), max(24, 120-2-2*n); got != want {
			t.Errorf("paneAvailWidth(%d) = %d; want %d", n, got, want)
		}
	}
	if got, want := m.fullscreenWidth(), max(20, 120-4); got != want {
		t.Errorf("fullscreenWidth = %d; want %d", got, want)
	}
	if got := lipgloss.Height(m.renderHeader()); got != 2 {
		t.Errorf("horizontal header is %d rows; want 2 (workspaces + tabs)", got)
	}
}

// TestTabBarVerticalTakesWidthAndGivesBackARow pins the trade the vertical bar
// makes: it costs the panes its column width and refunds them the header row it
// no longer occupies. Both halves have to hold, or the body drifts.
func TestTabBarVerticalTakesWidthAndGivesBackARow(t *testing.T) {
	h := buildTestModel(120, 40, "hello")
	v := vertical(h)

	col := v.tabColumnWidth()
	if col <= 0 {
		t.Fatalf("vertical tab bar reserves %d columns; want > 0", col)
	}
	if got := lipgloss.Height(v.renderHeader()); got != 1 {
		t.Errorf("vertical header is %d rows; want 1 (workspaces only)", got)
	}
	if got, want := v.bodyXOffset(), h.bodyXOffset()+col; got != want {
		t.Errorf("bodyXOffset = %d; want %d (shifted right by the column)", got, want)
	}
	if got, want := v.bodyHeight(), h.bodyHeight()+1; got != want {
		t.Errorf("bodyHeight = %d; want %d (the freed header row)", got, want)
	}
	for _, n := range []int{1, 2, 3} {
		if got, want := v.paneAvailWidth(n), h.paneAvailWidth(n)-col; got != want {
			t.Errorf("paneAvailWidth(%d) = %d; want %d (narrower by the column)", n, got, want)
		}
	}
	if got, want := v.fullscreenWidth(), h.fullscreenWidth()-col; got != want {
		t.Errorf("fullscreenWidth = %d; want %d", got, want)
	}
}

// TestTabColumnWidthIgnoresTheAttentionBlink is the reason tabColumnLabel exists
// separately from the rendered row. The attention marker blinks twice a second;
// if it fed the column width, every pane and every mouse rect would resize with
// it. The row absorbs the marker within a fixed width instead.
func TestTabColumnWidthIgnoresTheAttentionBlink(t *testing.T) {
	m := vertical(buildTestModel(120, 40, "hello"))
	m.attentionState["pane-1"] = &attentionInfo{Count: 7}

	m.attentionBlink = false
	dark := m.tabColumnWidth()
	darkRows := m.renderTabColumn(dark, 0)

	m.attentionBlink = true
	lit := m.tabColumnWidth()
	litRows := m.renderTabColumn(lit, 0)

	if dark != lit {
		t.Errorf("tab column width blinks: %d unlit vs %d lit", dark, lit)
	}
	for label, block := range map[string]string{"unlit": darkRows, "lit": litRows} {
		for i, line := range strings.Split(block, "\n") {
			if got := lipgloss.Width(line); got != dark {
				t.Errorf("%s column row %d is %d cells wide; want exactly %d", label, i, got, dark)
			}
		}
	}
	if !strings.Contains(ansi.Strip(litRows), "●7") {
		t.Errorf("lit column dropped the attention marker:\n%s", ansi.Strip(litRows))
	}
}

// TestTabColumnWidthIsBounded pins that a long tab title cannot squeeze the
// panes out, and that a narrow terminal still leaves a usable sliver.
func TestTabColumnWidthIsBounded(t *testing.T) {
	m := vertical(buildTestModel(120, 40, "hello"))
	m.snapshot.Tabs[0].Title = strings.Repeat("long-tab-title", 8)
	if got := m.tabColumnWidth(); got > tabColMaxWidth {
		t.Errorf("a %d-char title widened the column to %d; want <= %d",
			len(m.snapshot.Tabs[0].Title), got, tabColMaxWidth)
	}

	narrow := vertical(buildTestModel(30, 40, "hello"))
	got := narrow.tabColumnWidth()
	if got > 30/3 {
		t.Errorf("column is %d of a 30-col terminal; want <= a third", got)
	}
	if got <= 0 {
		t.Errorf("column collapsed to %d on a narrow terminal; want a usable sliver", got)
	}
}

// TestVerticalTabBarFitsTheTerminal is the end-to-end guard: whatever the
// column costs, the tab bar plus the pane grid beside it may not exceed the
// terminal width. An over-wide row wraps, which pushes every pane down a line
// and is exactly the drift the shared geometry helpers exist to prevent.
//
// Header and body only — renderFooter is unbounded in this harness (it renders
// a full status line against a zero-value config, ~215 cells regardless of
// m.width) and is the same in both orientations, so including it would measure
// the harness rather than the tab bar.
func TestVerticalTabBarFitsTheTerminal(t *testing.T) {
	for _, dims := range []struct{ w, h int }{{120, 40}, {80, 24}, {60, 20}} {
		m := vertical(buildTestModelMulti(dims.w, dims.h, []string{"one", "two"}))
		block := m.renderHeader() + "\n" + m.renderBody()
		for i, line := range strings.Split(block, "\n") {
			if got := lipgloss.Width(line); got > dims.w {
				t.Errorf("%dx%d: row %d is %d cells wide; want <= %d",
					dims.w, dims.h, i, got, dims.w)
			}
		}
	}
}

// TestVerticalTabBarBodyIsNoWiderThanHorizontal pins that the column is paid
// for out of the pane budget rather than added to the total: the body must
// occupy the same screen width in both orientations.
func TestVerticalTabBarBodyIsNoWiderThanHorizontal(t *testing.T) {
	for _, dims := range []struct{ w, h int }{{120, 40}, {80, 24}} {
		h := buildTestModelMulti(dims.w, dims.h, []string{"one", "two"})
		v := vertical(h)
		if got, want := lipgloss.Width(v.renderBody()), lipgloss.Width(h.renderBody()); got != want {
			t.Errorf("%dx%d: vertical body is %d cells wide; horizontal is %d",
				dims.w, dims.h, got, want)
		}
	}
}

// TestTabColumnNeverOutgrowsTheBody pins the overflow behaviour. A horizontal
// strip with too many tabs wraps onto another header row; a column that did the
// equivalent would make the View taller than the terminal and push the footer
// off screen. Past the body height the column scrolls instead, and the active
// tab stays visible wherever it is in the list.
func TestTabColumnNeverOutgrowsTheBody(t *testing.T) {
	for _, activeIdx := range []int{0, 12, 29} {
		m := vertical(buildTestModel(120, 24, "hello"))
		m.snapshot.Tabs = nil
		for i := 0; i < 30; i++ {
			m.snapshot.Tabs = append(m.snapshot.Tabs,
				domain.TabSnapshot{ID: fmt.Sprintf("tab-%d", i), Title: fmt.Sprintf("t%d", i)})
		}
		m.snapshot.ActiveTabID = fmt.Sprintf("tab-%d", activeIdx)

		// Measure against the terminal, not against renderBody() — the body
		// already contains the column, so comparing the two would be circular
		// and would pass however tall the column grew.
		chrome := lipgloss.Height(m.renderHeader()) + lipgloss.Height(m.renderBody())
		if chrome > m.height {
			t.Errorf("active=%d: header+body is %d rows on a %d-row terminal",
				activeIdx, chrome, m.height)
		}

		col := m.renderTabColumn(m.tabColumnWidth(), m.bodyHeight())
		if got, want := lipgloss.Height(col), m.bodyHeight(); got > want {
			t.Errorf("active=%d: column is %d rows against a %d-row body budget",
				activeIdx, got, want)
		}

		plain := ansi.Strip(col)
		if !strings.Contains(plain, fmt.Sprintf("%d t%d", activeIdx+1, activeIdx)) {
			t.Errorf("active=%d: the active tab scrolled out of the column:\n%s", activeIdx, plain)
		}
		if !strings.Contains(plain, "more") {
			t.Errorf("active=%d: 30 tabs in a short column reported no remainder:\n%s", activeIdx, plain)
		}
	}
}

// TestVerticalTabBarRendersTabsAndPaneRectsClearIt pins that the column shows
// the tabs and that mouse hit-testing starts to the right of it — a rect that
// overlapped the column would map clicks on the tab bar into a pane.
func TestVerticalTabBarRendersTabsAndPaneRectsClearIt(t *testing.T) {
	m := vertical(buildTestModel(120, 40, "hello"))
	m.snapshot.Tabs[0].Title = "builds"

	if !strings.Contains(ansi.Strip(m.renderTabColumn(m.tabColumnWidth(), 0)), "1 builds") {
		t.Errorf("column does not show the numbered tab title:\n%s",
			ansi.Strip(m.renderTabColumn(m.tabColumnWidth(), 0)))
	}

	m.recomputePaneRects()
	if len(m.paneRects) == 0 {
		t.Fatal("no pane rects computed")
	}
	for _, r := range m.paneRects {
		if r.x < m.tabColumnWidth() {
			t.Errorf("pane %s starts at x=%d, inside the %d-wide tab column",
				r.paneID, r.x, m.tabColumnWidth())
		}
	}
}
