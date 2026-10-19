// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/fleet"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// The fleet watch (design 054) over a real bus: panes answer VT reads, the
// board is a subscription, and every assertion is on what crossed the wire.

const (
	wMgr = "eeeeeeee-0000-4000-8000-00000000000a"
	wW1  = "ffffffff-0000-4000-8000-00000000000b"
)

// servePaneVT answers a pane's VT read with whatever screen() returns.
func servePaneVT(t *testing.T, nc *nats.Conn, codecs *msg.CodecRegistry, paneID string, screen func() []string) {
	t.Helper()
	sub, err := nc.Subscribe(msg.T("pane", paneID, "snapshot"), func(m *nats.Msg) {
		var in msg.NATSEnvelope
		if err := json.Unmarshal(m.Data, &in); err != nil || in.ReplyTo == "" {
			return
		}
		reply := &msg.MsgPaneVTReply{Screen: screen()}
		payload, _ := json.Marshal(reply)
		data, _ := json.Marshal(msg.NATSEnvelope{TypeTag: codecs.TagOf(reply), Payload: payload})
		_ = nc.Publish(in.ReplyTo, data)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
}

// recorder collects decoded messages of one type from a subject.
type recorder[T any] struct {
	mu  sync.Mutex
	got []*T
}

func record[T any](t *testing.T, nc *nats.Conn, subject string) *recorder[T] {
	t.Helper()
	r := &recorder[T]{}
	sub, err := nc.Subscribe(subject, func(m *nats.Msg) {
		var env msg.NATSEnvelope
		if json.Unmarshal(m.Data, &env) != nil {
			return
		}
		v := new(T)
		if json.Unmarshal(env.Payload, v) != nil {
			return
		}
		r.mu.Lock()
		r.got = append(r.got, v)
		r.mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	return r
}

func (r *recorder[T]) waitFor(t *testing.T, what string, pred func(*T) bool) *T {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		for _, v := range r.got {
			if pred(v) {
				r.mu.Unlock()
				return v
			}
		}
		r.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
	return nil
}

func declareOverBus(t *testing.T, nc *nats.Conn, pane string, u fleet.StatusUpdate) *fleet.UpdateReply {
	t.Helper()
	reply, err := fleet.Send(nc, fleet.Update{Op: fleet.OpStatus, PaneID: pane, Status: u}, fleetTestTimeout)
	if err != nil {
		t.Fatalf("declare %+v: %v", u, err)
	}
	return reply
}

func TestFleetWatchStallCheckinAndUnblock(t *testing.T) {
	oldTick, oldGap := afaWatchInterval, afaObserveGap
	afaWatchInterval, afaObserveGap = 150*time.Millisecond, 30*time.Millisecond
	t.Cleanup(func() { afaWatchInterval, afaObserveGap = oldTick, oldGap })

	nc := newABLATestNATS(t)
	codecs := msg.DefaultCodecRegistry()
	// Both panes sit still: an idle claude repaints nothing.
	idle := func() []string { return []string{"> ", "  ⏵⏵ bypass permissions on"} }
	servePaneVT(t, nc, codecs, wMgr, idle)
	servePaneVT(t, nc, codecs, wW1, idle)
	posts := record[msg.MsgBoardPost](t, nc, msg.BoardPostSubject("wf"))
	meta := record[msg.MsgPaneSetMeta](t, nc, msg.T("pane", wW1, "inbox"))

	startFleetActor(t, nc, newFleetTestKV(t, nc))
	registerFleet(t, nc, fleet.Fleet{Name: "wf"})
	if _, err := fleet.Send(nc, fleet.Update{Op: fleet.OpState, Name: "wf", State: fleet.StateUp}, fleetTestTimeout); err != nil {
		t.Fatal(err)
	}
	for _, m := range []fleet.Member{
		{PaneID: wMgr, Label: "mgr-01", Role: "manager"},
		{PaneID: wW1, Label: "wkr-01", Role: "worker", Parent: wMgr},
	} {
		if _, err := fleet.Send(nc, fleet.Update{Op: fleet.OpMemberUpsert, Name: "wf", Member: m}, fleetTestTimeout); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fleet.Send(nc, fleet.Update{Op: fleet.OpWatch, Name: "wf",
		Watch: fleet.Watch{IntervalSec: 600, StallSec: 1}}, fleetTestTimeout); err != nil {
		t.Fatal(err)
	}

	// 1. A declaration is posted on the board AS the member.
	declareOverBus(t, nc, wW1, fleet.StatusUpdate{Work: fleet.WorkProgress, Task: "build the parser"})
	posts.waitFor(t, "the progress post", func(p *msg.MsgBoardPost) bool {
		return p.PaneID == wW1 && p.Kind == "progress" && strings.Contains(p.Text, "build the parser")
	})

	// 2. Idle on screen while declaring progress: stalled, alerted, and
	// checked in with by a board cmd addressed to the member.
	posts.waitFor(t, "the stalled alert", func(p *msg.MsgBoardPost) bool {
		return p.PaneID == FleetIdentity && p.Kind == fleetAlertKind && strings.Contains(p.Text, "STALLED")
	})
	ci := posts.waitFor(t, "a check-in cmd", func(p *msg.MsgBoardPost) bool {
		return p.PaneID == FleetIdentity && p.Kind == msg.BoardKindCmd && p.ToPaneID == wW1
	})
	if !strings.HasPrefix(ci.CmdID, "ci-") || ci.ThreadID != msg.CmdThreadID(FleetIdentity, ci.CmdID) {
		t.Fatalf("check-in cmd=%q thread=%q", ci.CmdID, ci.ThreadID)
	}
	for _, want := range []string{"CHECK-IN", "stalled", "fleet status <idle|progress|blocked|done> --as " + wW1, "--fleet wf"} {
		if !strings.Contains(ci.Text, want) {
			t.Errorf("check-in text missing %q:\n%s", want, ci.Text)
		}
	}

	// 3. Answering: the status lands on the check-in's thread, so the board
	// shows the check-in as answered; the ledger is mirrored onto pane meta.
	r := declareOverBus(t, nc, wW1, fleet.StatusUpdate{Work: fleet.WorkBlocked, BlockedBy: []string{"mgr-01"}, KeepTask: true})
	if r.AnsweredCheckin != ci.CmdID {
		t.Fatalf("answered %q, want %q", r.AnsweredCheckin, ci.CmdID)
	}
	posts.waitFor(t, "the blocked post on the check-in thread", func(p *msg.MsgBoardPost) bool {
		return p.PaneID == wW1 && p.Kind == msg.BoardKindBlocked && p.ThreadID == ci.ThreadID &&
			strings.Contains(p.Text, "blocked by mgr-01")
	})
	meta.waitFor(t, "fleet.work=blocked on the pane", func(m *msg.MsgPaneSetMeta) bool {
		return m.Key == MetaFleetWork && m.Value == fleet.WorkBlocked
	})
	meta.waitFor(t, "fleet.blocked_by on the pane", func(m *msg.MsgPaneSetMeta) bool {
		return m.Key == MetaFleetBlockedBy && m.Value == wMgr
	})

	// 4. The blocker finishes: the blocked member is told at once.
	declareOverBus(t, nc, wMgr, fleet.StatusUpdate{Work: fleet.WorkDone, Task: "spec"})
	posts.waitFor(t, "an unblocked check-in", func(p *msg.MsgBoardPost) bool {
		return p.Kind == msg.BoardKindCmd && p.ToPaneID == wW1 && strings.Contains(p.Text, "(unblocked)") &&
			strings.Contains(p.Text, "mgr-01")
	})

	// 5. The ledger is readable over the bus, blocker_for derived.
	reply, err := fleet.Ask(nc, fleet.Query{Name: "wf"}, fleetTestTimeout)
	if err != nil {
		t.Fatal(err)
	}
	var mgr fleet.Member
	for _, m := range reply.Fleets[0].Members {
		if m.PaneID == wMgr {
			mgr = m
		}
	}
	if mgr.Status == nil || len(mgr.Status.BlockerFor) != 1 || mgr.Status.BlockerFor[0] != wW1 {
		t.Fatalf("mgr blocker_for = %+v, want [w1]", mgr.Status)
	}
}

func TestFleetWatchEscalatesAvailableToParent(t *testing.T) {
	oldTick, oldGap := afaWatchInterval, afaObserveGap
	afaWatchInterval, afaObserveGap = 150*time.Millisecond, 30*time.Millisecond
	t.Cleanup(func() { afaWatchInterval, afaObserveGap = oldTick, oldGap })

	nc := newABLATestNATS(t)
	codecs := msg.DefaultCodecRegistry()
	// The manager is busy (its screen changes every read); the worker is still.
	var n int
	var mu sync.Mutex
	servePaneVT(t, nc, codecs, wMgr, func() []string {
		mu.Lock()
		defer mu.Unlock()
		n++
		return []string{"✻ Thinking… " + strings.Repeat(".", n%5)}
	})
	servePaneVT(t, nc, codecs, wW1, func() []string { return []string{"> "} })
	posts := record[msg.MsgBoardPost](t, nc, msg.BoardPostSubject("wf2"))

	startFleetActor(t, nc, newFleetTestKV(t, nc))
	registerFleet(t, nc, fleet.Fleet{Name: "wf2"})
	_, _ = fleet.Send(nc, fleet.Update{Op: fleet.OpState, Name: "wf2", State: fleet.StateUp}, fleetTestTimeout)
	for _, m := range []fleet.Member{
		{PaneID: wMgr, Label: "mgr-01", Role: "manager"},
		{PaneID: wW1, Label: "wkr-01", Role: "worker", Parent: wMgr},
	} {
		_, _ = fleet.Send(nc, fleet.Update{Op: fleet.OpMemberUpsert, Name: "wf2", Member: m}, fleetTestTimeout)
	}
	declareOverBus(t, nc, wW1, fleet.StatusUpdate{Work: fleet.WorkDone, Task: "lexer"})

	esc := posts.waitFor(t, "an escalation to the parent", func(p *msg.MsgBoardPost) bool {
		return p.Kind == msg.BoardKindCmd && p.ToPaneID == wMgr && strings.Contains(p.Text, "ESCALATION")
	})
	if !strings.Contains(esc.Text, "wkr-01 is AVAILABLE: it declared done (lexer) and its pane is idle") {
		t.Fatalf("escalation text:\n%s", esc.Text)
	}
	// The busy manager is never pinged for itself under the smart policy.
	time.Sleep(500 * time.Millisecond)
	posts.mu.Lock()
	defer posts.mu.Unlock()
	for _, p := range posts.got {
		if p.Kind == msg.BoardKindCmd && p.ToPaneID == wMgr && strings.Contains(p.Text, "CHECK-IN") {
			t.Fatalf("a working manager was checked in with:\n%s", p.Text)
		}
	}
}
