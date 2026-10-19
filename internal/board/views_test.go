// SPDX-License-Identifier: Apache-2.0

package board

import (
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func TestViews(t *testing.T) {
	s := New(0)
	s.Apply(msg.NewBoardPost("a", "a", msg.BoardKindMilestone, "status", 1))
	c := msg.NewBoardPost("a", "a", msg.BoardKindCmd, "do it", 2)
	c.ToPaneID, c.CmdID, c.ThreadID = "b", "c1", msg.CmdThreadID("a", "c1")
	s.Apply(c)
	// A provisional prompt thread: the ack arrived before its command.
	ack := msg.NewBoardPost("b", "b", msg.BoardKindAck, "ok", 3)
	ack.CmdID, ack.ThreadID = "c2", msg.CmdThreadID("z", "c2")
	s.Apply(ack)

	ts := s.Threads()
	if st, pr := CountViews(ts); st != 1 || pr != 2 {
		t.Fatalf("CountViews = (%d, %d), want (1, 2)", st, pr)
	}
	if n := len(FilterThreads(ts, ViewStatus)); n != 1 {
		t.Errorf("status view kept %d threads, want 1", n)
	}
	if n := len(FilterThreads(ts, ViewPrompt)); n != 2 {
		t.Errorf("prompt view kept %d threads, want 2 (incl. the provisional one)", n)
	}
	if n := len(FilterThreads(ts, ViewAll)); n != 3 {
		t.Errorf("all view kept %d threads, want 3", n)
	}
	r := Answer(s, Query{})
	f := FilterReply(&r, ViewPrompt)
	if len(f.Threads) != 2 || f.Stats.Threads != 3 {
		t.Errorf("FilterReply narrows the list (%d) but must keep whole-board stats (%d)", len(f.Threads), f.Stats.Threads)
	}
	for in, want := range map[string]View{"s": ViewStatus, "Prompt": ViewPrompt, "a": ViewAll, "f": ViewFlow, "flow": ViewFlow} {
		if v, err := ParseView(in); err != nil || v != want {
			t.Errorf("ParseView(%q) = %q, %v", in, v, err)
		}
	}
	if _, err := ParseView("bogus"); err == nil {
		t.Error("ParseView accepted bogus")
	}
}
