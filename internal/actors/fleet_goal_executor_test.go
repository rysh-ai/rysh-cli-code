// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/fleet"
	"github.com/rysh-ai/rysh-cli-code/internal/fleetgoal"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// The goal executor (design 055) over a real bus: the recorder answers board
// reads, the fleet actor supervises the executor, and every assertion is on
// what crossed the wire — directives on the board, meta on the panes, the
// verdict in the registry.

const (
	gLead = "a1a1a1a1-0000-4000-8000-0000000000a1"
	gAPI  = "b2b2b2b2-0000-4000-8000-0000000000b2"
	gUI   = "c3c3c3c3-0000-4000-8000-0000000000c3"
	gE2E  = "d4d4d4d4-0000-4000-8000-0000000000d4"
)

func fastGoalExecutor(t *testing.T) {
	t.Helper()
	oldUnit, oldFirst, oldDeb := goalExecUnit, goalExecFirstRun, goalExecDebounce
	goalExecUnit, goalExecFirstRun, goalExecDebounce = 10*time.Millisecond, 1, 20
	t.Cleanup(func() { goalExecUnit, goalExecFirstRun, goalExecDebounce = oldUnit, oldFirst, oldDeb })
}

func TestGoalExecutorSerialisesTheBottleneck(t *testing.T) {
	fastGoalExecutor(t)
	nc := newABLATestNATS(t)
	codecs := msg.DefaultCodecRegistry()
	abla := NewAgentBoardListenerActor(nc, codecs, newABLATestKV(t, nc))
	abla.Start()
	t.Cleanup(abla.Stop)

	posts := record[msg.MsgBoardPost](t, nc, msg.BoardPostSubject("ship"))
	apiMeta := record[msg.MsgPaneSetMeta](t, nc, msg.T("pane", gAPI, "inbox"))

	afa := startFleetActor(t, nc, newFleetTestKV(t, nc))
	upd := func(u fleet.Update) *fleet.UpdateReply {
		t.Helper()
		r, err := fleet.Send(nc, u, fleetTestTimeout)
		if err != nil {
			t.Fatalf("%s: %v", u.Op, err)
		}
		return r
	}
	registerFleet(t, nc, fleet.Fleet{Name: "ship"})
	for _, m := range []fleet.Member{
		{PaneID: gLead, Label: "lead", Role: "lead"},
		{PaneID: gAPI, Label: "api", Role: "worker", Parent: gLead},
		{PaneID: gUI, Label: "ui", Role: "worker", Parent: gLead},
		{PaneID: gE2E, Label: "e2e", Role: "worker", Parent: gLead},
	} {
		upd(fleet.Update{Op: fleet.OpMemberUpsert, Name: "ship", Member: m})
	}
	upd(fleet.Update{Op: fleet.OpGoal, Name: "ship", Goal: fleet.GoalUpdate{Text: "ship the orders service v2", DoneWhen: "e2e green", DueSec: 4 * 3600}})
	upd(fleet.Update{Op: fleet.OpGoal, Name: "ship", Goal: fleet.GoalUpdate{Member: "api", Text: "orders API v2 endpoints"}})
	upd(fleet.Update{Op: fleet.OpGoalExec, Name: "ship", GoalExec: fleet.GoalExec{MinSec: 60, MaxSec: 120}})
	upd(fleet.Update{Op: fleet.OpStatus, PaneID: gLead, Status: fleet.StatusUpdate{Work: fleet.WorkProgress, Task: "coordinate"}})
	upd(fleet.Update{Op: fleet.OpStatus, PaneID: gAPI, Status: fleet.StatusUpdate{Work: fleet.WorkProgress, Task: "orders API v2 endpoints"}})
	upd(fleet.Update{Op: fleet.OpStatus, PaneID: gUI, Status: fleet.StatusUpdate{Work: fleet.WorkBlocked, Task: "orders UI", BlockedBy: []string{"api"}}})
	upd(fleet.Update{Op: fleet.OpStatus, PaneID: gE2E, Status: fleet.StatusUpdate{Work: fleet.WorkBlocked, Task: "e2e suite", BlockedBy: []string{"api"}}})

	if afa.GoalExecutorRunning("ship") {
		t.Fatal("an executor runs for a fleet that is not up")
	}
	upd(fleet.Update{Op: fleet.OpState, Name: "ship", State: fleet.StateUp})
	if !afa.GoalExecutorRunning("ship") {
		t.Fatal("no executor for a fleet that came up")
	}

	// 1. The warm-up cycle reports and sends nothing.
	posts.waitFor(t, "the first goal report", func(p *msg.MsgBoardPost) bool {
		return p.PaneID == fleetgoal.Identity && p.Kind == fleetgoal.KindReport && strings.Contains(p.Text, "bottleneck api (2 waiting)")
	})

	// 2. The next cycle: HURRY the bottleneck once (an ask), WAIT the two
	// waiters (told not to ask).
	hurry := posts.waitFor(t, "HURRY to api", func(p *msg.MsgBoardPost) bool {
		return p.Kind == msg.BoardKindCmd && p.ToPaneID == gAPI && strings.Contains(p.Text, "GOAL HURRY")
	})
	if !strings.Contains(hurry.Text, "2 agents wait on you: ui, e2e") || !strings.Contains(hurry.Text, "fleet status <idle|progress|blocked|done>") {
		t.Fatalf("HURRY text:\n%s", hurry.Text)
	}
	for _, w := range []string{gUI, gE2E} {
		w := w
		wait := posts.waitFor(t, "WAIT to "+w, func(p *msg.MsgBoardPost) bool {
			return p.Kind == msg.BoardKindCmd && p.ToPaneID == w && strings.Contains(p.Text, "GOAL WAIT")
		})
		if !strings.Contains(wait.Text, "Do not message them") || !strings.Contains(wait.Text, "board ack ga-") {
			t.Fatalf("WAIT text:\n%s", wait.Text)
		}
	}
	apiMeta.waitFor(t, "api's goal state mirrored", func(m *msg.MsgPaneSetMeta) bool {
		return m.Key == MetaFleetGoalState && m.Value == fleetgoal.CondBlocker
	})

	// 3. api answers with an ETA: the answer lands on the HURRY's thread, and
	// the waiters are told the ETA by the executor.
	r := upd(fleet.Update{Op: fleet.OpStatus, PaneID: gAPI, Status: fleet.StatusUpdate{Work: fleet.WorkProgress, EtaSec: 1200, Pct: intPtr(40), KeepTask: true}})
	if r.AnsweredGoalAsk != hurry.CmdID {
		t.Fatalf("api's declaration answered %q, want the HURRY %q", r.AnsweredGoalAsk, hurry.CmdID)
	}
	posts.waitFor(t, "api's answer on the HURRY's thread", func(p *msg.MsgBoardPost) bool {
		return p.PaneID == gAPI && p.ThreadID == msg.CmdThreadID(fleetgoal.Identity, hurry.CmdID) && strings.Contains(p.Text, "progress 40%")
	})
	posts.waitFor(t, "a WAIT carrying api's ETA", func(p *msg.MsgBoardPost) bool {
		return p.Kind == msg.BoardKindCmd && p.ToPaneID == gE2E && strings.Contains(p.Text, "GOAL WAIT") && strings.Contains(p.Text, "ETA ")
	})
	apiMeta.waitFor(t, "api's pct mirrored", func(m *msg.MsgPaneSetMeta) bool {
		return m.Key == MetaFleetGoalPct && m.Value == "40"
	})

	// 4. The verdict is readable from any process, with the fleet.
	reply, err := fleet.Ask(nc, fleet.Query{Name: "ship"}, fleetTestTimeout)
	if err != nil || len(reply.Fleets) != 1 {
		t.Fatalf("ask: %v %+v", err, reply)
	}
	gs := reply.Fleets[0].GoalStatus
	if gs == nil || gs.Bottleneck != gAPI || gs.Members[gAPI] != fleetgoal.CondBlocker || gs.Pct != 40 {
		t.Fatalf("goal status = %+v", gs)
	}

	// 5. Down: the executor stops.
	upd(fleet.Update{Op: fleet.OpState, Name: "ship", State: fleet.StateDown})
	if afa.GoalExecutorRunning("ship") {
		t.Fatal("the executor outlived its fleet")
	}
}

func TestGoalExecutorOffMeansNoExecutor(t *testing.T) {
	fastGoalExecutor(t)
	nc := newABLATestNATS(t)
	afa := startFleetActor(t, nc, newFleetTestKV(t, nc))
	registerFleet(t, nc, fleet.Fleet{Name: "quiet"})
	if _, err := fleet.Send(nc, fleet.Update{Op: fleet.OpGoalExec, Name: "quiet", GoalExec: fleet.GoalExec{Mode: fleet.GoalExecOff}}, fleetTestTimeout); err != nil {
		t.Fatal(err)
	}
	if _, err := fleet.Send(nc, fleet.Update{Op: fleet.OpState, Name: "quiet", State: fleet.StateUp}, fleetTestTimeout); err != nil {
		t.Fatal(err)
	}
	if afa.GoalExecutorRunning("quiet") {
		t.Fatal("an executor runs for a fleet that switched it off")
	}
	if _, err := fleet.Send(nc, fleet.Update{Op: fleet.OpGoalExec, Name: "quiet", GoalExec: fleet.GoalExec{Mode: fleet.GoalExecAdaptive}}, fleetTestTimeout); err != nil {
		t.Fatal(err)
	}
	if !afa.GoalExecutorRunning("quiet") {
		t.Fatal("switching it back on started nothing")
	}
}

func intPtr(n int) *int { return &n }
