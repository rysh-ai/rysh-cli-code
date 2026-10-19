// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// E17 (f): the REPLAY badge must stay on ONE line in narrow panes. It used to
// wrap onto 2–3 lines at 45 and 30 columns (measured 2026-10-03), pushing the
// replay output down. The badge degrades instead: the position bar goes
// first, then the pane's meta suffix ("| flex:1"), then the wall clock;
// position/total, speed and the pause marker always survive.

const wideBadge = "REPLAY ▕███░░░░░░░░░▏ 01:23/04:56 · 14:32:07 ×0.5 ⏸"

// renderReplayPanel frames a replay pane exactly as the lane layout does
// (paneStyle.Width(w) around buildPanePanel) and returns the screen lines.
func renderReplayPanel(t *testing.T, w int) []string {
	t.Helper()
	m := buildBoardModel(nil)
	pane := m.snapshot.Tabs[0].Lanes[0].PaneGroups[0].Panes[0]
	pane.PaneType = "replay"
	pane.Status = wideBadge
	pane.Output = "hello\n"
	panel := m.buildPanePanel(pane, m.snapshot.Tabs[0], w, 8)
	return strings.Split(ansi.Strip(paneStyle.Width(w).Height(8).Render(panel)), "\n")
}

func TestReplayBadgeStaysOnOneLine(t *testing.T) {
	for _, tc := range []struct {
		w           int
		mustHave    []string
		mustNotHave []string
	}{
		{100, []string{"▕███░░░░░░░░░▏", "01:23/04:56", "14:32:07", "×0.5", "⏸", "flex:1"}, nil},
		{45, []string{"01:23/04:56", "14:32:07", "×0.5", "⏸"}, []string{"▕"}},
		{30, []string{"01:23/04:56", "×0.5", "⏸"}, []string{"▕", "14:32:07"}},
	} {
		lines := renderReplayPanel(t, tc.w)
		// lines[0] is the top border, lines[1] the meta line; the meta is
		// followed by a blank spacer, so a wrapped badge shows up in lines[2].
		meta, next := lines[1], strings.Trim(lines[2], "│ ")
		if next != "" {
			t.Errorf("width %d: badge wrapped onto a second line:\n%s\n%s", tc.w, meta, lines[2])
		}
		for _, s := range tc.mustHave {
			if !strings.Contains(meta, s) {
				t.Errorf("width %d: meta line %q lost %q", tc.w, meta, s)
			}
		}
		for _, s := range tc.mustNotHave {
			if strings.Contains(meta, s) {
				t.Errorf("width %d: meta line %q still carries %q, which does not fit", tc.w, meta, s)
			}
		}
	}
}

// TestNonReplayMetaUnchanged: panes that are not replay panes keep their meta
// text exactly as before, even when it is wider than the pane.
func TestNonReplayMetaUnchanged(t *testing.T) {
	m := buildBoardModel(nil)
	pane := m.snapshot.Tabs[0].Lanes[0].PaneGroups[0].Panes[0]
	pane.PaneType = ""
	pane.Status = wideBadge // a normal pane is never reshaped, whatever its status says
	pane.Output = "hello\n"
	panel := m.buildPanePanel(pane, m.snapshot.Tabs[0], 30, 8)
	if first := ansi.Strip(strings.Split(panel, "\n")[0]); !strings.Contains(first, "▕") {
		t.Fatalf("a normal pane's meta was reshaped: %q", first)
	}
}
