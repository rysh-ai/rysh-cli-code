// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Design 050 (polling-stream): `board send` puts a COMMAND on the board, and the
// courier delivers it into the recipient's own session. These tests pin the
// door. The headline is the first one: a command is a board message and
// nothing else — no keystroke, no pane output, no focus change — because the
// pane's composer belongs to the human, and typing into it is exactly the
// defect polling-stream exists to remove.

func sendCmd(t *testing.T, w *WorkspaceActor, m *msg.MsgCLIBoardPost) *msg.MsgCLIResponse {
	t.Helper()
	if m.Kind == "" {
		m.Kind = msg.BoardKindCmd
	}
	return w.handleCLIBoardPost(m)
}

func TestBoardCmdTouchesNoPaneAndIsAddressed(t *testing.T) {
	w, nc := newBoardTestWorkspace(t)
	paneTraffic := recordSubject(t, nc, msg.T("pane", ">"))
	boardTraffic := recordSubject(t, nc, msg.BoardPostSubject(""))

	// The human is in pA; the CEO in pA sends wkr-01 (pB) work BY NAME.
	resp := sendCmd(t, w, &msg.MsgCLIBoardPost{AsPaneID: "pA", To: "wkr-01", Text: "build the parser"})
	if resp == nil || !resp.OK {
		t.Fatalf("command refused: %+v", resp)
	}
	settle(t, nc)

	if subjects, _ := paneTraffic.seen(); len(subjects) != 0 {
		t.Fatalf("a board command published %d message(s) on pane subjects: %v\n"+
			"polling-stream must never reach a pane's input or output (design 050 §1)", len(subjects), subjects)
	}
	if w.activePaneID != "pA" || w.activeTabIdx != 0 {
		t.Errorf("focus moved to pane %q tab %d — a command must not focus its recipient", w.activePaneID, w.activeTabIdx)
	}

	subjects, payloads := boardTraffic.seen()
	if len(subjects) != 1 {
		t.Fatalf("board saw %d posts, want 1", len(subjects))
	}
	var got msg.MsgBoardPost
	if err := json.Unmarshal(payloads[0], &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Kind != msg.BoardKindCmd {
		t.Errorf("kind = %q, want cmd", got.Kind)
	}
	if got.ToPaneID != "pB" {
		t.Errorf("ToPaneID = %q, want pB — the name must be resolved to the pane uuid the courier delivers by", got.ToPaneID)
	}
	if got.ToPersona != "wkr-01" {
		t.Errorf("ToPersona = %q, want wkr-01", got.ToPersona)
	}
	if got.CmdID == "" || msg.ValidateCmdID(got.CmdID) != nil {
		t.Errorf("CmdID = %q, want a minted, valid id", got.CmdID)
	}
	if want := msg.CmdThreadID("pA", got.CmdID); got.ThreadID != want {
		t.Errorf("ThreadID = %q, want %q — a command opens its own thread so its ack lands under it", got.ThreadID, want)
	}
	if resp.ID != got.ThreadID {
		t.Errorf("response ID = %q, want the thread %q", resp.ID, got.ThreadID)
	}
	if !strings.Contains(resp.Output, got.CmdID) || !strings.Contains(resp.Output, "wkr-01") {
		t.Errorf("confirmation %q should name the command id and the recipient", resp.Output)
	}
}

func TestBoardCmdKeepsCallerIDAndThread(t *testing.T) {
	w, nc := newBoardTestWorkspace(t)
	boardTraffic := recordSubject(t, nc, msg.BoardPostSubject(""))

	resp := sendCmd(t, w, &msg.MsgCLIBoardPost{
		AsPaneID: "pA", To: "pB", CmdID: "msg-0042", ThreadID: "pA/7",
		Text: "see file", File: "/tmp/order.md",
	})
	if resp == nil || !resp.OK {
		t.Fatalf("command refused: %+v", resp)
	}
	settle(t, nc)
	_, payloads := boardTraffic.seen()
	if len(payloads) != 1 {
		t.Fatalf("want 1 post, got %d", len(payloads))
	}
	var got msg.MsgBoardPost
	_ = json.Unmarshal(payloads[0], &got)
	if got.CmdID != "msg-0042" || got.ThreadID != "pA/7" || got.File != "/tmp/order.md" {
		t.Errorf("caller's id/thread/file not kept: id=%q thread=%q file=%q", got.CmdID, got.ThreadID, got.File)
	}
}

// Every refusal must publish NOTHING: a half-addressed order on the board is
// worse than none, because the board would show work that nobody will get.
func TestBoardCmdRefusals(t *testing.T) {
	cases := []struct {
		name string
		m    msg.MsgCLIBoardPost
		want string
	}{
		{"no recipient", msg.MsgCLIBoardPost{AsPaneID: "pA", Kind: msg.BoardKindCmd, Text: "x"}, "recipient"},
		{"unknown recipient", msg.MsgCLIBoardPost{AsPaneID: "pA", Kind: msg.BoardKindCmd, To: "ghost", Text: "x"}, "not found"},
		{"to self", msg.MsgCLIBoardPost{AsPaneID: "pB", Kind: msg.BoardKindCmd, To: "wkr-01", Text: "x"}, "own sender"},
		{"bad id", msg.MsgCLIBoardPost{AsPaneID: "pA", Kind: msg.BoardKindCmd, To: "pB", CmdID: "a b", Text: "x"}, "invalid command id"},
		{"relative file", msg.MsgCLIBoardPost{AsPaneID: "pA", Kind: msg.BoardKindCmd, To: "pB", File: "order.md", Text: "x"}, "absolute"},
		{"status with --to", msg.MsgCLIBoardPost{AsPaneID: "pA", Kind: msg.BoardKindMilestone, To: "pB", Text: "x"}, "commands only"},
		{"status with id", msg.MsgCLIBoardPost{AsPaneID: "pA", Kind: msg.BoardKindReply, CmdID: "c1", ThreadID: "pA/1", Text: "x"}, "commands only"},
		{"ack without id", msg.MsgCLIBoardPost{AsPaneID: "pB", Kind: msg.BoardKindAck, ThreadID: "pA/c1", Text: "ok"}, "name the command"},
		{"ack without thread", msg.MsgCLIBoardPost{AsPaneID: "pB", Kind: msg.BoardKindAck, CmdID: "c1", Text: "ok"}, "thread"},
		{"ack with --to", msg.MsgCLIBoardPost{AsPaneID: "pB", Kind: msg.BoardKindAck, CmdID: "c1", ThreadID: "pA/c1", To: "pA", Text: "ok"}, "no --to"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, nc := newBoardTestWorkspace(t)
			boardTraffic := recordSubject(t, nc, msg.BoardPostSubject(""))
			m := tc.m
			resp := w.handleCLIBoardPost(&m)
			if resp == nil || resp.OK {
				t.Fatalf("accepted, want refusal containing %q: %+v", tc.want, resp)
			}
			if !strings.Contains(resp.Error, tc.want) {
				t.Errorf("error %q does not contain %q", resp.Error, tc.want)
			}
			settle(t, nc)
			if subjects, _ := boardTraffic.seen(); len(subjects) != 0 {
				t.Errorf("a refused request still published %d post(s)", len(subjects))
			}
		})
	}
}

func TestBoardAckIsFiledOnTheCommandThread(t *testing.T) {
	w, nc := newBoardTestWorkspace(t)
	boardTraffic := recordSubject(t, nc, msg.BoardPostSubject(""))

	resp := w.handleCLIBoardPost(&msg.MsgCLIBoardPost{
		AsPaneID: "pB", Kind: msg.BoardKindAck, CmdID: "msg-0042",
		ThreadID: msg.CmdThreadID("pA", "msg-0042"), Text: "on it",
	})
	if resp == nil || !resp.OK {
		t.Fatalf("ack refused: %+v", resp)
	}
	settle(t, nc)
	_, payloads := boardTraffic.seen()
	if len(payloads) != 1 {
		t.Fatalf("want 1 post, got %d", len(payloads))
	}
	var got msg.MsgBoardPost
	_ = json.Unmarshal(payloads[0], &got)
	if got.Kind != msg.BoardKindAck || got.CmdID != "msg-0042" || got.ThreadID != "pA/msg-0042" || got.PaneID != "pB" {
		t.Errorf("ack = %+v", got)
	}
}

func TestBoardResolveNamesPaneAndBoard(t *testing.T) {
	nc := startInProcessNATS(t)
	codecs := msg.DefaultCodecRegistry()
	serveBoardTab(t, nc, codecs, "tab-A",
		boardTestPane{id: "pA-full-uuid", title: "pA", givenName: "ceo"},
		boardTestPane{id: "pB-full-uuid", title: "pB", givenName: "wkr-01", meta: map[string]string{paneMetaBoardID: "alpha"}},
	)
	w := &WorkspaceActor{
		pub:          msg.NewNATSPublisher(nc, codecs),
		tabs:         []*tabInfo{{id: "tab-A", title: "A"}},
		activePaneID: "pA-full-uuid",
	}

	resp := w.handleCLIBoardResolve(&msg.MsgCLIBoardResolve{AsPaneID: "wkr-01"})
	if resp == nil || !resp.OK {
		t.Fatalf("resolve refused: %+v", resp)
	}
	if resp.ID != "pB-full-uuid" || resp.Output != "alpha" {
		t.Errorf("resolve = (%q, %q), want (pB-full-uuid, alpha)", resp.ID, resp.Output)
	}
	if resp := w.handleCLIBoardResolve(&msg.MsgCLIBoardResolve{AsPaneID: "ceo"}); resp.Output != msg.DefaultBoardID {
		t.Errorf("a pane with no board.id resolves to %q, want the session board", resp.Output)
	}
	if resp := w.handleCLIBoardResolve(&msg.MsgCLIBoardResolve{}); resp.OK {
		t.Error("an empty --as must be refused, never the active pane")
	}
	if w.activePaneID != "pA-full-uuid" {
		t.Errorf("resolve moved focus to %q", w.activePaneID)
	}
}

// Design 051: a daemon reports its board capabilities only when asked, so the
// first line of a plain resolve stays the bare board id older clients read.
func TestBoardResolveCapsAreOptIn(t *testing.T) {
	w, _ := newBoardTestWorkspace(t)
	plain := w.handleCLIBoardResolve(&msg.MsgCLIBoardResolve{AsPaneID: "pB"})
	if plain.Output != msg.DefaultBoardID {
		t.Errorf("plain resolve Output = %q, want the bare board id", plain.Output)
	}
	withCaps := w.handleCLIBoardResolve(&msg.MsgCLIBoardResolve{AsPaneID: "pB", WantCaps: true})
	if withCaps.Output != msg.DefaultBoardID+"\ncaps: queue" {
		t.Errorf("caps resolve Output = %q", withCaps.Output)
	}
}
