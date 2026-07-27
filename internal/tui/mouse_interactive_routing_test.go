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

// TestDragCopyOnActivePane drives the full press->motion->release flow on the
// ACTIVE pane in normal mode (so no focus NATS message is sent) and checks that
// a copy was attempted (flashMsg set) with the expected selected text. This is
// the regression guard for normal-mode mouse copy.
func TestDragCopyOnActivePane(t *testing.T) {
	output := "line0aaaa\nline1bbbb\nline2cccc\nline3dddd"
	m := buildTestModel(120, 30, output)
	r := m.paneRects[0]

	press := tea.MouseMsg{X: r.x + 0, Y: r.y + 0, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress}
	mm, _ := m.handleMouse(press)
	m = mm.(Model)

	motion := tea.MouseMsg{X: r.x + 4, Y: r.y + 2, Button: tea.MouseButtonLeft, Action: tea.MouseActionMotion}
	mm, _ = m.handleMouse(motion)
	m = mm.(Model)

	release := tea.MouseMsg{X: r.x + 4, Y: r.y + 2, Button: tea.MouseButtonLeft, Action: tea.MouseActionRelease}
	mm, _ = m.handleMouse(release)
	m = mm.(Model)

	if !m.selection.active {
		t.Errorf("selection not active after drag")
	}
	if m.flashMsg == "" {
		t.Errorf("flashMsg empty: copy was not attempted (extract returned empty?)")
	}
	got := m.extractSelectedText()
	if !strings.Contains(got, "line0") {
		t.Errorf("expected selected text to include the first content row, got %q", got)
	}
}

// buildInteractiveMultiModel builds a TWO-lane model (side-by-side panes). The
// active pane (pane-a) is an interactive program with mouse tracking on
// (RawMode + MouseEnabled), and the TUI is in modeRaw. pane-b is a normal pane.
func buildInteractiveMultiModel(b *bus.Bus) Model {
	var pub *msgpkg.NATSPublisher
	if b != nil {
		pub = b.Publisher()
	}
	pa := domain.PaneSnapshot{ID: "pane-a", Title: "claude", Status: "running", ProviderName: "claude", Flex: 1, RawMode: true, MouseEnabled: true}
	pb := domain.PaneSnapshot{ID: "pane-b", Title: "shell", Status: "ready", ProviderName: "shell", Flex: 1, Output: "hello world from pane b"}
	tab := domain.TabSnapshot{
		ID:           "tab-1",
		Title:        "t1",
		ActivePaneID: "pane-a",
		Lanes: []domain.LaneSnapshot{
			{ID: "lane-a", Flex: 1, ActivePaneID: "pane-a", PaneGroups: []domain.PaneGroupSnapshot{{ID: "g-a", RowFlex: 1, ActivePaneID: "pane-a", Panes: []domain.PaneSnapshot{pa}}}},
			{ID: "lane-b", Flex: 1, ActivePaneID: "pane-b", PaneGroups: []domain.PaneGroupSnapshot{{ID: "g-b", RowFlex: 1, ActivePaneID: "pane-b", Panes: []domain.PaneSnapshot{pb}}}},
		},
	}
	m := Model{
		snapshot:          domain.WorkspaceSnapshot{Tabs: []domain.TabSnapshot{tab}, ActiveTabID: "tab-1", ActivePaneID: "pane-a"},
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

// TestInteractivePaneClickFocusesOtherPane is the regression test for the fix:
// when the active pane is an interactive program with mouse tracking on, a
// left-click on a DIFFERENT pane must be handled by the multiplexer (focus /
// selection), NOT swallowed by the active pane's PTY. Before the fix, the
// MouseMsg routing forwarded every event to forwardRawMouse, which silently
// dropped clicks outside the active pane.
func TestInteractivePaneClickFocusesOtherPane(t *testing.T) {
	b := startTestBus(t)
	m := buildInteractiveMultiModel(b)

	// Locate pane-b's rect (the non-active pane).
	var rb *paneRect
	for i := range m.paneRects {
		if m.paneRects[i].paneID == "pane-b" {
			rb = &m.paneRects[i]
		}
	}
	if rb == nil {
		t.Fatalf("pane-b rect not found; rects=%+v", m.paneRects)
	}

	// Left-press inside pane-b while pane-a (interactive) is active and modeRaw.
	press := tea.MouseMsg{X: rb.x + 1, Y: rb.y + 1, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress}
	updated, _ := m.Update(press)
	got := updated.(Model)

	// handleMouse ran (not forwardRawMouse): it records a selection anchored on
	// pane-b and starts dragging. forwardRawMouse never touches m.selection.
	if got.selection.paneID != "pane-b" {
		t.Fatalf("click on non-active pane was not routed to the multiplexer: selection.paneID=%q (want pane-b)", got.selection.paneID)
	}
	if !got.selection.dragging {
		t.Errorf("expected selection.dragging=true after press on pane-b")
	}
}

// TestInteractivePaneClickOnSelfForwardsToPTY guards the other half: a click
// INSIDE the active interactive pane must still be forwarded to its child
// process (i.e. NOT treated as a multiplexer selection).
func TestInteractivePaneClickOnSelfForwardsToPTY(t *testing.T) {
	b := startTestBus(t)
	m := buildInteractiveMultiModel(b)

	var ra *paneRect
	for i := range m.paneRects {
		if m.paneRects[i].paneID == "pane-a" {
			ra = &m.paneRects[i]
		}
	}
	if ra == nil {
		t.Fatalf("pane-a rect not found")
	}

	press := tea.MouseMsg{X: ra.x + 1, Y: ra.y + 1, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress}
	updated, _ := m.Update(press)
	got := updated.(Model)

	// forwardRawMouse does not set a multiplexer selection.
	if got.selection.paneID != "" || got.selection.dragging {
		t.Errorf("click inside active interactive pane should forward to PTY, but a multiplexer selection was started: %+v", got.selection)
	}
}
