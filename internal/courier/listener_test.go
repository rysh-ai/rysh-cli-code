// SPDX-License-Identifier: Apache-2.0

package courier

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func TestListenerForTakesOnlyItsOwnPrompts(t *testing.T) {
	forY := cmd("x", "y", "c1")
	forZ := cmd("x", "z", "c2")
	status := msg.NewBoardPost("x", "x", msg.BoardKindMilestone, "status", 1)
	cases := []struct {
		name string
		ev   board.Event
		want bool
	}{
		{"prompt for me on my board", ev("alpha", forY), true},
		{"prompt for another agent", ev("alpha", forZ), false},
		{"status post", ev("alpha", status), false},
		{"prompt for me on another board", ev("beta", forY), false},
		{"register event", board.Event{Kind: board.EventRegister, Board: "alpha"}, false},
	}
	for _, tc := range cases {
		if got := ListenerFor(tc.ev, "y", "alpha"); got != tc.want {
			t.Errorf("%s: ListenerFor = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCatchUpHandsOverTheOpenBacklogOldestFirst(t *testing.T) {
	s := board.New(0)
	old := cmd("x", "y", "c1")
	old.TS = 100
	newer := cmd("x", "y", "c2")
	newer.TS = 200
	answered := cmd("x", "y", "c3")
	answered.TS = 50
	other := cmd("x", "z", "c4")
	already := cmd("x", "y", "c5")
	already.TS = 300
	for _, p := range []*msg.MsgBoardPost{newer, old, answered, other, already} {
		s.Apply(p)
	}
	ack := msg.NewBoardPost("y", "y", msg.BoardKindAck, "ok", 400)
	ack.CmdID, ack.ThreadID = "c3", answered.ThreadID
	s.Apply(ack)

	r := board.Answer(s, board.Query{})
	delivered := map[string]bool{DeliveredKey("alpha", already): true}
	got := CatchUp(&r, "y", "alpha", delivered)
	if len(got) != 2 || got[0].CmdID != "c1" || got[1].CmdID != "c2" {
		ids := []string{}
		for _, p := range got {
			ids = append(ids, p.CmdID)
		}
		t.Fatalf("catch-up = %v, want [c1 c2]: open, mine, not yet delivered, oldest first", ids)
	}
}

func TestDeliveredLogSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.delivered")
	d := NewDeliveredLog(path)
	if len(d.Load()) != 0 {
		t.Fatal("a missing log must load empty")
	}
	k := DeliveredKey("alpha", cmd("x", "y", "c1"))
	d.Record(k)
	if !NewDeliveredLog(path).Load()[k] {
		t.Error("a recorded key must survive a new DeliveredLog on the same file")
	}
}

func TestLockPaneAdmitsOneListener(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.lock")
	f1, ok1, err := LockPane(path)
	if err != nil || !ok1 {
		t.Fatalf("first lock: ok=%v err=%v", ok1, err)
	}
	if _, ok2, _ := LockPane(path); ok2 {
		t.Fatal("a second listener for the same pane got the lock")
	}
	f1.Close()
	f3, ok3, _ := LockPane(path)
	if !ok3 {
		t.Fatal("the lock must be free again once the holder exits")
	}
	f3.Close()
	_ = os.Remove(path)
}

// The daemon courier leaves queue panes to their listener; a listener (same
// code, AsListener) delivers to them. Exactly one of the two hands a queue
// prompt over — never both, never neither.
func TestQueuePanesBelongToTheirListener(t *testing.T) {
	for _, asListener := range []bool{false, true} {
		var mu sync.Mutex
		delivered := 0
		c := New(Deps{
			AsListener: asListener,
			Snapshot: func(id string) (Target, error) {
				return Target{PaneID: id, Program: "2.1.296", PGID: 42,
					Meta: map[string]string{MetaFleetComm: CommQueue}}, nil
			},
			IsSocket: sockSet("/tmp/cc-socks/42.sock"),
			Deliver: func(context.Context, Route, Target, string) error {
				mu.Lock()
				delivered++
				mu.Unlock()
				return nil
			},
			Backoff: []time.Duration{time.Millisecond},
		})
		c.Handle(ev("alpha", cmd("x", "y", "c1")))
		if !c.Drain(2 * time.Second) {
			t.Fatal("Drain timed out")
		}
		c.Close()
		want := 0
		if asListener {
			want = 1
		}
		if delivered != want {
			t.Errorf("AsListener=%v delivered %d, want %d", asListener, delivered, want)
		}
	}
}

// grid messaging keeps working beside queue: the daemon courier still pushes
// to grid panes, pre-051 "polling-stream" panes (wayto) and unmarked panes.
func TestCourierStillDeliversGridPanes(t *testing.T) {
	for _, comm := range []string{CommGrid, CommPolling, ""} {
		var mu sync.Mutex
		delivered := 0
		meta := map[string]string{}
		if comm != "" {
			meta[MetaFleetComm] = comm
		}
		c := New(Deps{
			Snapshot: func(id string) (Target, error) {
				return Target{PaneID: id, Program: "2.1.296", PGID: 42, Meta: meta}, nil
			},
			IsSocket: sockSet("/tmp/cc-socks/42.sock"),
			Deliver: func(context.Context, Route, Target, string) error {
				mu.Lock()
				delivered++
				mu.Unlock()
				return nil
			},
			Backoff: []time.Duration{time.Millisecond},
		})
		c.Handle(ev("alpha", cmd("x", "y", "c1")))
		c.Drain(2 * time.Second)
		c.Close()
		if delivered != 1 {
			t.Errorf("fleet.comm=%q: courier delivered %d, want 1", comm, delivered)
		}
	}
}
