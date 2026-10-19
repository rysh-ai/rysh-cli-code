// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Design 052: f shows the board as a flow — the chain tree with prompt edges
// over a sequence diagram — built from the same golden fixture the model and
// the app's port are tested against.

// boardFlowModel loads ../board/testdata/flow-input.json into a store and the
// snapshot, with the board pane on the fixture's board.
func boardFlowModel(t *testing.T) Model {
	t.Helper()
	raw, err := os.ReadFile("../board/testdata/flow-input.json")
	if err != nil {
		t.Fatal(err)
	}
	var in struct {
		Board   string           `json:"board"`
		Threads []board.Thread   `json:"threads"`
		Panes   []board.FlowPane `json:"panes"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatal(err)
	}
	var posts []*msgpkg.MsgBoardPost
	for _, th := range in.Threads {
		if th.Root != nil {
			posts = append(posts, th.Root)
		}
		posts = append(posts, th.Replies...)
	}
	sort.SliceStable(posts, func(i, j int) bool { return posts[i].TS < posts[j].TS })

	m := buildBoardModel(board.New(0))
	g := &m.snapshot.Tabs[0].Lanes[0].PaneGroups[0]
	g.Panes[0].Meta = map[string]string{boardIDMetaKey: in.Board}
	for _, p := range in.Panes {
		g.Panes = append(g.Panes, domain.PaneSnapshot{ID: p.ID, GivenName: p.Name, Meta: p.Meta})
	}
	store := m.boardStore(in.Board)
	for _, p := range posts {
		store.Apply(p)
	}
	return m
}

func boardViewTextWidth(m Model, width int) string {
	pane := domain.PaneSnapshot{ID: boardPaneID, PaneType: domain.PaneTypeAgentsBoard}
	lay := m.boardPanelLayout(pane, width, 60)
	return lay.header + "\n" + strings.Join(lay.rows, "\n")
}

func TestBoardFlowViewRenders(t *testing.T) {
	m := boardFlowModel(t)
	pressBoardKey(t, &m, 'f')
	if v := m.boardViewFor(boardPaneID).boardView(); v != board.ViewFlow {
		t.Fatalf("f set view %q, want flow", v)
	}
	got := boardViewTextWidth(m, 200)
	if testing.Verbose() {
		fmt.Println(got)
	}
	for _, want := range []string{
		"showing flow · 5 panes · 1 blocked · 5 prompts (3 answered · 1 undelivered · 1 open) · 4 status · keys s p a f t",
		// The tree: ceo at the root, its prompt edges, mgr-a under it.
		"ceo [queue]  ceo · 3 posts",
		"──▶ mgr-a  1 prompt · 1 ✓ · ack 2.0s",
		"──▶ wk-a2  1 prompt · 1 open",
		"└─ mgr-a [queue] {progress·working}  manager · 4 posts · plan the parser",
		"──▶ wk-a1  2 prompts · 1 ✓ · 1 ✗ · ack 1.0s",
		"└─ wk-a1 [grid] {blocked·idle}  worker · 3 posts · parser",
		// Design 054: the ledger's blocked_by, member and human alike.
		"⧗ blocked by mgr-a",
		"⧗ blocked by human",
		"└─ wk-a3 [queue] {progress·STALLED}  worker · 1 post · tests red",
		// The dependency tree, rooted at the bottleneck.
		"dependencies · 1 blocked · bottleneck: mgr-a (1 waiting)",
		"▣ mgr-a {progress·working}  ← 1 waiting",
		"└─ wk-a1 {blocked·idle}  · also blocked by human",
		"listener  1 post",
		"╳ wk-a1  1 undelivered",
		// The fleet actor's check-in, answered by wk-a3's status reply.
		"fleet  2 posts",
		"──▶ wk-a3  1 prompt · 1 ✓ · ack 1.5s",
		"fleet [fleet-alert] wk-a3 is STALLED",
		// The sequence.
		"prompt msg-0001 · [FLEET fleet-x | WORK ORDER | FROM ceo]",
		"ack msg-0001 · ack",
		"undelivered msg-0002 · pane not answering after 9m",
		"mgr-a [milestone] started the unit",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("flow view missing %q:\n%s", want, got)
		}
	}
	for _, glyph := range []string{"▶", "◀", "╳", "●", "○"} {
		if !strings.Contains(got, glyph) {
			t.Errorf("sequence diagram has no %q:\n%s", glyph, got)
		}
	}
	// Newest at the bottom.
	if i, j := strings.Index(got, "prompt msg-0001"), strings.Index(got, "prompt msg-0004"); i < 0 || j < i {
		t.Errorf("events must run oldest → newest:\n%s", got)
	}
}

func TestBoardFlowArrowsPointAtTheRecipient(t *testing.T) {
	m := boardFlowModel(t)
	f := m.boardFlow("fleet-x")
	cols := boardFlowColumns(f)
	head, rows := boardFlowSequence(f, cols, 200)
	// Columns: ceo, mgr-a, wk-a1, wk-a2, wk-a3 (chain order), listener, board.
	if strings.Contains(head, "agents-b") {
		t.Errorf("the board pane itself is not an agent lifeline: %q", head)
	}
	for _, l := range []string{"ceo", "mgr-a", "wk-a1", "wk-a2", "wk-a3", "listener", "fleet", "fleet-x"} {
		if !strings.Contains(head, l) {
			t.Errorf("lifeline header missing %q: %q", l, head)
		}
	}
	if strings.Index(head, "ceo") > strings.Index(head, "mgr-a") || strings.Index(head, "listener") > strings.Index(head, "fleet-x") {
		t.Errorf("lifelines out of order: %q", head)
	}
	byEvent := func(prefix string) string {
		for _, r := range rows {
			if strings.Contains(r, prefix) {
				return r
			}
		}
		t.Fatalf("no row for %q", prefix)
		return ""
	}
	// ceo → mgr-a: left to right, head on the right.
	if r := byEvent("prompt msg-0001"); !strings.Contains(r, "├") || strings.Index(r, "├") > strings.Index(r, "▶") {
		t.Errorf("prompt arrow: %q", r)
	}
	// mgr-a's ack back to ceo: right to left, dashed.
	if r := byEvent("ack msg-0001"); !strings.Contains(r, "◀╌") {
		t.Errorf("ack arrow: %q", r)
	}
	// The listener's bounce ends in ╳ on wk-a1.
	if r := byEvent("undelivered msg-0002"); !strings.Contains(r, "╳") {
		t.Errorf("undelivered row: %q", r)
	}
}

func TestBoardFlowScrollsEventsKeepsTree(t *testing.T) {
	m := boardFlowModel(t)
	store := m.boardStore("fleet-x")
	const mgr = "c0000000-0000-4000-8000-000000000002"
	for i := 0; i < 60; i++ {
		store.Apply(boardRootPost(mgr, "mgr-a", fmt.Sprintf("tick %02d", i), 100+i, int64(20_000+i)))
	}
	pressBoardKey(t, &m, 'f')
	got := boardViewText(m)
	if !strings.Contains(got, "tick 59") || strings.Contains(got, "tick 00") {
		t.Errorf("flow view must open on the newest events:\n%s", got)
	}
	if !strings.Contains(got, "ceo [queue]") {
		t.Errorf("the tree must stay on screen while events scroll:\n%s", got)
	}
	m.updateBoardPaneInput(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})
	got = boardViewText(m)
	if !strings.Contains(got, "prompt msg-0001") || strings.Contains(got, "tick 59") {
		t.Errorf("g must scroll the sequence to its oldest event:\n%s", got)
	}
	if !strings.Contains(got, "ceo [queue]") {
		t.Errorf("tree lost after scrolling:\n%s", got)
	}
}

func TestBoardFlowEmptyBoard(t *testing.T) {
	m := buildBoardModel(board.New(0))
	pressBoardKey(t, &m, 'f')
	got := boardViewText(m)
	if !strings.Contains(got, "showing flow · 0 panes · 0 prompts") {
		t.Errorf("header:\n%s", got)
	}
	if !strings.Contains(got, "No board traffic yet") && !strings.Contains(got, "does NOT mean nobody posted") {
		t.Errorf("an empty flow must say so:\n%s", got)
	}
}

func TestBoardFlowKeyIsALetterWhileComposing(t *testing.T) {
	m := boardViewModel(t)
	pressBoardKey(t, &m, 'i')
	m.updateBoardPaneInput(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	if v := m.boardViewFor(boardPaneID).boardView(); v != board.ViewStatus {
		t.Errorf("typing f while composing changed the view to %q", v)
	}
	if !strings.Contains(m.boardKeyHelp(), "f flow") {
		t.Errorf("key help must name f: %q", m.boardKeyHelp())
	}
}

func TestBoardFlowLatencyFormat(t *testing.T) {
	for ms, want := range map[int64]string{850: "850ms", 4100: "4.1s", 185_000: "3m05s"} {
		if got := boardFlowLatency(ms); got != want {
			t.Errorf("boardFlowLatency(%d) = %q, want %q", ms, got, want)
		}
	}
}
