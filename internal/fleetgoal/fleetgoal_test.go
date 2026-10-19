// SPDX-License-Identifier: Apache-2.0

package fleetgoal

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
	"github.com/rysh-ai/rysh-cli-code/internal/fleet"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// The goal executor's judgement and plan (design 055), one behaviour per test.
// Every input is constructed, every clock is a number: nothing here can be
// flaky, which is what makes the executor's interruptions defensible.

const (
	pLead = "aaaaaaaa-0000-4000-8000-000000000001"
	pAPI  = "bbbbbbbb-0000-4000-8000-000000000002"
	pUI   = "cccccccc-0000-4000-8000-000000000003"
	pE2E  = "dddddddd-0000-4000-8000-000000000004"
	pDocs = "eeeeeeee-0000-4000-8000-000000000005"
)

const minute = int64(60_000)

var t0 = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC).UnixMilli()

func cfg() fleet.GoalExec { return (&fleet.GoalExec{}).Resolved() } // adaptive, 5m, 30m

func st(work, task, activity, health string, blockedBy ...string) *fleet.MemberStatus {
	return &fleet.MemberStatus{Work: work, Task: task, Activity: activity, Health: health,
		BlockedBy: blockedBy, UpdatedAt: t0 - minute, ObservedAt: t0, JoinedAt: t0 - 60*minute, WorkSince: t0 - 10*minute}
}

// shipFleet: lead heads api, ui, e2e, docs. ui and e2e wait on api; docs
// waits on ui. api is the bottleneck: 3 wait on it.
func shipFleet() fleet.Fleet {
	return fleet.Fleet{
		Name: "ship", BoardID: "ship", State: fleet.StateUp,
		Goal: &fleet.Goal{Text: "ship the orders service v2", DoneWhen: "e2e green on staging", State: fleet.GoalOpen,
			SetAt: t0 - 120*minute, DueAt: t0 + 240*minute},
		Members: []fleet.Member{
			{PaneID: pLead, Label: "lead", Role: "lead", Status: st(fleet.WorkProgress, "coordinate the release", fleet.ActivityWorking, fleet.HealthOK)},
			{PaneID: pAPI, Label: "api", Role: "worker", Parent: pLead, Goal: &fleet.Goal{Text: "orders API v2 endpoints", State: fleet.GoalOpen},
				Status: st(fleet.WorkProgress, "orders API v2 endpoints", fleet.ActivityWorking, fleet.HealthOK)},
			{PaneID: pUI, Label: "ui", Role: "worker", Parent: pLead, Goal: &fleet.Goal{Text: "orders UI on v2", State: fleet.GoalOpen},
				Status: st(fleet.WorkBlocked, "orders UI on v2", fleet.ActivityIdle, fleet.HealthOK, pAPI)},
			{PaneID: pE2E, Label: "e2e", Role: "worker", Parent: pLead, Status: st(fleet.WorkBlocked, "e2e suite", fleet.ActivityIdle, fleet.HealthOK, pAPI)},
			{PaneID: pDocs, Label: "docs", Role: "worker", Parent: pLead, Status: st(fleet.WorkBlocked, "release notes", fleet.ActivityIdle, fleet.HealthOK, pUI)},
		},
	}
}

func input(f fleet.Fleet, now int64, threads ...board.Thread) Input {
	return Input{Fleet: f, Threads: threads, BoardOK: true, Now: now, Config: cfg()}
}

// cycle runs one executor cycle: observe, assess, plan.
func cycle(mem *Memory, in Input) (Assessment, Plan) {
	Observe(mem, in)
	a := Assess(in, mem)
	return a, MakePlan(a, in, mem)
}

// warm runs the warm-up cycle the executor always runs first.
func warm(t *testing.T, mem *Memory, in Input) {
	t.Helper()
	_, p := cycle(mem, in)
	if len(p.Actions) != 0 {
		t.Fatalf("the warm-up cycle sent %d directives; it must only observe", len(p.Actions))
	}
	if p.Report == "" {
		t.Fatal("the warm-up cycle posted no report")
	}
}

func sent(p Plan, kind, to string) *Action {
	for i := range p.Actions {
		if p.Actions[i].Kind == kind && p.Actions[i].To == to {
			return &p.Actions[i]
		}
	}
	return nil
}

func recipients(p Plan) map[string]int {
	out := map[string]int{}
	for _, a := range p.Actions {
		out[a.To]++
	}
	return out
}

func TestTheAssessmentReadsTheSameTreeTheViewsDraw(t *testing.T) {
	f := shipFleet()
	in := input(f, t0)
	mem := &Memory{}
	Observe(mem, in)
	a := Assess(in, mem)
	if a.Bottleneck != pAPI || a.BottleneckWaiting != 3 {
		t.Fatalf("bottleneck = %s (%d waiting), want api with 3", a.Bottleneck, a.BottleneckWaiting)
	}
	// The executor's own waiting counts must equal the dependency tree's.
	deps := board.BuildFlowDeps(board.BuildFlow(f.BoardID, nil, FlowPanes(&f)))
	var walk func(n board.FlowDepNode)
	walk = func(n board.FlowDepNode) {
		if m := a.Member(n.ID); m != nil && m.Waiting != n.Waiting {
			t.Errorf("%s: executor counts %d waiting, the tree draws %d", m.Label, m.Waiting, n.Waiting)
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	for _, r := range deps.Roots {
		walk(r)
	}
	if got := a.Member(pAPI).Primary; got != CondBlocker {
		t.Errorf("api primary = %q, want blocker", got)
	}
	if got := a.Member(pUI).Primary; got != CondBlocker {
		t.Errorf("ui primary = %q, want blocker (docs waits on it)", got)
	}
	if got := a.Member(pE2E).Primary; got != CondBlocked {
		t.Errorf("e2e primary = %q, want blocked", got)
	}
	if !strings.Contains(strings.Join(a.Deps, "\n"), "bottleneck: api (3 waiting)") {
		t.Errorf("rendered tree does not name the bottleneck:\n%s", strings.Join(a.Deps, "\n"))
	}
}

// The core of the design: the blocker is asked ONCE, the waiters are told by
// the executor and told not to ask. N questions to the bottleneck become one.
func TestHubAndSpokeHurryTheBlockerTellTheWaiters(t *testing.T) {
	mem := &Memory{}
	warm(t, mem, input(shipFleet(), t0))
	_, p := cycle(mem, input(shipFleet(), t0+5*minute))

	h := sent(p, KindHurry, pAPI)
	if h == nil || !h.Ask {
		t.Fatalf("no HURRY ask to the bottleneck api: %+v", p.Actions)
	}
	for _, want := range []string{"3 agents wait on you", "ui", "e2e", "--eta", "orders API v2 endpoints"} {
		if !strings.Contains(h.Text, want) {
			t.Errorf("HURRY lacks %q:\n%s", want, h.Text)
		}
	}
	for _, w := range []string{pUI, pE2E, pDocs} {
		wa := sent(p, KindWait, w)
		if wa == nil {
			t.Fatalf("no WAIT to %s", w)
		}
		if wa.Ask || !strings.Contains(wa.Text, "Do not message them") {
			t.Errorf("WAIT to %s must be informational and say not to ask:\n%s", w, wa.Text)
		}
	}
	if !strings.Contains(sent(p, KindWait, pE2E).Text, "2 others also waiting") {
		t.Errorf("e2e's WAIT does not say who else waits:\n%s", sent(p, KindWait, pE2E).Text)
	}
	// ui is a blocker (docs waits on it) but blocked itself: it cannot finish
	// first, so it is not hurried, and docs is told the chain.
	if sent(p, KindHurry, pUI) != nil {
		t.Errorf("a blocked blocker was hurried: %+v", p.Actions)
	}
	if d := sent(p, KindWait, pDocs); !strings.Contains(d.Text, "itself waiting on api") {
		t.Errorf("docs is not told whom ui waits on:\n%s", d.Text)
	}
	for to, n := range recipients(p) {
		if n > 1 {
			t.Errorf("%s got %d directives in one cycle; the budget is one", to, n)
		}
	}
	if sent(p, KindHurry, pLead) != nil || recipients(p)[pLead] != 0 {
		t.Errorf("the lead is progressing and on track: it must not be disturbed: %+v", p.Actions)
	}
}

func TestSaidOnceThenTheLadder(t *testing.T) {
	mem := &Memory{}
	warm(t, mem, input(shipFleet(), t0))
	_, p1 := cycle(mem, input(shipFleet(), t0+5*minute))
	if sent(p1, KindHurry, pAPI) == nil {
		t.Fatal("no HURRY at onset")
	}
	// Next cycle, nothing changed: silence.
	_, p2 := cycle(mem, input(shipFleet(), t0+10*minute))
	if len(p2.Actions) != 0 {
		t.Fatalf("an unchanged situation was re-sent: %+v", p2.Actions)
	}
	// Two min-cadences after the send: level 2, the parent copied.
	_, p3 := cycle(mem, input(shipFleet(), t0+15*minute))
	h := sent(p3, KindHurry, pAPI)
	if h == nil || h.Level != 2 {
		t.Fatalf("no level-2 HURRY: %+v", p3.Actions)
	}
	if c := sent(p3, KindHurry, pLead); c == nil || !strings.Contains(c.Text, "escalation level 2: about api") {
		t.Fatalf("level 2 did not copy api's parent: %+v", p3.Actions)
	}
	// Level 3: the head and a board alert for the human.
	_, p4 := cycle(mem, input(shipFleet(), t0+25*minute))
	if h := sent(p4, KindHurry, pAPI); h == nil || h.Level != 3 {
		t.Fatalf("no level-3 HURRY: %+v", p4.Actions)
	}
	if len(p4.Alerts) == 0 || !strings.Contains(strings.Join(p4.Alerts, "\n"), "ESCALATED (HURRY, level 3)") {
		t.Fatalf("level 3 raised no board alert: %v", p4.Alerts)
	}
	// And it stops there.
	_, p5 := cycle(mem, input(shipFleet(), t0+40*minute))
	if sent(p5, KindHurry, pAPI) != nil {
		t.Fatalf("the ladder climbed past its top: %+v", p5.Actions)
	}
}

func TestABlockerWithACurrentETAIsLeftAloneAndTheWaitersAreToldIt(t *testing.T) {
	f := shipFleet()
	// Only e2e waits on api here: one waiter, so an overdue api is late, not
	// stressed (3 waiting + overdue would be stressed and SHIELDed instead).
	f.Members[2].Status = st(fleet.WorkProgress, "orders UI on v2", fleet.ActivityWorking, fleet.HealthOK)
	f.Members[4].Status = st(fleet.WorkProgress, "release notes", fleet.ActivityWorking, fleet.HealthOK)
	f.Members[1].Status.EtaAt, f.Members[1].Status.EtaSetAt = t0+40*minute, t0
	mem := &Memory{}
	warm(t, mem, input(f, t0))
	_, p := cycle(mem, input(f, t0+5*minute))
	if sent(p, KindHurry, pAPI) != nil {
		t.Fatalf("a blocker with a current ETA was hurried: %+v", p.Actions)
	}
	w := sent(p, KindWait, pE2E)
	if w == nil || !strings.Contains(w.Text, "ETA "+clock(t0+40*minute)) {
		t.Fatalf("e2e was not told api's ETA: %+v", w)
	}
	// The ETA moves by more than the 5-minute bucket: the waiters hear it.
	f.Members[1].Status.EtaAt = t0 + 70*minute
	_, p2 := cycle(mem, input(f, t0+10*minute))
	if sent(p2, KindWait, pE2E) == nil {
		t.Fatalf("a changed ETA was not passed on: %+v", p2.Actions)
	}
	// Past it: now the blocker is asked, once.
	_, p3 := cycle(mem, input(f, t0+75*minute))
	if h := sent(p3, KindHurry, pAPI); h == nil || !strings.Contains(h.Text, "past your ETA") {
		t.Fatalf("an overdue blocker was not asked: %+v", p3.Actions)
	}
}

// cmdTo is a peer prompt on the board, open unless answered.
func cmdTo(from, to, id string, ts int64) board.Thread {
	p := msg.NewBoardPost(from, msg.BoardPersona("", "", from), msg.BoardKindCmd, "please answer "+id, ts)
	p.ToPaneID, p.CmdID = to, id
	return board.Thread{Key: msg.CmdThreadID(from, id), Root: p}
}

func TestStressedAgentIsShieldedNotHurried(t *testing.T) {
	f := shipFleet()
	threads := []board.Thread{
		cmdTo(pUI, pAPI, "c1", t0-3*minute), cmdTo(pE2E, pAPI, "c2", t0-2*minute), cmdTo(pDocs, pAPI, "c3", t0-minute),
		cmdTo(Identity, pAPI, "ci-x", t0-minute), // the fleet's own: never counted as pressure
	}
	mem := &Memory{}
	warm(t, mem, input(f, t0, threads...))
	a, p := cycle(mem, input(f, t0+5*minute, threads...))
	api := a.Member(pAPI)
	if api.OpenPrompts != 3 {
		t.Fatalf("open peer prompts = %d, want 3 (the fleet's own excluded)", api.OpenPrompts)
	}
	if api.Primary != CondStressed || api.StressScore < StressThreshold {
		t.Fatalf("api = %q (score %d %v), want stressed", api.Primary, api.StressScore, api.StressWhy)
	}
	if sent(p, KindHurry, pAPI) != nil {
		t.Fatalf("a stressed blocker was hurried: %+v", p.Actions)
	}
	s := sent(p, KindShield, pAPI)
	if s == nil || !strings.Contains(s.Text, "3 agents wait on it") || !strings.Contains(s.Text, "3 unanswered prompts") {
		t.Fatalf("no SHIELD with its reasons: %+v", s)
	}
	if ps := sent(p, KindShield, pLead); ps == nil || !strings.Contains(ps.Text, "api is STRESSED") {
		t.Fatalf("the parent was not told: %+v", p.Actions)
	}
	// One message an hour while stressed: the next onset of anything else to
	// api is held.
	f.Members[1].Status.Pct = intp(10)
	_, p2 := cycle(mem, input(f, t0+20*minute, threads...))
	if recipients(p2)[pAPI] != 0 {
		t.Fatalf("a stressed agent got a second message within the hour: %+v", p2.Actions)
	}
}

func intp(n int) *int { return &n }

func TestStrayedNotConvergingIsRealigned(t *testing.T) {
	f := shipFleet()
	// Nobody waits on api here: drop the waiters so only the stray remains.
	for i := 2; i < len(f.Members); i++ {
		f.Members[i].Status = st(fleet.WorkProgress, "own work", fleet.ActivityWorking, fleet.HealthOK)
	}
	f.Members[1].Status.Task = "polish the admin dashboard colours"
	f.Members[1].Status.Pct = intp(30)
	mem := &Memory{}
	warm(t, mem, input(f, t0))
	var p Plan
	var a Assessment
	for _, at := range []int64{t0 + 5*minute, t0 + 10*minute} {
		a, p = cycle(mem, input(f, at))
	}
	api := a.Member(pAPI)
	if api.Primary != CondStrayed {
		t.Fatalf("api = %q %v, want strayed after three still samples", api.Primary, api.StrayWhy)
	}
	r := sent(p, KindRealign, pAPI)
	if r == nil || !r.Ask {
		t.Fatalf("no REALIGN ask: %+v", p.Actions)
	}
	for _, want := range []string{"no progress (still 30%)", "shares no word with its goal", "orders API v2 endpoints", "ship the orders service v2"} {
		if !strings.Contains(r.Text, want) {
			t.Errorf("REALIGN lacks %q:\n%s", want, r.Text)
		}
	}
}

func TestStraySignals(t *testing.T) {
	base := func() fleet.Fleet {
		f := shipFleet()
		f.Members = f.Members[:2]
		f.Members[1].Parent = pLead
		return f
	}
	t.Run("wandering", func(t *testing.T) {
		f := base()
		mem := &Memory{}
		for i, task := range []string{"orders API v2 endpoints", "refactor logging", "try a new ORM"} {
			f.Members[1].Status.Task = task
			Observe(mem, input(f, t0+int64(i)*5*minute))
		}
		a := Assess(input(f, t0+10*minute), mem)
		if !strings.Contains(strings.Join(a.Member(pAPI).StrayWhy, ";"), "switched task 2 times") {
			t.Fatalf("wandering not seen: %v", a.Member(pAPI).StrayWhy)
		}
	})
	t.Run("undeclared work", func(t *testing.T) {
		f := base()
		f.Members[1].Status = st(fleet.WorkDone, "orders API v2 endpoints", fleet.ActivityWorking, fleet.HealthOK)
		mem := &Memory{}
		Observe(mem, input(f, t0))
		Observe(mem, input(f, t0+5*minute))
		a := Assess(input(f, t0+5*minute), mem)
		if !strings.Contains(strings.Join(a.Member(pAPI).StrayWhy, ";"), "while it declared done") {
			t.Fatalf("undeclared work not seen: %v", a.Member(pAPI).StrayWhy)
		}
	})
	t.Run("blocked and working is side work, not a stray", func(t *testing.T) {
		f := base()
		f.Members[1].Status = st(fleet.WorkBlocked, "orders API v2 endpoints", fleet.ActivityWorking, fleet.HealthOK, "human")
		mem := &Memory{}
		Observe(mem, input(f, t0))
		Observe(mem, input(f, t0+5*minute))
		a := Assess(input(f, t0+5*minute), mem)
		if why := a.Member(pAPI).StrayWhy; len(why) != 0 {
			t.Fatalf("legitimate side work flagged: %v", why)
		}
	})
	t.Run("overrun", func(t *testing.T) {
		f := base()
		f.Members[1].Status.EtaSetAt, f.Members[1].Status.EtaAt = t0-60*minute, t0-30*minute
		mem := &Memory{}
		Observe(mem, input(f, t0))
		a := Assess(input(f, t0), mem)
		if why := a.Member(pAPI).StrayWhy; !strings.Contains(strings.Join(why, ";"), "30m past its own ETA") {
			t.Fatalf("overrun not seen: %v", why)
		}
	})
	t.Run("a lexical miss alone is never a stray", func(t *testing.T) {
		f := base()
		f.Members[1].Status.Task = "polish the admin dashboard colours"
		mem := &Memory{}
		Observe(mem, input(f, t0))
		a := Assess(input(f, t0), mem)
		if why := a.Member(pAPI).StrayWhy; len(why) != 0 {
			t.Fatalf("off-goal alone made a stray: %v", why)
		}
	})
}

func TestDeadlockGetsOneBreaker(t *testing.T) {
	f := shipFleet()
	// ui and e2e wait on each other; docs waits on ui.
	f.Members[2].Status = st(fleet.WorkBlocked, "orders UI on v2", fleet.ActivityIdle, fleet.HealthDeadlock, pE2E)
	f.Members[3].Status = st(fleet.WorkBlocked, "e2e suite", fleet.ActivityIdle, fleet.HealthDeadlock, pUI)
	mem := &Memory{}
	warm(t, mem, input(f, t0))
	a, p := cycle(mem, input(f, t0+5*minute))
	if len(a.Cycles) != 1 || a.State != StateStuck {
		t.Fatalf("cycles %v, state %s; want one deadlock and stuck", a.Cycles, a.State)
	}
	breaker := sent(p, KindSequence, pUI) // docs waits on ui: ui frees the most
	holder := sent(p, KindSequence, pE2E)
	if breaker == nil || !breaker.Ask || !strings.Contains(breaker.Text, "You go first") {
		t.Fatalf("ui is not the breaker: %+v", p.Actions)
	}
	if holder == nil || holder.Ask || !strings.Contains(holder.Text, "ui goes first") {
		t.Fatalf("e2e was not told to hold: %+v", holder)
	}
	if sent(p, KindWait, pUI) != nil || sent(p, KindWait, pE2E) != nil {
		t.Fatalf("ring members also got WAITs: %+v", p.Actions)
	}
}

func TestNoDirectiveOnTopOfTheWatchsCheckin(t *testing.T) {
	f := shipFleet()
	mem := &Memory{}
	warm(t, mem, input(f, t0))
	f.Members[1].Status.CheckinAt = t0 + 4*minute // the watch just asked api
	_, p := cycle(mem, input(f, t0+5*minute))
	if sent(p, KindHurry, pAPI) != nil {
		t.Fatalf("HURRY landed on top of the watch's check-in: %+v", p.Actions)
	}
	_, p2 := cycle(mem, input(f, t0+10*minute))
	if sent(p2, KindHurry, pAPI) == nil {
		t.Fatalf("HURRY was not sent once the check-in aged: %+v", p2.Actions)
	}
}

func TestIdleAgentIsPulledToTheBottleneckAfterTheWatch(t *testing.T) {
	f := shipFleet()
	f.Members[4].Status = st(fleet.WorkDone, "release notes", fleet.ActivityIdle, fleet.HealthAvailable)
	f.Members[4].Status.HealthSince = t0 + 4*minute // the watch just escalated "available"
	mem := &Memory{}
	warm(t, mem, input(f, t0))
	_, p := cycle(mem, input(f, t0+5*minute))
	if sent(p, KindPull, pLead) != nil {
		t.Fatalf("PULL went out the same cycle as the watch's escalation: %+v", p.Actions)
	}
	_, p2 := cycle(mem, input(f, t0+10*minute))
	pull := sent(p2, KindPull, pLead)
	if pull == nil {
		t.Fatalf("no PULL to the parent: %+v", p2.Actions)
	}
	if !strings.Contains(pull.Text, "docs is idle") || !strings.Contains(pull.Text, "The bottleneck is api with 2 waiting") {
		t.Fatalf("PULL is not goal-aware:\n%s", pull.Text)
	}
}

func TestFleetStateRiskAndConfirm(t *testing.T) {
	f := shipFleet()
	mem := &Memory{}
	warm(t, mem, input(f, t0))
	a, _ := cycle(mem, input(f, t0+5*minute))
	if a.State != StateAtRisk {
		t.Fatalf("state = %s %v, want at-risk (bottleneck with 3 waiting and no ETA)", a.State, a.Why)
	}
	// Past the due time: off-track, and the head is told it got worse.
	a2, p2 := cycle(mem, input(f, f.Goal.DueAt+minute))
	if a2.State != StateOffTrack {
		t.Fatalf("state = %s, want off-track past the due", a2.State)
	}
	if r := sent(p2, KindRisk, pLead); r == nil || !strings.Contains(r.Text, "OFF-TRACK (was at-risk)") {
		t.Fatalf("no RISK to the head: %+v", p2.Actions)
	}
	// Every member goal met while the fleet's is open: ask the head.
	g := shipFleet()
	for i := range g.Members {
		g.Members[i].Goal = &fleet.Goal{Text: "x", State: fleet.GoalMet}
		g.Members[i].Status = st(fleet.WorkDone, "x", fleet.ActivityIdle, fleet.HealthAvailable)
	}
	mem2 := &Memory{}
	warm(t, mem2, input(g, t0))
	_, p3 := cycle(mem2, input(g, t0+5*minute))
	if c := sent(p3, KindConfirm, pLead); c == nil || !strings.Contains(c.Text, "rysh fleet goal met ship") {
		t.Fatalf("no CONFIRM to the head: %+v", p3.Actions)
	}
	_, p4 := cycle(mem2, input(g, t0+10*minute))
	if sent(p4, KindConfirm, pLead) != nil {
		t.Fatal("CONFIRM repeated")
	}
}

func TestCriticalPathFinish(t *testing.T) {
	f := shipFleet()
	f.Members[1].Status.EtaAt, f.Members[1].Status.EtaSetAt = t0+30*minute, t0
	// e2e will need 20 minutes once api is through.
	f.Members[3].Status.EtaSetAt, f.Members[3].Status.EtaAt = t0, t0+20*minute
	mem := &Memory{}
	Observe(mem, input(f, t0))
	a := Assess(input(f, t0), mem)
	if got := a.Member(pE2E).Finish; got != t0+50*minute {
		t.Fatalf("e2e finish = %s, want api's ETA + its own 20m", time.UnixMilli(got).UTC())
	}
	if a.Unknown == 0 {
		t.Fatal("ui and docs have no estimate: the fleet finish must say some paths are unknown")
	}
}

func TestCadenceAdaptsAndFixedDoesNot(t *testing.T) {
	calm := shipFleet()
	calm.Members = calm.Members[:2]
	calm.Members[1].Status.EtaAt = t0 + 60*minute
	mem := &Memory{}
	var got []int
	for i := 0; i < 5; i++ {
		_, p := cycle(mem, input(calm, t0+int64(i)*30*minute))
		got = append(got, p.NextSec)
	}
	if fmt.Sprint(got) != "[300 600 1200 1800 1800]" {
		t.Fatalf("calm cadence = %v, want doubling from 5m to 30m", got)
	}
	_, p := cycle(mem, input(shipFleet(), t0+200*minute))
	if p.NextSec != 300 {
		t.Fatalf("trouble did not bring the cadence back to the minimum: %d", p.NextSec)
	}
	in := input(calm, t0)
	in.Config.Mode = fleet.GoalExecFixed
	_, pf := cycle(&Memory{}, in)
	if pf.NextSec != 300 {
		t.Fatalf("fixed cadence = %d", pf.NextSec)
	}
}

func TestAnUnreadableBoardIsUnknownPressureNotZero(t *testing.T) {
	in := input(shipFleet(), t0)
	in.BoardOK = false
	mem := &Memory{}
	Observe(mem, in)
	a := Assess(in, mem)
	if a.Member(pAPI).OpenPrompts != -1 {
		t.Fatalf("open prompts = %d on an unreadable board, want -1 (unknown)", a.Member(pAPI).OpenPrompts)
	}
	p := MakePlan(a, in, mem)
	if !strings.Contains(p.Report, "pressure unknown") {
		t.Fatalf("the report hides that the board was unreadable:\n%s", p.Report)
	}
}

func TestReportOnlyOnChangeOrMaxCadence(t *testing.T) {
	calm := shipFleet()
	calm.Members = calm.Members[:2]
	calm.Members[1].Status.EtaAt = t0 + 60*minute
	mem := &Memory{}
	_, p1 := cycle(mem, input(calm, t0))
	if !strings.HasPrefix(p1.Report, "ON-TRACK") {
		t.Fatalf("report = %q", p1.Report)
	}
	if _, p2 := cycle(mem, input(calm, t0+5*minute)); p2.Report != "" {
		t.Fatalf("an unchanged fleet was re-reported: %q", p2.Report)
	}
	if _, p3 := cycle(mem, input(calm, t0+31*minute)); p3.Report == "" {
		t.Fatal("no report after the max cadence")
	}
}

// Found live (design 055 demo, 2026-10-14): three regressions.
func TestLiveFindings(t *testing.T) {
	t.Run("a stressed agent gets its SHIELD even after an earlier HURRY", func(t *testing.T) {
		f := shipFleet()
		mem := &Memory{}
		warm(t, mem, input(f, t0))
		_, p := cycle(mem, input(f, t0+5*minute))
		if sent(p, KindHurry, pAPI) == nil {
			t.Fatal("no HURRY first")
		}
		threads := []board.Thread{cmdTo(pUI, pAPI, "c1", t0+6*minute), cmdTo(pE2E, pAPI, "c2", t0+6*minute), cmdTo(pDocs, pAPI, "c3", t0+6*minute)}
		_, p2 := cycle(mem, input(f, t0+10*minute, threads...))
		if sent(p2, KindShield, pAPI) == nil {
			t.Fatalf("the stressed agent's own SHIELD was held by the hour cap: %+v", p2.Actions)
		}
	})
	t.Run("a long estimate is not a stray after two cadences", func(t *testing.T) {
		f := shipFleet()
		f.Members = f.Members[:1]
		f.Members[0].Status.Pct = intp(30)
		f.Members[0].Status.EtaSetAt, f.Members[0].Status.EtaAt = t0, t0+90*minute
		mem := &Memory{}
		var a Assessment
		for _, at := range []int64{t0, t0 + minute, t0 + 2*minute} {
			Observe(mem, input(f, at))
			a = Assess(input(f, at), mem)
		}
		if why := a.Member(pLead).StrayWhy; len(why) != 0 {
			t.Fatalf("a 90-minute task was called strayed after 2 minutes: %v", why)
		}
		// Still for a quarter of its estimate: now it is.
		for _, at := range []int64{t0 + 12*minute, t0 + 25*minute} {
			Observe(mem, input(f, at))
			a = Assess(input(f, at), mem)
		}
		if len(a.Member(pLead).StrayWhy) == 0 {
			t.Fatal("still for a quarter of its own estimate and not flagged")
		}
	})
	t.Run("a WAIT is sent once per change and never escalated", func(t *testing.T) {
		mem := &Memory{}
		warm(t, mem, input(shipFleet(), t0))
		_, p := cycle(mem, input(shipFleet(), t0+5*minute))
		if sent(p, KindWait, pDocs) == nil {
			t.Fatal("no WAIT at onset")
		}
		for _, at := range []int64{t0 + 15*minute, t0 + 25*minute, t0 + 35*minute} {
			_, px := cycle(mem, input(shipFleet(), at))
			if sent(px, KindWait, pDocs) != nil {
				t.Fatalf("an unchanged WAIT was re-sent at +%dm: %+v", (at-t0)/minute, px.Actions)
			}
		}
	})
}

// Found live in take 3 (design 055 §12, findings 5 and 6).
func TestLiveFindingsTake3(t *testing.T) {
	t.Run("PULL waits a cadence after the declaration, not after the watch's verdict", func(t *testing.T) {
		f := shipFleet()
		mem := &Memory{}
		warm(t, mem, input(f, t0))
		// docs declares done at +5m; the watch has not judged it yet.
		f.Members[4].Status = st(fleet.WorkDone, "release notes", fleet.ActivityIdle, fleet.HealthOK)
		f.Members[4].Status.WorkSince = t0 + 5*minute
		_, p := cycle(mem, input(f, t0+5*minute+30_000))
		if sent(p, KindPull, pLead) != nil {
			t.Fatalf("PULL raced the watch's own escalation: %+v", p.Actions)
		}
		_, p2 := cycle(mem, input(f, t0+11*minute))
		if sent(p2, KindPull, pLead) == nil {
			t.Fatalf("no PULL a cadence later: %+v", p2.Actions)
		}
	})
	t.Run("an escalation that reaches nobody new is not re-sent", func(t *testing.T) {
		f := shipFleet()
		f.Members[4].Status = st(fleet.WorkDone, "release notes", fleet.ActivityIdle, fleet.HealthAvailable)
		mem := &Memory{}
		warm(t, mem, input(f, t0))
		_, p := cycle(mem, input(f, t0+5*minute))
		if sent(p, KindPull, pLead) == nil {
			t.Fatal("no PULL at onset")
		}
		_, p2 := cycle(mem, input(f, t0+15*minute))
		if sent(p2, KindPull, pLead) != nil {
			t.Fatalf("the same PULL was repeated to the same head: %+v", p2.Actions)
		}
		_, p3 := cycle(mem, input(f, t0+25*minute))
		if !strings.Contains(strings.Join(p3.Alerts, "\n"), "ESCALATED (PULL, level 3)") {
			t.Fatalf("level 3 raised no alert for the human: %v", p3.Alerts)
		}
	})
}
