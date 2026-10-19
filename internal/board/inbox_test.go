// SPDX-License-Identifier: Apache-2.0

package board

import (
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func cmdPost(from, to, id string, ts int64) *msg.MsgBoardPost {
	p := msg.NewBoardPost(from, "p-"+from, msg.BoardKindCmd, "do "+id, ts)
	p.ToPaneID, p.ToPersona, p.CmdID = to, "p-"+to, id
	p.ThreadID = msg.CmdThreadID(from, id)
	return p
}

func ackPost(by, from, id string, ts int64) *msg.MsgBoardPost {
	p := msg.NewBoardPost(by, "p-"+by, msg.BoardKindAck, "ack", ts)
	p.CmdID, p.ThreadID = id, msg.CmdThreadID(from, id)
	return p
}

// replyFromStore feeds posts through the REAL store and query, so the test also
// pins that a command's thread id makes it a root (not a provisional thread)
// and that its ack files as a reply under it.
func replyFromStore(t *testing.T, posts ...*msg.MsgBoardPost) *QueryReply {
	t.Helper()
	s := New(0)
	for _, p := range posts {
		s.Apply(p)
	}
	r := Answer(s, Query{})
	return &r
}

func TestCommandOpensItsOwnThreadAndAckFilesUnderIt(t *testing.T) {
	r := replyFromStore(t, cmdPost("ceo", "wkr", "c1", 100), ackPost("wkr", "ceo", "c1", 200))
	if len(r.Threads) != 1 {
		t.Fatalf("threads = %d, want 1 (the ack must file under the command)", len(r.Threads))
	}
	th := r.Threads[0]
	if th.Provisional || th.Root == nil || th.Root.CmdID != "c1" {
		t.Fatalf("the command is not the thread's root: %+v", th)
	}
	if len(th.Replies) != 1 || th.Replies[0].Kind != msg.BoardKindAck {
		t.Fatalf("replies = %+v, want the ack", th.Replies)
	}
}

func TestInboxListsOnlyMyCommandsNewestFirst(t *testing.T) {
	status := msg.NewBoardPost("ceo", "p-ceo", msg.BoardKindMilestone, "status, not work", 50)
	r := replyFromStore(t,
		status,
		cmdPost("ceo", "wkr", "c1", 100),
		cmdPost("ceo", "other", "c2", 150),
		cmdPost("mgr", "wkr", "c3", 300),
	)
	got := Inbox(r, "wkr")
	if len(got) != 2 || got[0].CmdID != "c3" || got[1].CmdID != "c1" {
		t.Fatalf("inbox = %+v, want [c3 c1]", got)
	}
	if got[0].From != "p-mgr" || got[0].FromPaneID != "mgr" || got[0].Answered {
		t.Errorf("item = %+v", got[0])
	}
}

func TestInboxAnsweredByAckOrByReply(t *testing.T) {
	reply := msg.NewBoardPost("wkr", "p-wkr", "REPORT", "done with c2", 400)
	reply.ThreadID = msg.CmdThreadID("ceo", "c2")
	strangerOnThread := msg.NewBoardPost("bystander", "p-b", "REPORT", "not wkr", 450)
	strangerOnThread.ThreadID = msg.CmdThreadID("ceo", "c3")

	r := replyFromStore(t,
		cmdPost("ceo", "wkr", "c1", 100),
		cmdPost("ceo", "wkr", "c2", 110),
		cmdPost("ceo", "wkr", "c3", 120),
		ackPost("wkr", "ceo", "c1", 300),
		reply,
		strangerOnThread,
	)
	state := map[string]bool{}
	for _, it := range Inbox(r, "wkr") {
		state[it.CmdID] = it.Answered
	}
	if !state["c1"] {
		t.Error("c1 was acked but reads open")
	}
	if !state["c2"] {
		t.Error("c2 was answered by a reply on its thread but reads open")
	}
	if state["c3"] {
		t.Error("c3 reads answered because SOMEONE ELSE posted on its thread — only the recipient answers")
	}
}

// A retried send can put the same command on the board twice (the board's
// dedup keys on the poster's clock, which a retry changes). The inbox counts it
// once: the identity is (sender, CmdID).
func TestInboxCountsARetriedCommandOnce(t *testing.T) {
	r := replyFromStore(t, cmdPost("ceo", "wkr", "c1", 100), cmdPost("ceo", "wkr", "c1", 105))
	if got := Inbox(r, "wkr"); len(got) != 1 {
		t.Fatalf("inbox = %d items, want 1", len(got))
	}
}

func TestFindCommand(t *testing.T) {
	r := replyFromStore(t, cmdPost("ceo", "wkr", "c1", 100))
	if it, ok := FindCommand(r, "wkr", "c1"); !ok || it.ThreadID != "ceo/c1" {
		t.Fatalf("FindCommand = %+v, %v", it, ok)
	}
	if _, ok := FindCommand(r, "other", "c1"); ok {
		t.Error("found a command addressed to someone else")
	}
	if Inbox(nil, "wkr") != nil || Inbox(r, "") != nil {
		t.Error("nil reply or empty pane must yield nothing")
	}
}

func TestCmdIDHelpers(t *testing.T) {
	id := msg.MintCmdID(1_700_000_000_000)
	if err := msg.ValidateCmdID(id); err != nil {
		t.Fatalf("minted id %q does not validate: %v", id, err)
	}
	if msg.MintCmdID(1) == msg.MintCmdID(1) {
		t.Error("two ids minted in the same millisecond collided")
	}
	for _, bad := range []string{"", "-x", "a b", "a/b", string(make([]byte, 65))} {
		if msg.ValidateCmdID(bad) == nil {
			t.Errorf("ValidateCmdID(%q) accepted", bad)
		}
	}
	for _, good := range []string{"msg-0042", "c1", "cmd-abc.1_x"} {
		if err := msg.ValidateCmdID(good); err != nil {
			t.Errorf("ValidateCmdID(%q) = %v", good, err)
		}
	}
}
