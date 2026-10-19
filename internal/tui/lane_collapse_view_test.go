// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func lanesFlex(flex []int, collapsed ...int) []domain.LaneRenderInfo {
	out := make([]domain.LaneRenderInfo, len(flex))
	for i, f := range flex {
		out[i] = domain.LaneRenderInfo{LaneID: string(rune('a' + i)), Flex: f}
	}
	for _, c := range collapsed {
		out[c].Collapsed = true
	}
	return out
}

func sum(xs []int) int {
	n := 0
	for _, x := range xs {
		n += x
	}
	return n
}

// The founder's example: three lanes at a third each; fold lane 2 and lanes 1
// and 3 take half each, lane 2 stays on screen as a strip.
func TestLaneWidths_FoldedLaneGivesItsWidthToTheOpenOnes(t *testing.T) {
	const total = 200
	w := laneWidths(lanesFlex([]int{10, 10, 10}, 1), total)
	if w[1] != collapsedLaneWidth {
		t.Fatalf("folded lane width = %d; want %d", w[1], collapsedLaneWidth)
	}
	if sum(w) != total {
		t.Errorf("widths %v sum to %d; want the whole budget %d", w, sum(w), total)
	}
	if d := w[0] - w[2]; d < -1 || d > 1 {
		t.Errorf("open lanes %d and %d should split the rest evenly", w[0], w[2])
	}
	if w[0] < (total-collapsedLaneWidth)/2-1 {
		t.Errorf("open lane %d is not about half of %d", w[0], total)
	}

	// Flex still decides among the open lanes.
	w = laneWidths(lanesFlex([]int{20, 10, 10}, 2), total)
	if w[2] != collapsedLaneWidth || w[0] <= w[1] || sum(w) != total {
		t.Errorf("flex 20/10 with the last folded: %v", w)
	}

	// The first lane folded: the last open lane still absorbs the remainder.
	w = laneWidths(lanesFlex([]int{10, 10, 10}, 0), total)
	if w[0] != collapsedLaneWidth || sum(w) != total {
		t.Errorf("first lane folded: %v", w)
	}
}

func TestLaneWidths_AllFoldedFallsBackToOpen(t *testing.T) {
	w := laneWidths(lanesFlex([]int{10, 10}, 0, 1), 100)
	if w[0] < 12 || w[1] < 12 {
		t.Errorf("every lane folded must lay them out open, got %v", w)
	}
}

func TestVerticalLabel(t *testing.T) {
	if got := verticalLabel("2 api", 10); strings.Join(got, "") != "2 api" || len(got) != 5 {
		t.Errorf("verticalLabel = %q", got)
	}
	got := verticalLabel("2 a-long-name", 5)
	if len(got) != 5 || got[4] != "…" {
		t.Errorf("truncation = %q; want 5 cells ending in …", got)
	}
	if got := verticalLabel("2 日本x\x1b", 10); strings.Join(got, "") != "2 x" {
		t.Errorf("wide and control runes must be dropped, got %q", got)
	}
}

// foldedModel is three side-by-side lanes with the middle one folded.
func foldedModel() Model {
	m := buildTestModelMulti(120, 40, []string{"left", "middle", "right"})
	tab := &m.snapshot.Tabs[0]
	tab.Lanes[1].Collapsed = true
	tab.Lanes[1].Name = "api"
	m.recomputePaneRects()
	return m
}

func TestCollapsedLane_RendersAsAStripWithVerticalName(t *testing.T) {
	open := buildTestModelMulti(120, 40, []string{"left", "middle", "right"})
	m := foldedModel()

	body := ansi.Strip(m.View())
	for _, cell := range []string{"│2│", "│a│", "│p│", "│i│", "╭─╮", "╰─╯"} {
		if !strings.Contains(body, cell) {
			t.Errorf("strip cell %q missing from the view:\n%s", cell, body)
		}
	}
	// Folding must not change the screen's outer size: same height, and no
	// line wider than the unfolded layout's.
	if lipgloss.Height(m.View()) != lipgloss.Height(open.View()) {
		t.Errorf("view height %d; unfolded %d", lipgloss.Height(m.View()), lipgloss.Height(open.View()))
	}
	if lipgloss.Width(m.View()) != lipgloss.Width(open.View()) {
		t.Errorf("view width %d; unfolded %d", lipgloss.Width(m.View()), lipgloss.Width(open.View()))
	}
}

func TestCollapsedLane_NoPaneRectOnlyAStrip(t *testing.T) {
	m := foldedModel()
	for _, r := range m.paneRects {
		if r.paneID == "pane-b" {
			t.Errorf("a folded lane's pane must have no rect (nothing is drawn): %+v", r)
		}
	}
	if len(m.laneStrips) != 1 || m.laneStrips[0].laneID != "lane-b" || m.laneStrips[0].w != collapsedLaneWidth+2 {
		t.Fatalf("laneStrips = %+v", m.laneStrips)
	}
	s := m.laneStrips[0]
	if got := m.laneStripAt(s.x+1, s.y+3); got != "lane-b" {
		t.Errorf("laneStripAt inside the strip = %q", got)
	}
	if got := m.laneStripAt(s.x+s.w, s.y+3); got != "" {
		t.Errorf("laneStripAt right of the strip = %q; want none", got)
	}
	// The open lanes sit either side of it, without overlap.
	var ra, rc paneRect
	for _, r := range m.paneRects {
		switch r.paneID {
		case "pane-a":
			ra = r
		case "pane-c":
			rc = r
		}
	}
	if ra.x+ra.w > s.x || rc.x < s.x+s.w {
		t.Errorf("pane rects overlap the strip: a=%+v strip=%+v c=%+v", ra, s, rc)
	}
}

func TestCollapsedLane_ClickOpensAndFocuses(t *testing.T) {
	b := startTestBus(t)
	nc := b.Conn()
	sub, err := nc.SubscribeSync(msgpkg.T("ws", "inbox"))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	_ = nc.Flush()

	m := foldedModel()
	m.pub = b.Publisher()
	m.bus = b
	m.workspaceInbox = msgpkg.T("ws", "inbox")
	s := m.laneStrips[0]
	mm, _ := m.handleMouse(tea.MouseMsg{X: s.x + 1, Y: s.y + 2, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if mm.(Model).selection.dragging {
		t.Error("a click on a strip must not start a text selection")
	}

	var got []any
	for i := 0; i < 2; i++ {
		raw, err := sub.NextMsg(time.Second)
		if err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		var env msgpkg.NATSEnvelope
		if err := json.Unmarshal(raw.Data, &env); err != nil {
			t.Fatal(err)
		}
		d, err := b.Codecs().Decode(env.TypeTag, env.Payload)
		if err != nil {
			t.Fatalf("decode %q: %v", env.TypeTag, err)
		}
		got = append(got, d)
	}
	lc, ok := got[0].(*msgpkg.MsgLaneCollapse)
	if !ok || lc.Verb != "expand" || lc.Lane != "lane-b" {
		t.Fatalf("first message = %#v; want MsgLaneCollapse{expand, lane-b}", got[0])
	}
	if f, ok := got[1].(*msgpkg.MsgFocusPaneByID); !ok || f.ID != "pane-b" {
		t.Fatalf("second message = %#v; want MsgFocusPaneByID{pane-b}", got[1])
	}
}
