// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/rysh-ai/rysh-cli-code/internal/bus"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// TestPaneNeedsScrollFallback is a pure unit test of the gating rule: a
// remote/mirror share always falls back to rysh's own scrollback (no local
// PTY to forward to); a local interactive pane falls back only when its
// foreground program never claimed its own mouse tracking (the codex case);
// a program that DOES claim mouse tracking (the claude case) is left alone,
// and a non-interactive pane never triggers the fallback either.
func TestPaneNeedsScrollFallback(t *testing.T) {
	cases := []struct {
		name string
		pane *domain.PaneSnapshot
		want bool
	}{
		{"nil pane", nil, false},
		{"remote interactive", &domain.PaneSnapshot{RemoteInteractive: true}, true},
		{"local raw, no mouse tracking (codex)", &domain.PaneSnapshot{RawMode: true, MouseEnabled: false}, true},
		{"local raw, mouse tracking on (claude)", &domain.PaneSnapshot{RawMode: true, MouseEnabled: true}, false},
		{"not raw at all (plain shell)", &domain.PaneSnapshot{RawMode: false, MouseEnabled: false}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := paneNeedsScrollFallback(tc.pane); got != tc.want {
				t.Errorf("paneNeedsScrollFallback(%+v) = %v, want %v", tc.pane, got, tc.want)
			}
		})
	}
}

// buildSingleInteractiveModel builds a one-pane model with the active pane an
// interactive program (RawMode=true) and the TUI already in modeRaw — mirrors
// buildInteractiveMultiModel in mouse_interactive_routing_test.go but as a
// single, non-stacked pane, and lets the caller choose MouseEnabled so the
// same helper covers both the codex-like and claude-like cases.
func buildSingleInteractiveModel(b *bus.Bus, mouseEnabled bool) Model {
	var pub *msgpkg.NATSPublisher
	if b != nil {
		pub = b.Publisher()
	}
	pane := domain.PaneSnapshot{
		ID: "pane-1", Title: "agent", Status: "running", ProviderName: "agent",
		Flex: 1, RawMode: true, MouseEnabled: mouseEnabled,
	}
	tab := domain.TabSnapshot{
		ID:           "tab-1",
		Title:        "t1",
		ActivePaneID: "pane-1",
		Lanes: []domain.LaneSnapshot{
			{ID: "lane-1", Flex: 1, ActivePaneID: "pane-1", PaneGroups: []domain.PaneGroupSnapshot{
				{ID: "g-1", RowFlex: 1, ActivePaneID: "pane-1", Panes: []domain.PaneSnapshot{pane}},
			}},
		},
	}
	m := Model{
		snapshot:          domain.WorkspaceSnapshot{Tabs: []domain.TabSnapshot{tab}, ActiveTabID: "tab-1", ActivePaneID: "pane-1"},
		mode:              modeRaw,
		width:             120,
		height:            40,
		inputs:            map[string]textinput.Model{},
		paneInputModes:    map[string]string{},
		panePastedText:    map[string]string{},
		paneHistoryIdx:    map[string]int{},
		paneHistorySaved:  map[string]string{},
		paneScrollOffsets: map[string]int{},
		pipelineOutputs:   map[string]string{},
		attentionState:    map[string]*attentionInfo{},
		pub:               pub,
		bus:               b,
		workspaceInbox:    msgpkg.T("ws", "inbox"),
	}
	m.recomputePaneRects()
	return m
}

// TestWheelUpEntersScrollModeWhenChildHasNoMouseTracking is the regression
// test for the codex half of the bug: a foreground program that never
// enabled its own mouse tracking has no way to react to a wheel event
// forwarded into its PTY, so rysh must fall back to its own scrollback
// (copy) mode instead of leaving the gesture dead.
func TestWheelUpEntersScrollModeWhenChildHasNoMouseTracking(t *testing.T) {
	b := startTestBus(t)
	m := buildSingleInteractiveModel(b, false)
	r := m.paneRects[0]

	wheel := tea.MouseMsg{X: r.x + 1, Y: r.y + 1, Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress}
	updated, _ := m.Update(wheel)
	got := updated.(Model)

	if got.mode != modeRawScroll {
		t.Fatalf("expected wheel-up over a non-mouse-tracking interactive pane to enter modeRawScroll, got mode=%v", got.mode)
	}
	if got.rawScrollPaneID != "pane-1" {
		t.Errorf("expected rawScrollPaneID=pane-1, got %q", got.rawScrollPaneID)
	}
}

// TestWheelUpLeavesMouseTrackingChildAlone guards the other half: a program
// that DOES claim mouse tracking (claude) must keep receiving the wheel
// event directly — this fallback must never intercept it.
func TestWheelUpLeavesMouseTrackingChildAlone(t *testing.T) {
	b := startTestBus(t)
	m := buildSingleInteractiveModel(b, true)
	r := m.paneRects[0]

	wheel := tea.MouseMsg{X: r.x + 1, Y: r.y + 1, Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress}
	updated, _ := m.Update(wheel)
	got := updated.(Model)

	if got.mode == modeRawScroll {
		t.Fatalf("wheel-up over a mouse-tracking interactive pane must not enter modeRawScroll (it should be forwarded to the PTY instead)")
	}
}

// TestPgUpEntersScrollModeWhenChildHasNoMouseTracking mirrors the wheel test
// for the keyboard path (pgup/shift+up), which is gated by the same
// paneNeedsScrollFallback rule.
func TestPgUpEntersScrollModeWhenChildHasNoMouseTracking(t *testing.T) {
	b := startTestBus(t)
	m := buildSingleInteractiveModel(b, false)

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	got := updated.(Model)

	if got.mode != modeRawScroll {
		t.Fatalf("expected pgup over a non-mouse-tracking interactive pane to enter modeRawScroll, got mode=%v", got.mode)
	}
}

// TestPgUpLeavesMouseTrackingChildAlone guards that pgup still reaches
// claude's own PTY unchanged (forwardRawKey), matching the wheel guard above.
func TestPgUpLeavesMouseTrackingChildAlone(t *testing.T) {
	b := startTestBus(t)
	m := buildSingleInteractiveModel(b, true)

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	got := updated.(Model)

	if got.mode == modeRawScroll {
		t.Fatalf("pgup over a mouse-tracking interactive pane must not enter modeRawScroll (it should be forwarded to the PTY instead)")
	}
}

// TestRawScrollDragCopy drives the full press->motion->release flow while
// modeRawScroll is showing a frozen interactive-pane history, and checks
// that a copy was attempted (flashMsg set) with the expected selected text.
// This is the regression guard for the other half of the bug: before this
// fix, handleRawScrollMouse handled only the wheel and silently dropped
// every press/motion/release event, so copying was impossible for any
// interactive pane, not just codex's.
func TestRawScrollDragCopy(t *testing.T) {
	b := startTestBus(t)
	m := buildSingleInteractiveModel(b, false)
	r := m.paneRects[0]

	// Enter scrollback (copy) mode directly with known frozen rows, bypassing
	// the async NATS fetch enterRawScroll would normally kick off.
	m.mode = modeRawScroll
	m.rawScrollPaneID = "pane-1"
	m.rawScrollOffset = 0
	m.rawScrollRows = []string{
		"row zero aaaa",
		"row one bbbb",
		"row two cccc",
	}

	press := tea.MouseMsg{X: r.x, Y: r.y + 1, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress}
	updated, _ := m.Update(press)
	m = updated.(Model)

	motion := tea.MouseMsg{X: r.x + 8, Y: r.y + 1, Button: tea.MouseButtonLeft, Action: tea.MouseActionMotion}
	updated, _ = m.Update(motion)
	m = updated.(Model)

	release := tea.MouseMsg{X: r.x + 8, Y: r.y + 1, Button: tea.MouseButtonLeft, Action: tea.MouseActionRelease}
	updated, _ = m.Update(release)
	m = updated.(Model)

	if !m.selection.active {
		t.Fatalf("selection not active after drag in modeRawScroll")
	}
	if m.selection.paneID != "pane-1" {
		t.Errorf("expected selection.paneID=pane-1, got %q", m.selection.paneID)
	}
	if m.flashMsg == "" {
		t.Fatalf("flashMsg empty: copy was not attempted (extractSelectedText returned empty?)")
	}
	got := m.extractSelectedText()
	if !strings.Contains(got, "row zero") {
		t.Errorf("expected selected text to include the frozen history content, got %q", got)
	}
}

// TestRawScrollWheelStillScrollsAfterFix guards that the plain wheel-only
// path (no drag) still just scrolls the frozen view and does not start a
// selection — the pre-existing behaviour this change must not disturb.
func TestRawScrollWheelStillScrollsAfterFix(t *testing.T) {
	m := buildTestModel(120, 30, "seed")
	m.mode = modeRawScroll
	m.rawScrollPaneID = "pane-1"
	m.rawScrollRows = make([]string, 50)
	for i := range m.rawScrollRows {
		m.rawScrollRows[i] = "line"
	}
	m.rawScrollOffset = 0

	wheel := tea.MouseMsg{Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress}
	mm, _ := m.handleRawScrollMouse(wheel)
	m2 := mm.(Model)

	if m2.rawScrollOffset <= m.rawScrollOffset {
		t.Fatalf("expected wheel-up to increase rawScrollOffset, got %d (was %d)", m2.rawScrollOffset, m.rawScrollOffset)
	}
	if m2.selection.active {
		t.Errorf("wheel scroll must not start a selection")
	}
}
