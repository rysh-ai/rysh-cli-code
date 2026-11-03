// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"testing"

	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// F-100 part 2: after part 1 (the blink tick), an idle attach still
// re-rendered ~74 times a minute for messages that change nothing on screen,
// keeping it at 11-15% of a core with busy panes (measured, branch
// e9-f100-idle-attach-probe). Each of these must serve the cached frame when
// nothing changed, and still render when something did.

func updated(t *testing.T, m Model, msg any) Model {
	t.Helper()
	next, _ := m.Update(msg)
	return next.(Model)
}

func TestF100IdenticalWorkspaceReplyServesTheCachedFrame(t *testing.T) {
	m := Model{lastWorkspaceHash: 42}
	if !updated(t, m, workspaceLoadedMsg{hash: 42}).viewSkip {
		t.Fatal("a byte-identical workspace reply (the idle 1.5 s reconcile) re-rendered the whole view")
	}
}

func TestF100RecorderAskTickServesTheCachedFrame(t *testing.T) {
	if !updated(t, Model{}, recorderAskTick{}).viewSkip {
		t.Fatal("a tick that only schedules the recorder question re-rendered the whole view")
	}
}

func TestF100UnchangedRecorderAnswerServesTheCachedFrame(t *testing.T) {
	m := Model{boardRecorder: RecorderLive}
	if !updated(t, m, boardRecorderMsg{state: RecorderLive}).viewSkip {
		t.Fatal("the same recorder answer re-rendered the whole view")
	}
	if updated(t, m, boardRecorderMsg{state: RecorderStale}).viewSkip {
		t.Fatal("a changed recorder answer did not render: the board would show a stale state")
	}
}

func TestF100UnchangedPeerFeedServesTheCachedFrame(t *testing.T) {
	feed := &msgpkg.MsgPeerFeed{}
	m := Model{peerFeed: &msgpkg.MsgPeerFeed{}}
	if !updated(t, m, peerFeedMsg{feed: feed}).viewSkip {
		t.Fatal("an identical peer feed re-rendered the whole view")
	}
	if updated(t, Model{}, peerFeedMsg{feed: feed}).viewSkip {
		t.Fatal("the first peer feed did not render")
	}
}

// The feed's Updated time changes on every read. It is drawn only in a board
// pane showing the peer view, so a feed that differs only by Updated renders
// when such a view is open and serves the cached frame otherwise.
func TestF100PeerFeedUpdatedTimeRendersOnlyWhenShown(t *testing.T) {
	old := &msgpkg.MsgPeerFeed{Board: "b", Updated: 1}
	next := &msgpkg.MsgPeerFeed{Board: "b", Updated: 2}
	m := Model{peerFeed: old}
	if !updated(t, m, peerFeedMsg{feed: next}).viewSkip {
		t.Fatal("a feed that differs only by its Updated time re-rendered with no peer view open")
	}
	m = Model{peerFeed: old, boardViews: map[string]*boardViewState{"b1": {peer: true}}}
	if updated(t, m, peerFeedMsg{feed: next}).viewSkip {
		t.Fatal("a peer view shows the Updated time, but the new time was not rendered")
	}
	changed := &msgpkg.MsgPeerFeed{Board: "b", Updated: 2, Connected: true}
	if updated(t, Model{peerFeed: old}, peerFeedMsg{feed: changed}).viewSkip {
		t.Fatal("a feed with a real change did not render")
	}
}
