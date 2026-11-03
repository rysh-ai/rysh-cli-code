// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// The peer view (design 044 N1, root-order-051): P in an agents-board pane
// shows the session's connected peer board — read-only, from the daemon's
// peer feed — and the other views count what is unread there (N2).

func peerFeedFixture() *msgpkg.MsgPeerFeed {
	return &msgpkg.MsgPeerFeed{Connected: true, URL: "https://rysh.example", Board: "team", Updated: 1700000000000, Messages: []msgpkg.PeerFeedMessage{
		{Sequence: 1, ID: "root-1", ThreadID: "root-1", SessionName: "pbw-a", AgentName: "first-parakeet", Text: "release build is green", CreatedAt: 1700000000000, Mine: true},
		{Sequence: 2, ID: "re-2", ThreadID: "root-1", SessionName: "pbw-b", AgentName: "good-buffalo", Text: "thanks, rebasing docs", CreatedAt: 1700000060000, Alert: msgpkg.PeerAlertReply},
		{Sequence: 3, ID: "p-3", ThreadID: "p-3", SessionName: "pbw-b", AgentName: "good-buffalo", Text: "\x1b[31mred\x1b[0m text\nsecond line", CreatedAt: 1700000120000, Alert: msgpkg.PeerAlertMessage},
	}}
}

func TestBoardPeerViewShowsTheFeed(t *testing.T) {
	m := buildBoardModel(board.New(0))
	m.applyPeerFeed(peerFeedFixture())
	pressBoardKey(t, &m, 'P')
	if !m.boardViewFor(boardPaneID).peer {
		t.Fatal("P did not switch the pane to the peer view")
	}
	got := boardViewTextWidth(m, 200)
	if testing.Verbose() {
		fmt.Println(got)
	}
	for _, want := range []string{
		"PEER BOARD team",
		"3 messages",
		"keys s p a f t P",
		"pbw-a/first-parakeet (you)",
		"release build is green",
		"↳ pbw-b/good-buffalo [reply]",
		"thanks, rebasing docs",
		"red text",
		"second line",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("peer view missing %q:\n%s", want, got)
		}
	}
	if strings.ContainsRune(got, '\x1b') {
		t.Errorf("peer view drew a remote escape sequence:\n%q", got)
	}
	// Newest at the bottom, like every board view.
	if strings.Index(got, "release build is green") > strings.Index(got, "second line") {
		t.Errorf("peer view is not oldest-first:\n%s", got)
	}
	// Leaving: s returns to the status view.
	pressBoardKey(t, &m, 's')
	if m.boardViewFor(boardPaneID).peer {
		t.Fatal("s did not leave the peer view")
	}
}

func TestBoardPeerUnreadCountsAndClears(t *testing.T) {
	m := buildBoardModel(board.New(0))
	// The first snapshot is history: nothing is unread when the TUI starts.
	m.applyPeerFeed(peerFeedFixture())
	if n := m.peerUnread(); n != 0 {
		t.Fatalf("history counted as unread: %d", n)
	}
	next := peerFeedFixture()
	next.Messages = append(next.Messages,
		msgpkg.PeerFeedMessage{Sequence: 4, ID: "p-4", ThreadID: "p-4", SessionName: "pbw-b", AgentName: "good-buffalo", Text: "@pbw-a look", Alert: msgpkg.PeerAlertMention},
		msgpkg.PeerFeedMessage{Sequence: 5, ID: "p-5", ThreadID: "p-5", SessionName: "pbw-a", AgentName: "first-parakeet", Text: "mine", Mine: true},
	)
	m.applyPeerFeed(next)
	if n := m.peerUnread(); n != 1 {
		t.Fatalf("unread = %d, want 1 (own messages never count)", n)
	}
	if got := boardViewTextWidth(m, 200); !strings.Contains(got, "peer: 1 unread") {
		t.Errorf("status view does not show the peer unread count:\n%s", got)
	}
	// A peer alert raised on some pane (N2) is cleared by looking at the peer view.
	m.attentionState["pane-connect"] = &attentionInfo{Count: 1, Category: msgpkg.AttentionPeer}
	m.attentionState["pane-other"] = &attentionInfo{Count: 1, Category: msgpkg.AttentionPrompt}
	pressBoardKey(t, &m, 'P')
	if n := m.peerUnread(); n != 0 {
		t.Fatalf("unread after opening the peer view: %d", n)
	}
	if _, ok := m.attentionState["pane-connect"]; ok {
		t.Error("opening the peer view left the peer alert")
	}
	if _, ok := m.attentionState["pane-other"]; !ok {
		t.Error("opening the peer view cleared an unrelated needs-you")
	}
	// While the peer view is open, new messages are read as they arrive.
	more := peerFeedFixture()
	more.Messages = append(next.Messages, msgpkg.PeerFeedMessage{Sequence: 6, ID: "p-6", ThreadID: "p-6", SessionName: "pbw-b", Text: "live"})
	m.applyPeerFeed(more)
	if n := m.peerUnread(); n != 0 {
		t.Fatalf("message arriving in an open peer view counted unread: %d", n)
	}
}

func TestBoardPeerViewNotConnected(t *testing.T) {
	m := buildBoardModel(board.New(0))
	pressBoardKey(t, &m, 'P')
	got := boardViewTextWidth(m, 200)
	if !strings.Contains(got, "PEER BOARD") || !strings.Contains(got, "not connected") || !strings.Contains(got, "##peer connect") {
		t.Errorf("no-feed peer view must say how to connect:\n%s", got)
	}
	m.applyPeerFeed(&msgpkg.MsgPeerFeed{Connected: true, Board: "team", Error: "peer server unreachable; no delivery confirmed", Messages: peerFeedFixture().Messages})
	got = boardViewTextWidth(m, 200)
	if !strings.Contains(got, "peer server unreachable") || !strings.Contains(got, "release build is green") {
		t.Errorf("an outage must show the error and keep the messages:\n%s", got)
	}
}

// N2 alerts follow `[ui] notify_terminal` like a "needs you" prompt: silent
// by default, never louder without opt-in.
func TestPeerAlertFollowsNotifyTerminal(t *testing.T) {
	if got := attentionNotifyBytes("", msgpkg.AttentionPeer, "pbw-a"); got != "" {
		t.Errorf("peer alert with notify off wrote %q", got)
	}
	if got := attentionNotifyBytes("bell", msgpkg.AttentionPeer, "pbw-a"); got != "\a" {
		t.Errorf("peer alert with bell wrote %q", got)
	}
	if got := attentionNotifyBytes("osc9", msgpkg.AttentionPeer, "pbw-a"); !strings.Contains(got, "rysh: pbw-a needs you") {
		t.Errorf("peer alert with osc9 wrote %q", got)
	}
	m := buildBoardModel(board.New(0))
	if icon := m.attentionIcon(msgpkg.AttentionPeer); icon == m.attentionIcon("unknown") {
		t.Errorf("peer alerts have no icon of their own: %q", icon)
	}
}

// N3: a board the user published says so in its header, in every view.
func TestBoardHeaderShowsPublished(t *testing.T) {
	m := buildBoardModel(board.New(0))
	f := peerFeedFixture()
	m.applyPeerFeed(f)
	if got := boardViewTextWidth(m, 200); strings.Contains(got, "published to peer board") {
		t.Fatalf("an unpublished board claims to be published:\n%s", got)
	}
	f2 := peerFeedFixture()
	f2.Published = []string{msgpkg.DefaultBoardID}
	m.applyPeerFeed(f2)
	for _, key := range []rune{'s', 'f', 't'} {
		pressBoardKey(t, &m, key)
		if got := boardViewTextWidth(m, 200); !strings.Contains(got, "⇪ published to peer board team") {
			t.Errorf("view %q does not show the published marker:\n%s", key, got)
		}
	}
}
