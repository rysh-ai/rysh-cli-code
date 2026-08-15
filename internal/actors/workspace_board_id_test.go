// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"strings"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Which board does a post go to? (design 028 §6.1, gate D-12.)
//
// The fixture is deliberately the SAME shape as newBoardTestWorkspace: two
// tabs, a human's pane and an agent's. The only addition is `board.id` meta,
// because that is the entire mechanism — there is no fleet, no registry and no
// new message type involved in deciding a post's board.

func newBoardIDTestWorkspace(t *testing.T) (*WorkspaceActor, *nats.Conn) {
	t.Helper()
	nc := startInProcessNATS(t)
	codecs := msg.DefaultCodecRegistry()

	serveBoardTab(t, nc, codecs, "tab-A",
		boardTestPane{id: "pA", title: "pA", givenName: "human-pane"})
	serveBoardTab(t, nc, codecs, "tab-B",
		boardTestPane{id: "pB", title: "brave-otter", givenName: "wkr-07",
			meta: map[string]string{paneMetaBoardID: "epic-07"}},
		boardTestPane{id: "pC", title: "keen-lynx", givenName: "wkr-08",
			meta: map[string]string{paneMetaBoardID: "epic-08"}},
		boardTestPane{id: "pD", title: "plain-pane", givenName: "wkr-none"})

	w := &WorkspaceActor{
		pub:          msg.NewNATSPublisher(nc, codecs),
		tabs:         []*tabInfo{{id: "tab-A", title: "A"}, {id: "tab-B", title: "B"}},
		activeTabIdx: 0,
		activePaneID: "pA",
	}
	return w, nc
}

// TestAPostGoesToThePostersOwnBoard is the property every fleet brief depends
// on: an agent posts exactly as it always did — no --board, no new flag — and
// its milestone lands on ITS board because its pane carries `board.id`.
//
// Without it, every one of 25 fleets' briefs would need editing on the day the
// second board appears, and any brief that was missed would post into the
// session board while reporting success.
func TestAPostGoesToThePostersOwnBoard(t *testing.T) {
	w, nc := newBoardIDTestWorkspace(t)
	traffic := recordSubject(t, nc, msg.T("board", ">"))

	resp := w.handleCLIBoardPost(&msg.MsgCLIBoardPost{AsPaneID: "pB", Text: "seven done"})
	if resp == nil || !resp.OK {
		t.Fatalf("post refused: %+v", resp)
	}
	settle(t, nc)

	subjects, _ := traffic.seen()
	if len(subjects) != 1 {
		t.Fatalf("subjects = %v, want exactly one board publish", subjects)
	}
	if want := msg.BoardPostSubject("epic-07"); subjects[0] != want {
		t.Fatalf("post went to %q, want %q — a fleet's milestone landed on another board",
			subjects[0], want)
	}
	if !strings.Contains(resp.Output, "epic-07") {
		t.Errorf("the confirmation does not name the board it posted to: %q", resp.Output)
	}
}

// TestAPaneWithNoBoardMetaPostsToTheSessionBoard is founder gate 3, kept
// structural: every claude may post, and a claude that belongs to no fleet
// needs no board meta, no registry entry and no permission to do it.
func TestAPaneWithNoBoardMetaPostsToTheSessionBoard(t *testing.T) {
	w, nc := newBoardIDTestWorkspace(t)
	traffic := recordSubject(t, nc, msg.T("board", ">"))

	resp := w.handleCLIBoardPost(&msg.MsgCLIBoardPost{AsPaneID: "pD", Text: "just a claude"})
	if resp == nil || !resp.OK {
		t.Fatalf("post refused: %+v", resp)
	}
	settle(t, nc)

	subjects, _ := traffic.seen()
	if len(subjects) != 1 || subjects[0] != msg.BoardPostSubject("") {
		t.Fatalf("subjects = %v, want the session board's legacy subject %q",
			subjects, msg.BoardPostSubject(""))
	}
}

// TestAnExplicitBoardOverridesThePanesOwn — `--board` on the request wins, so a
// supervisor can report onto a board it is not itself a member of.
func TestAnExplicitBoardOverridesThePanesOwn(t *testing.T) {
	w, nc := newBoardIDTestWorkspace(t)
	traffic := recordSubject(t, nc, msg.T("board", ">"))

	resp := w.handleCLIBoardPost(&msg.MsgCLIBoardPost{
		AsPaneID: "pB", Board: "epic-08", Text: "cross-posting deliberately"})
	if resp == nil || !resp.OK {
		t.Fatalf("post refused: %+v", resp)
	}
	settle(t, nc)

	subjects, _ := traffic.seen()
	if len(subjects) != 1 || subjects[0] != msg.BoardPostSubject("epic-08") {
		t.Fatalf("subjects = %v, want epic-08's subject", subjects)
	}
}

// TestAnInvalidBoardIsRefusedAndNothingIsPublished.
//
// REFUSED, not downgraded to the session board. An agent told its milestone
// landed while it went somewhere else is the receipt-without-delivery failure
// this whole track keeps re-learning — and a board id that is not a legal
// subject token would publish into a subject nobody subscribes to.
func TestAnInvalidBoardIsRefusedAndNothingIsPublished(t *testing.T) {
	w, nc := newBoardIDTestWorkspace(t)
	traffic := recordSubject(t, nc, msg.T("board", ">"))

	for _, bad := range []string{"epic.07", "*", ">", "has space"} {
		resp := w.handleCLIBoardPost(&msg.MsgCLIBoardPost{
			AsPaneID: "pB", Board: bad, Text: "should not land"})
		if resp == nil || resp.OK {
			t.Fatalf("board %q was accepted: %+v", bad, resp)
		}
	}
	settle(t, nc)

	if subjects, _ := traffic.seen(); len(subjects) != 0 {
		t.Fatalf("a refused post still published to %v", subjects)
	}
}

// TestBoardMetaThatIsNotALegalIDFallsBackToTheSession. Meta is free-form and
// hand-writable (`##pane meta set board.id …`), so the READING side cannot
// assume it is valid. Falling back to the session board keeps the pane posting
// somewhere a human will actually look, which is the safest wrong answer;
// publishing into an unsubscribed subject is the worst one.
func TestBoardMetaThatIsNotALegalIDFallsBackToTheSession(t *testing.T) {
	nc := startInProcessNATS(t)
	codecs := msg.DefaultCodecRegistry()
	serveBoardTab(t, nc, codecs, "tab-A",
		boardTestPane{id: "pX", title: "pX", givenName: "wkr",
			meta: map[string]string{paneMetaBoardID: "not a board id"}})
	w := &WorkspaceActor{
		pub:          msg.NewNATSPublisher(nc, codecs),
		tabs:         []*tabInfo{{id: "tab-A", title: "A"}},
		activeTabIdx: 0,
		activePaneID: "pX",
	}
	traffic := recordSubject(t, nc, msg.T("board", ">"))

	resp := w.handleCLIBoardPost(&msg.MsgCLIBoardPost{AsPaneID: "pX", Text: "still posts"})
	if resp == nil || !resp.OK {
		t.Fatalf("post refused: %+v", resp)
	}
	settle(t, nc)

	subjects, _ := traffic.seen()
	if len(subjects) != 1 || subjects[0] != msg.BoardPostSubject("") {
		t.Fatalf("subjects = %v, want the session board", subjects)
	}
}

// TestBoardFlagParsingRefusesAMissingValue. `##board post --board` with nothing
// after it must stop the command, not post to the session board while the human
// believes they named a fleet — F-19's rule, applied to the new flag.
func TestBoardFlagParsingRefusesAMissingValue(t *testing.T) {
	if _, _, err := extractBoardFlag([]string{"post", "--board"}); err == nil {
		t.Fatal("--board with no value was accepted")
	}
	if _, _, err := extractBoardFlag([]string{"post", "--board", "epic.07", "text"}); err == nil {
		t.Fatal("--board with an illegal id was accepted")
	}

	rest, id, err := extractBoardFlag([]string{"post", "--board", "epic-07", "wave 1 done"})
	if err != nil {
		t.Fatalf("valid flag refused: %v", err)
	}
	if id != "epic-07" {
		t.Fatalf("board = %q, want epic-07", id)
	}
	if strings.Join(rest, " ") != "post wave 1 done" {
		t.Fatalf("remaining args = %v, want the flag removed and nothing else", rest)
	}

	// `--fleet` is the same flag under the name a fleet operator reaches for.
	if _, id, err := extractBoardFlag([]string{"open", "--fleet=epic-08"}); err != nil || id != "epic-08" {
		t.Fatalf("--fleet=epic-08 → (%q, %v), want epic-08", id, err)
	}
}

// TestBoardsAreOpenedOncePerBoard. The singleton became per board: reopening
// the same board reuses its pane (two views would halve the screen each gets),
// while a second board gets a pane of its own — that is the whole point.
func TestBoardsAreOpenedOncePerBoard(t *testing.T) {
	w, _ := newBoardIDTestWorkspace(t)
	w.boardPanes = map[string]string{}

	// A pane already renders epic-07. findPaneSnapshot resolves it through the
	// same tab responder the rest of the fixture uses.
	w.boardPanes["epic-07"] = "pB"

	var out strings.Builder
	w.openAgentsBoardPane(&out, "pA", "epic-07")
	if !strings.Contains(out.String(), "already open") {
		t.Fatalf("reopening epic-07 did not reuse its pane: %q", out.String())
	}
	if !strings.Contains(out.String(), "epic-07") {
		t.Errorf("the message does not say WHICH board is already open: %q", out.String())
	}
}
