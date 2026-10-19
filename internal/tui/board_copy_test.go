// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Copying out of an agents-board pane: mouse selection, y / Y, and the
// daemon's `##board copy` hand-off. The mouse bug these pin was that
// extractSelectedText read buildDisplayLines — a buffer a board pane never
// displays — so a drag in a board pane copied nothing useful.

// captureClipboard swaps writeClipboard for the test and returns what would
// have been copied.
func captureClipboard(t *testing.T) *[]string {
	t.Helper()
	var got []string
	prev := writeClipboard
	writeClipboard = func(s string) error { got = append(got, s); return nil }
	t.Cleanup(func() { writeClipboard = prev })
	return &got
}

// boardFrame returns the board pane's rect and the plain lines it shows.
func boardFrame(t *testing.T, m Model) (paneRect, []string) {
	t.Helper()
	if len(m.paneRects) == 0 {
		t.Fatal("no pane rects")
	}
	r := m.paneRects[0]
	pane := m.snapshot.Tabs[0].Lanes[0].PaneGroups[0].Panes[0]
	lay := m.boardPanelLayout(pane, r.w+2, r.h)
	return r, lay.plainLines(r.w)
}

func selectRows(m *Model, startRow, startCol, endRow, endCol int) {
	m.selection = mouseSelection{active: true, paneID: boardPaneID,
		startX: startCol, startY: startRow, endX: endCol, endY: endRow}
}

func rowOf(t *testing.T, lines []string, want string) int {
	t.Helper()
	for i, l := range lines {
		if strings.Contains(l, want) {
			return i
		}
	}
	t.Fatalf("no screen row contains %q:\n%s", want, strings.Join(lines, "\n"))
	return -1
}

func TestBoardMouseCopyReturnsTheHighlightedRows(t *testing.T) {
	st := board.New(0)
	st.Apply(boardRootPost(boardPaneA, "issue-314", "STATUS: green", 1, 1_000))
	st.Apply(boardRootPost(boardPaneB, "issue-31", "BLOCKED: waiting on CI", 1, 2_000))
	m := buildBoardModel(st)
	r, lines := boardFrame(t, m)

	from := rowOf(t, lines, "issue-314")
	to := rowOf(t, lines, "BLOCKED")
	selectRows(&m, from, 0, to, r.w-1)
	got := m.extractSelectedText()

	for _, want := range []string{"issue-314", "STATUS: green", "issue-31", "BLOCKED: waiting on CI"} {
		if !strings.Contains(got, want) {
			t.Fatalf("copied text is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "AGENTS-BOARD") {
		t.Fatalf("the header was not selected but was copied:\n%s", got)
	}
}

// TestBoardMouseCopyIncludesTheHeaderLine: row 0 of the pane is the header.
func TestBoardMouseCopyIncludesTheHeaderLine(t *testing.T) {
	st := board.New(0)
	st.Apply(boardRootPost(boardPaneA, "issue-314", "STATUS: green", 1, 1_000))
	m := buildBoardModel(st)
	r, lines := boardFrame(t, m)

	selectRows(&m, 0, 0, rowOf(t, lines, "STATUS"), r.w-1)
	got := m.extractSelectedText()
	if !strings.HasPrefix(got, "AGENTS-BOARD") || !strings.Contains(got, "STATUS: green") {
		t.Fatalf("header-inclusive selection copied:\n%s", got)
	}
}

// TestBoardMouseCopyWhenScrolled: with scrollOffset > 0 the copied rows are
// the ones on screen, not the live tail's.
func TestBoardMouseCopyWhenScrolled(t *testing.T) {
	st := board.New(0)
	for i := 1; i <= 40; i++ {
		st.Apply(boardRootPost(boardPaneA, "issue-314", "milestone-"+string(rune('A'+i%26))+strings.Repeat("x", i), i, int64(i)*1_000))
	}
	m := buildBoardModel(st)
	pane := m.snapshot.Tabs[0].Lanes[0].PaneGroups[0].Panes[0]
	r := m.paneRects[0]
	_ = m.buildBoardPanel(pane, r.w+2, r.h) // measure, as a frame would
	m.boardViewFor(boardPaneID).scrollOffset = 17

	_, lines := boardFrame(t, m)
	if m.boardViewFor(boardPaneID).scrollOffset != 17 {
		t.Fatal("test is not scrolled")
	}
	row := 5
	want := strings.TrimRight(lines[row], " ")
	selectRows(&m, row, 0, row, r.w-1)
	if got := m.extractSelectedText(); got != want && got != board.SanitiseBody(strings.TrimSpace(want)) {
		t.Fatalf("scrolled copy = %q, want the on-screen row %q", got, want)
	}

	// And it is NOT the unscrolled row at the same position.
	m.boardViewFor(boardPaneID).scrollOffset = 0
	_, live := boardFrame(t, m)
	if strings.TrimRight(live[row], " ") == want {
		t.Fatal("test is not exercising scrolling: the row is the same at offset 0")
	}
}

// TestBoardMouseCopyJoinsAWrappedPost: a post wrapped over several rows and
// wholly selected is copied as its original text — the 200-character URL
// whole, not in pane-width pieces.
func TestBoardMouseCopyJoinsAWrappedPost(t *testing.T) {
	url := "https://ci.example.com/runs/" + strings.Repeat("0123456789", 18)
	body := "RUN: " + url + "\nSTATUS: green"
	st := board.New(0)
	st.Apply(boardRootPost(boardPaneA, "issue-314", body, 1, 1_000))
	m := buildBoardModel(st)
	r, lines := boardFrame(t, m)

	head := rowOf(t, lines, "issue-314")
	last := rowOf(t, lines, "STATUS: green")
	if last-head < 3 {
		t.Fatalf("the post did not wrap (rows %d..%d); widen the body", head, last)
	}

	selectRows(&m, head, 0, last, r.w-1)
	got := m.extractSelectedText()
	if !strings.HasSuffix(got, "\n"+body) {
		t.Fatalf("a wholly selected wrapped post must copy its original text:\n%q\nwant suffix\n%q", got, body)
	}
	if !strings.Contains(got, "issue-314") {
		t.Fatalf("the selected header row was dropped:\n%s", got)
	}

	// Part of the post: copy what is visible, fragments and all.
	selectRows(&m, head+1, 0, head+2, r.w-1)
	part := m.extractSelectedText()
	if strings.Contains(part, url) || !strings.Contains(part, "RUN:") || !strings.Contains(part, "https://ci.example.com") {
		t.Fatalf("a partial selection must copy the visible rows only, got %q", part)
	}
}

// TestBoardMouseEventsReachSelection: press, drag, release on a board pane go
// through handleMouse to the clipboard — nothing on the board path swallows
// them.
func TestBoardMouseEventsReachSelection(t *testing.T) {
	got := captureClipboard(t)
	st := board.New(0)
	st.Apply(boardRootPost(boardPaneA, "issue-314", "STATUS: green", 1, 1_000))
	m := buildBoardModel(st)
	r, lines := boardFrame(t, m)
	row := rowOf(t, lines, "STATUS")

	var tm tea.Model = m
	tm, _ = tm.(Model).handleMouse(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: r.x, Y: r.y + row})
	tm, _ = tm.(Model).handleMouse(tea.MouseMsg{Action: tea.MouseActionMotion, X: r.x + r.w - 1, Y: r.y + row})
	tm, _ = tm.(Model).handleMouse(tea.MouseMsg{Action: tea.MouseActionRelease, X: r.x + r.w - 1, Y: r.y + row})

	if len(*got) != 1 || !strings.Contains((*got)[0], "STATUS: green") {
		t.Fatalf("a drag over a board post copied %q", *got)
	}
	if flash := tm.(Model).flashMsg; !strings.HasPrefix(flash, "copied ") {
		t.Fatalf("flash = %q, want the same confirmation as other panes", flash)
	}
}

func TestBoardYCopiesNewestAndShiftYTheVisibleWindow(t *testing.T) {
	got := captureClipboard(t)
	url := "https://github.com/o/r/pull/" + strings.Repeat("9", 180)
	st := board.New(0)
	st.Apply(boardRootPost(boardPaneA, "issue-314", "STATUS: green", 1, 1_000))
	st.Apply(boardRootPost(boardPaneB, "issue-31", "PR "+url, 1, 2_000))
	m := buildBoardModel(st)

	if handled, _ := m.updateBoardPaneInput(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}}); !handled {
		t.Fatal("y was not claimed by the board pane")
	}
	if len(*got) != 1 || !strings.HasSuffix((*got)[0], "\nPR "+url) || strings.Contains((*got)[0], "STATUS") {
		t.Fatalf("y copied %q; want only the newest message, unwrapped", *got)
	}
	if m.flashMsg != "[board] copied 1 of 2 messages to the clipboard" {
		t.Fatalf("y flash = %q", m.flashMsg)
	}

	if handled, _ := m.updateBoardPaneInput(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'Y'}}); !handled {
		t.Fatal("Y was not claimed by the board pane")
	}
	// Both messages are on screen, so Y copies both — through the shared
	// formatter, byte for byte what `##board copy` produces.
	reply := board.Answer(st, board.Query{})
	want, _ := board.BuildCopy(&reply, "", 0)
	if len(*got) != 2 || (*got)[1] != want.Text {
		t.Fatalf("Y copied %q, want %q", (*got)[1:], want.Text)
	}
}

// TestBoardShiftYCopiesOnlyWhatIsOnScreen: scrolled back, Y leaves out the
// messages below the window.
func TestBoardShiftYCopiesOnlyWhatIsOnScreen(t *testing.T) {
	got := captureClipboard(t)
	st := board.New(0)
	for i := 1; i <= 40; i++ {
		st.Apply(boardRootPost(boardPaneA, "issue-314", fmt.Sprintf("message-%02d", i), i, int64(i)*1_000))
	}
	m := buildBoardModel(st)
	pane := m.snapshot.Tabs[0].Lanes[0].PaneGroups[0].Panes[0]
	r := m.paneRects[0]
	_ = m.buildBoardPanel(pane, r.w+2, r.h)
	m.boardViewFor(boardPaneID).scrollOffset = maxBoardScroll(m.boardViewFor(boardPaneID).lastRows, m.boardViewFor(boardPaneID).lastVisible)

	m.updateBoardPaneInput(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'Y'}})
	if len(*got) != 1 {
		t.Fatalf("Y copied %d times", len(*got))
	}
	if !strings.Contains((*got)[0], "message-01") || strings.Contains((*got)[0], "message-40") {
		t.Fatalf("Y at the top of the board copied:\n%s", (*got)[0])
	}
	if !strings.Contains(m.flashMsg, "of 40 messages") {
		t.Fatalf("flash = %q", m.flashMsg)
	}
}

func TestBoardOWritesTheVisibleWindowAsHTML(t *testing.T) {
	var req board.OutputRequest
	prev := writeBoardOutput
	writeBoardOutput = func(r board.OutputRequest) (string, error) { req = r; return "/tmp/x.html", nil }
	t.Cleanup(func() { writeBoardOutput = prev })

	st := board.New(0)
	st.Apply(boardRootPost(boardPaneA, "issue-314", "STATUS: green", 1, 1_000))
	m := buildBoardModel(st)
	handled, cmd := m.updateBoardPaneInput(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})
	if !handled || cmd == nil {
		t.Fatal("o was not claimed")
	}
	done, ok := cmd().(boardOutputDoneMsg)
	if !ok || done.text != "[board] wrote 1 of 1 messages → /tmp/x.html" {
		t.Fatalf("o result = %+v", done)
	}
	if req.Format != board.FormatHTML || !req.Open || len(req.Page.Picked) != 1 {
		t.Fatalf("o request = %+v", req)
	}
}

// TestBoardCopyKeysAreLettersWhileComposing: in `i` input mode y, Y and o type.
func TestBoardCopyKeysAreLettersWhileComposing(t *testing.T) {
	got := captureClipboard(t)
	st := board.New(0)
	st.Apply(boardRootPost(boardPaneA, "issue-314", "STATUS: green", 1, 1_000))
	m := buildBoardModel(st)

	m.updateBoardPaneInput(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'i'}})
	for _, r := range "yYo" {
		m.updateBoardPaneInput(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if len(*got) != 0 {
		t.Fatalf("copied while composing: %q", *got)
	}
	if v := m.inputs[boardPaneID].Value(); v != "yYo" {
		t.Fatalf("composing input = %q, want \"yYo\"", v)
	}
}

func TestBoardYOnAnEmptyBoardRefusesInsteadOfCopyingNothing(t *testing.T) {
	got := captureClipboard(t)
	m := buildBoardModel(board.New(0))
	m.updateBoardPaneInput(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if len(*got) != 0 {
		t.Fatalf("an empty board was copied: %q", *got)
	}
	if !strings.HasPrefix(m.flashMsg, "[board] no messages") {
		t.Fatalf("flash = %q", m.flashMsg)
	}
}

func TestBoardKeyHelpNamesTheCopyKeys(t *testing.T) {
	h := buildBoardModel(board.New(0)).boardKeyHelp()
	for _, want := range []string{"y copy newest", "Y copy visible", "o open visible as HTML"} {
		if !strings.Contains(h, want) {
			t.Fatalf("boardKeyHelp does not document %q: %q", want, h)
		}
	}
}

// TestDaemonClipboardHandOff: the daemon's `##board copy` text goes through
// the TUI's one clipboard path, and an empty request is refused.
func TestDaemonClipboardHandOff(t *testing.T) {
	got := captureClipboard(t)
	resp, flash := answerClipboardWrite(&msgpkg.MsgClipboardWrite{PaneID: "p", Text: "issue-314  [milestone]\nhello"})
	if !resp.OK || len(*got) != 1 || (*got)[0] != "issue-314  [milestone]\nhello" || flash == "" {
		t.Fatalf("hand-off = %+v, copied %q, flash %q", resp, *got, flash)
	}
	if resp, _ := answerClipboardWrite(&msgpkg.MsgClipboardWrite{}); resp.OK {
		t.Fatal("an empty copy was accepted")
	}
}
