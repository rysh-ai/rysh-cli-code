// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Regression tests for the agents-board view (design 025 §6).
//
// The four properties under test are the ones the design says the view OWES,
// each traceable to a decision made elsewhere:
//
//  1. a reply renders UNDER its root, not as a second root (§6.1);
//  2. an orphan reply — one that arrives before its root, which §4.3 makes an
//     expected ordering because thread ids are minted agent-side with no round
//     trip — renders provisionally and is re-parented WITHOUT duplicating the
//     root (§6.1);
//  3. persona rendering survives the approval-pane \x1f overload and follows
//     the documented fallback chain (§5, §6.2);
//  4. the user is never trapped: the board claims scroll keys only, and esc,
//     tab switching and every multiplexer chord fall through (replay_pane.go's
//     rule, restated for a second shell-less PaneType).

const (
	boardPaneA  = "aaaaaaaa-1111-2222-3333-444444444444" // an agent that posts roots
	boardPaneB  = "bbbbbbbb-5555-6666-7777-888888888888" // a different agent that replies
	boardPaneID = "board-pane-1"                         // the agents-board pane itself
)

// buildBoardModel builds a Model whose active pane is an agents-board pane
// (PaneType "agents-board", no shell) in modeNormal, over the given store.
// No bus: nothing in the view publishes, so the tests need none.
func buildBoardModel(store *board.Store) Model {
	bp := domain.PaneSnapshot{
		ID:       boardPaneID,
		Title:    "agents-board",
		PaneType: domain.PaneTypeAgentsBoard,
		Flex:     1,
	}
	tab := domain.TabSnapshot{
		ID:           "tab-1",
		Title:        "t1",
		ActivePaneID: boardPaneID,
		Lanes: []domain.LaneSnapshot{
			{
				ID:           "lane-1",
				Flex:         1,
				ActivePaneID: boardPaneID,
				PaneGroups: []domain.PaneGroupSnapshot{
					{ID: "g-1", RowFlex: 1, ActivePaneID: boardPaneID, Panes: []domain.PaneSnapshot{bp}},
				},
			},
		},
	}
	m := Model{
		snapshot: domain.WorkspaceSnapshot{
			Tabs:         []domain.TabSnapshot{tab},
			ActiveTabID:  "tab-1",
			ActivePaneID: boardPaneID,
		},
		mode:              modeNormal,
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
		boards:            map[string]*board.Store{msgpkg.DefaultBoardID: store},
		boardViews:        map[string]*boardViewState{},
		workspaceInbox:    msgpkg.T("ws", "inbox"),
	}
	m.recomputePaneRects()
	return m
}

// root builds a post that OWNS its thread — the store treats a ThreadID of the
// shape "<posterPaneID>/<n>" as that pane opening the thread (board.ownsThread).
func boardRootPost(paneID, persona, text string, n int, ts int64) *msgpkg.MsgBoardPost {
	p := msgpkg.NewBoardPost(paneID, persona, msgpkg.BoardKindMilestone, text, ts)
	p.ThreadID = msgpkg.MintThreadID(paneID, n)
	return p
}

// reply builds a post into somebody ELSE's thread, which is what makes it a
// reply rather than a root.
func boardReplyPost(paneID, persona, text, threadID string, ts int64) *msgpkg.MsgBoardPost {
	p := msgpkg.NewBoardPost(paneID, persona, msgpkg.BoardKindReply, text, ts)
	p.ThreadID = threadID
	return p
}

func countRows(rows []string, glyph string) int {
	n := 0
	for _, r := range rows {
		if strings.Contains(r, glyph) {
			n++
		}
	}
	return n
}

func rowIndexContaining(t *testing.T, rows []string, want string) int {
	t.Helper()
	for i, r := range rows {
		if strings.Contains(r, want) {
			return i
		}
	}
	t.Fatalf("no row contains %q; rows =\n%s", want, strings.Join(rows, "\n"))
	return -1
}

// TestBoardReplyRendersUnderItsRoot: a reply is nested beneath the root it
// names, not rendered as a second root. This is the headline of the epic's
// acceptance criterion.
func TestBoardReplyRendersUnderItsRoot(t *testing.T) {
	st := board.New(0)
	thread := msgpkg.MintThreadID(boardPaneA, 1)
	st.Apply(boardRootPost(boardPaneA, "ceo", "gate one is closed", 1, 1_000))
	st.Apply(boardReplyPost(boardPaneB, "worker-3", "acknowledged, building", thread, 2_000))

	m := buildBoardModel(st)
	rows := m.boardRows("", 100)

	if got := countRows(rows, boardRootGlyph); got != 1 {
		t.Fatalf("expected exactly 1 root line, got %d; rows =\n%s", got, strings.Join(rows, "\n"))
	}
	if got := countRows(rows, boardProvisionalGlyph); got != 0 {
		t.Fatalf("expected no provisional lines, got %d; rows =\n%s", got, strings.Join(rows, "\n"))
	}

	rootIdx := rowIndexContaining(t, rows, "gate one is closed")
	replyHdrIdx := rowIndexContaining(t, rows, boardReplyGlyph)
	replyIdx := rowIndexContaining(t, rows, "acknowledged, building")

	if !(rootIdx < replyHdrIdx && replyHdrIdx < replyIdx) {
		t.Fatalf("reply must render under its root; root@%d replyHdr@%d replyText@%d; rows =\n%s",
			rootIdx, replyHdrIdx, replyIdx, strings.Join(rows, "\n"))
	}
	// The reply must be INDENTED under the root, or "under" is only vertical
	// adjacency and two consecutive roots would read identically.
	if !strings.HasPrefix(rows[replyHdrIdx], strings.Repeat(" ", boardRootIndent)+boardReplyGlyph) {
		t.Fatalf("reply header is not indented under the root: %q", rows[replyHdrIdx])
	}
}

// TestBoardOrphanReplyIsReparentedWithoutDuplicatingTheRoot: deliver the reply
// FIRST, then its root. The reply must render immediately (as a provisional
// thread), and once the root lands there must be exactly one root and the reply
// nested under it — not a provisional thread AND a real one.
func TestBoardOrphanReplyIsReparentedWithoutDuplicatingTheRoot(t *testing.T) {
	st := board.New(0)
	thread := msgpkg.MintThreadID(boardPaneA, 7)

	// 1. The orphan arrives alone.
	st.Apply(boardReplyPost(boardPaneB, "worker-3", "starting on the view", thread, 2_000))

	m := buildBoardModel(st)
	rows := m.boardRows("", 100)
	if countRows(rows, boardProvisionalGlyph) != 1 {
		t.Fatalf("an orphan reply must render as a provisional root; rows =\n%s", strings.Join(rows, "\n"))
	}
	if countRows(rows, "starting on the view") != 1 {
		t.Fatalf("an orphan reply must be visible before its root arrives; rows =\n%s", strings.Join(rows, "\n"))
	}

	// 2. The root lands late.
	st.Apply(boardRootPost(boardPaneA, "ceo", "unit 01 kickoff", 7, 1_000))
	rows = m.boardRows("", 100)

	if got := countRows(rows, boardRootGlyph); got != 1 {
		t.Fatalf("after re-parenting expected exactly 1 root, got %d; rows =\n%s", got, strings.Join(rows, "\n"))
	}
	if got := countRows(rows, boardProvisionalGlyph); got != 0 {
		t.Fatalf("after re-parenting expected no provisional lines, got %d; rows =\n%s", got, strings.Join(rows, "\n"))
	}
	if got := countRows(rows, "unit 01 kickoff"); got != 1 {
		t.Fatalf("the root must render exactly once, got %d; rows =\n%s", got, strings.Join(rows, "\n"))
	}
	if got := countRows(rows, "starting on the view"); got != 1 {
		t.Fatalf("the re-parented reply must render exactly once, got %d; rows =\n%s", got, strings.Join(rows, "\n"))
	}

	rootIdx := rowIndexContaining(t, rows, "unit 01 kickoff")
	replyIdx := rowIndexContaining(t, rows, "starting on the view")
	if rootIdx > replyIdx {
		t.Fatalf("re-parented reply must sit under its root; root@%d reply@%d; rows =\n%s",
			rootIdx, replyIdx, strings.Join(rows, "\n"))
	}
}

// TestBoardPersonaNeverLeaksTheApprovalOverload: approval panes overload
// GivenName with "requestID\x1FresponseSubject" (actors/approval_pane.go), so a
// view that renders Persona raw eventually prints a NATS subject as somebody's
// name. The view must fall back to the pane label instead.
func TestBoardPersonaNeverLeaksTheApprovalOverload(t *testing.T) {
	poisoned := "req-42\x1frysh.pane.abc.approval.response"

	// (a) The view's own guard, independent of the store: a post can reach a
	//     renderer without passing through Store.Apply.
	got := boardDisplayPersona(&msgpkg.MsgBoardPost{Persona: poisoned, PaneID: boardPaneA})
	if strings.Contains(got, "\x1f") || strings.Contains(got, "approval.response") {
		t.Fatalf("persona leaked the approval overload: %q", got)
	}
	if want := "pane-" + boardPaneA[:8]; got != want {
		t.Fatalf("persona fallback = %q, want %q", got, want)
	}

	// (b) End to end, through the store and into the rendered rows.
	st := board.New(0)
	st.Apply(boardRootPost(boardPaneA, poisoned, "a milestone", 1, 1_000))
	m := buildBoardModel(st)
	rows := m.boardRows("", 100)
	joined := strings.Join(rows, "\n")
	if strings.Contains(joined, "\x1f") || strings.Contains(joined, "approval.response") {
		t.Fatalf("rendered board leaked the approval overload; rows =\n%s", joined)
	}
	if !strings.Contains(joined, "pane-"+boardPaneA[:8]) {
		t.Fatalf("rendered board dropped the persona fallback; rows =\n%s", joined)
	}
}

// TestBoardPersonaFallbackChain: given-name, then "pane-<8>", never blank.
// A poster with no persona is a FIRST-CLASS citizen (founder gate 3: every
// claude may post, not only fleet members), so it must get a stable label
// rather than an empty column.
func TestBoardPersonaFallbackChain(t *testing.T) {
	cases := []struct {
		name    string
		persona string
		paneID  string
		want    string
	}{
		{"a clean given-name is used verbatim", "mgr-01", boardPaneA, "mgr-01"},
		{"an empty persona falls back to the pane label", "", boardPaneA, "pane-" + boardPaneA[:8]},
		{"a poisoned persona falls back to the pane label", "x\x1fy", boardPaneA, "pane-" + boardPaneA[:8]},
		{"no persona and no pane id still yields a label", "", "", "pane-unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := boardDisplayPersona(&msgpkg.MsgBoardPost{Persona: tc.persona, PaneID: tc.paneID})
			if got != tc.want {
				t.Fatalf("persona = %q, want %q", got, tc.want)
			}
			if got == "" {
				t.Fatal("persona must never render blank")
			}
		})
	}
}

// TestBoardUserIsNeverTrapped: the board claims scroll keys and enter, and
// NOTHING else. esc, tab switching and the multiplexer chords must fall through
// to the global key handling, or a user who focuses a board pane cannot leave
// it. This is the failure mode replay_pane.go's comment warns about, restated
// for the second shell-less PaneType.
func TestBoardUserIsNeverTrapped(t *testing.T) {
	st := board.New(0)
	st.Apply(boardRootPost(boardPaneA, "ceo", "a milestone", 1, 1_000))

	mustPassThrough := []tea.KeyMsg{
		{Type: tea.KeyEsc},
		{Type: tea.KeyRunes, Runes: []rune{'['}},
		{Type: tea.KeyRunes, Runes: []rune{']'}},
		{Type: tea.KeyCtrlO},
		{Type: tea.KeyCtrlP},
		{Type: tea.KeyCtrlT},
		{Type: tea.KeyCtrlW},
		{Type: tea.KeyCtrlL},
		{Type: tea.KeyCtrlN},
		{Type: tea.KeyCtrlA},
		{Type: tea.KeyLeft, Alt: true},
		{Type: tea.KeyRight, Alt: true},
	}
	for _, k := range mustPassThrough {
		m := buildBoardModel(st)
		if handled, _ := m.updateBoardPaneInput(k); handled {
			t.Fatalf("the board swallowed %q — the user would be trapped", k.String())
		}
	}

	mustBeClaimed := []tea.KeyMsg{
		{Type: tea.KeyUp},
		{Type: tea.KeyDown},
		{Type: tea.KeyPgUp},
		{Type: tea.KeyPgDown},
		{Type: tea.KeyHome},
		{Type: tea.KeyEnd},
		{Type: tea.KeyRunes, Runes: []rune{'j'}},
		{Type: tea.KeyRunes, Runes: []rune{'k'}},
		{Type: tea.KeyRunes, Runes: []rune{'g'}},
		{Type: tea.KeyRunes, Runes: []rune{'G'}},
		{Type: tea.KeyEnter},
	}
	for _, k := range mustBeClaimed {
		m := buildBoardModel(st)
		if handled, _ := m.updateBoardPaneInput(k); !handled {
			t.Fatalf("the board did not claim %q; it would leak into the input line of a shell-less pane", k.String())
		}
	}

	// End to end through the real Update loop: a multiplexer chord still
	// reaches the mode machinery while a board pane is focused.
	m := buildBoardModel(st)
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlP})
	if got := updated.(Model).mode; got != modePane {
		t.Fatalf("ctrl+p from a focused board pane left mode = %v, want modePane", got)
	}
}

// TestBoardKeysOnlyClaimedForBoardPanes: the same keys on a normal pane are not
// intercepted, so j/k keep typing into an ordinary shell pane.
func TestBoardKeysOnlyClaimedForBoardPanes(t *testing.T) {
	m := buildBoardModel(board.New(0))
	m.snapshot.Tabs[0].Lanes[0].PaneGroups[0].Panes[0].PaneType = domain.PaneTypeNormal

	for _, k := range []tea.KeyMsg{{Type: tea.KeyUp}, {Type: tea.KeyRunes, Runes: []rune{'j'}}, {Type: tea.KeyEnter}} {
		if handled, _ := m.updateBoardPaneInput(k); handled {
			t.Fatalf("key %q was claimed on a NORMAL pane", k.String())
		}
	}
}

// TestBoardPanelIsReachableFromBuildPanePanel: the render dispatch is wired, so
// an agents-board pane actually shows the board instead of an empty buffer.
// Without this the view is code nothing calls.
func TestBoardPanelIsReachableFromBuildPanePanel(t *testing.T) {
	st := board.New(0)
	st.Apply(boardRootPost(boardPaneA, "mgr-01", "wave three dispatched", 1, 1_000))

	m := buildBoardModel(st)
	pane := m.snapshot.Tabs[0].Lanes[0].PaneGroups[0].Panes[0]
	out := m.buildPanePanel(pane, m.snapshot.Tabs[0], 100, 20)

	if !strings.Contains(out, "AGENTS-BOARD") {
		t.Fatalf("buildPanePanel did not route an agents-board pane to the board view:\n%s", out)
	}
	if !strings.Contains(out, "wave three dispatched") {
		t.Fatalf("the board pane did not render its posts:\n%s", out)
	}
	if !strings.Contains(out, "mgr-01") {
		t.Fatalf("the board pane did not attribute the post:\n%s", out)
	}
}

// TestBoardTextIsSanitised: post text is untrusted — any process that can reach
// the session's NATS can post (design 025 §7.5) — so escape sequences must not
// reach the terminal, where one post could repaint the pane and hide every
// other agent's messages.
func TestBoardTextIsSanitised(t *testing.T) {
	st := board.New(0)
	st.Apply(boardRootPost(boardPaneA, "ceo", "before\x1b[2Jafter\x1b[31m", 1, 1_000))

	m := buildBoardModel(st)
	joined := strings.Join(m.boardRows("", 100), "\n")

	if strings.Contains(joined, "\x1b") {
		t.Fatalf("an escape sequence survived into the rendered board: %q", joined)
	}
	if !strings.Contains(joined, "before") || !strings.Contains(joined, "after") {
		t.Fatalf("sanitising dropped the readable text: %q", joined)
	}
}

// TestBoardScrollClampsToTheStream: G/end returns to the live tail and the
// offset never runs past the top, so the board cannot be scrolled into an
// empty region and stranded there.
func TestBoardScrollClampsToTheStream(t *testing.T) {
	st := board.New(0)
	for i := 1; i <= 30; i++ {
		st.Apply(boardRootPost(boardPaneA, "ceo", "milestone", i, int64(i)*1_000))
	}
	m := buildBoardModel(st)
	pane := m.snapshot.Tabs[0].Lanes[0].PaneGroups[0].Panes[0]

	// A render is what measures the pane; the key handler clamps against it.
	_ = m.buildBoardPanel(pane, 100, 12)
	stt := m.boardViewFor(boardPaneID)

	if stt.scrollOffset != 0 {
		t.Fatalf("a fresh board must be pinned to the live tail, got offset %d", stt.scrollOffset)
	}

	for i := 0; i < 500; i++ {
		m.updateBoardPaneInput(tea.KeyMsg{Type: tea.KeyUp})
	}
	maxOff := maxBoardScroll(stt.lastRows, stt.lastVisible)
	if maxOff == 0 {
		t.Fatal("test is not exercising scrolling: the stream fits the pane")
	}
	if stt.scrollOffset != maxOff {
		t.Fatalf("scroll ran past the top: offset %d, max %d", stt.scrollOffset, maxOff)
	}

	m.updateBoardPaneInput(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'G'}})
	if stt.scrollOffset != 0 {
		t.Fatalf("G must return to the live tail, got offset %d", stt.scrollOffset)
	}
}
