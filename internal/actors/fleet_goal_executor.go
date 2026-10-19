// SPDX-License-Identifier: Apache-2.0

package actors

// The AgentFleetGoalExecutor (design 055): one per fleet that is up, supervised
// by the AgentFleetActor. It keeps the fleet and each agent on their goals by
// reading the same table and dependency tree the human reads, and by
// serialising goal-level communication through itself.
//
// AN ACTOR IN THE SENSE THE AFA'S OWN LOOPS ARE: one goroutine owns every byte
// of the executor's state (its memory of past cycles, the ladder, the budgets,
// the meta it mirrored) and serves one mailbox — a timer, kicks, stop. Nothing
// else touches that state, so nothing needs a lock beyond the registry's own.
//
// IT TALKS THROUGH THE BOARD AND NOTHING ELSE, exactly like the fleet watch: a
// directive is a board `cmd` delivered by the member's listener (queue) or the
// courier (grid), beside the keyboard and never through it; a report is a
// status post. The judgement is internal/fleetgoal, which is pure — this file
// gathers the inputs and carries the outputs, and decides nothing.

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
	"github.com/rysh-ai/rysh-cli-code/internal/fleet"
	"github.com/rysh-ai/rysh-cli-code/internal/fleetgoal"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Pane meta the executor mirrors (written only by it).
const (
	MetaFleetGoal      = "fleet.goal"
	MetaFleetGoalState = "fleet.goal_state"
	MetaFleetGoalPct   = "fleet.goal_pct"
	MetaFleetGoalEta   = "fleet.goal_eta"
)

// The executor's clock. goalExecUnit scales every configured number of
// seconds (cadence, debounce): a test runs a 60 s cadence in well under a
// second without the planner's arithmetic changing at all.
var (
	goalExecUnit = time.Second
	// goalExecFirstRun is the delay before the first (warm-up) cycle, in
	// units: soon enough that a new fleet gets its first report at once.
	goalExecFirstRun = 2
	// goalExecDebounce is the least time between two kicked cycles, in
	// units: a burst of declarations is one cycle.
	goalExecDebounce = 60
	// goalExecBoardWindow is how far back the board is read.
	goalExecBoardWindow = 6 * time.Hour
	// goalExecBoardTimeout bounds the board read: a slow recorder degrades a
	// cycle to "pressure unknown", it never stalls the executor.
	goalExecBoardTimeout = 2 * time.Second
)

// goalSupervisor is the AFA's set of executors, one per fleet.
type goalSupervisor struct {
	mu    sync.Mutex
	execs map[string]*goalExecutor
}

func newGoalSupervisor() *goalSupervisor {
	return &goalSupervisor{execs: map[string]*goalExecutor{}}
}

// syncGoalExecutors starts an executor for every fleet that is up and has not
// switched it off, and stops every other. Idempotent; called from the watch
// loop and after every registry write that can change the answer.
func (a *AgentFleetActor) syncGoalExecutors() {
	a.mu.Lock()
	stopped := a.stopped
	a.mu.Unlock()
	want := map[string]bool{}
	if !stopped {
		for _, f := range a.reg.List() {
			if f.State == fleet.StateUp && f.GoalExec.Resolved().Mode != fleet.GoalExecOff {
				want[f.Name] = true
			}
		}
	}
	s := a.goals
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, e := range s.execs {
		if !want[name] {
			e.stop()
			delete(s.execs, name)
		}
	}
	for name := range want {
		if s.execs[name] == nil {
			e := newGoalExecutor(a, name)
			s.execs[name] = e
			go e.run()
		}
	}
}

// kickGoalExecutor asks a fleet's executor to look again soon (debounced).
func (a *AgentFleetActor) kickGoalExecutor(name string) {
	s := a.goals
	s.mu.Lock()
	e := s.execs[msg.NormalizeBoardID(name)]
	s.mu.Unlock()
	if e != nil {
		e.kick()
	}
}

// stopGoalExecutors stops every executor (the AFA is stopping).
func (a *AgentFleetActor) stopGoalExecutors() {
	s := a.goals
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, e := range s.execs {
		e.stop()
		delete(s.execs, name)
	}
}

// GoalExecutorRunning reports whether a fleet has a live executor.
func (a *AgentFleetActor) GoalExecutorRunning(name string) bool {
	s := a.goals
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.execs[msg.NormalizeBoardID(name)] != nil
}

// goalExecutor is one fleet's executor.
type goalExecutor struct {
	afa   *AgentFleetActor
	fleet string

	kickc  chan struct{}
	done   chan struct{}
	exited chan struct{}
	once   sync.Once

	// The clock, snapshotted at birth: the goroutine never reads the package
	// variables, so a test that tunes them cannot race a running executor.
	unit     time.Duration
	first    int
	debounce int

	// Owned by run's goroutine.
	mem      fleetgoal.Memory
	lastRun  time.Time
	mirrored map[string]map[string]string
}

func newGoalExecutor(a *AgentFleetActor, name string) *goalExecutor {
	return &goalExecutor{afa: a, fleet: name, kickc: make(chan struct{}, 1), done: make(chan struct{}),
		exited: make(chan struct{}), mirrored: map[string]map[string]string{},
		unit: goalExecUnit, first: goalExecFirstRun, debounce: goalExecDebounce}
}

func (e *goalExecutor) kick() {
	select {
	case e.kickc <- struct{}{}:
	default:
	}
}

// stop ends the executor and waits for its goroutine: a cycle in flight is
// finished, never cut, and nothing of a stopped executor runs afterwards. The
// wait is bounded by one cycle (its board read is itself bounded).
func (e *goalExecutor) stop() {
	e.once.Do(func() { close(e.done) })
	select {
	case <-e.exited:
	case <-time.After(10 * time.Second):
		slog.Warn("goal executor: did not stop within 10s", "fleet", e.fleet)
	}
}

// run is the mailbox loop.
func (e *goalExecutor) run() {
	defer close(e.exited)
	timer := time.NewTimer(time.Duration(e.first) * e.unit)
	defer timer.Stop()
	due := time.Now().Add(time.Duration(e.first) * e.unit)
	reset := func(d time.Duration) {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(d)
		due = time.Now().Add(d)
	}
	for {
		select {
		case <-e.done:
			return
		case <-timer.C:
			reset(e.step())
		case <-e.kickc:
			// A kicked cycle runs now if the last one is old enough, else at
			// the debounce boundary — never later than already scheduled.
			at := e.lastRun.Add(time.Duration(e.debounce) * e.unit)
			switch {
			case e.lastRun.IsZero():
				// the warm-up has not run yet; it will
			case !time.Now().Before(at):
				reset(e.step())
			case at.Before(due):
				reset(time.Until(at))
			}
		}
	}
}

// step runs one cycle and returns the delay to the next.
func (e *goalExecutor) step() time.Duration {
	e.lastRun = time.Now()
	a := e.afa
	f := a.reg.Get(e.fleet)
	if f == nil || f.State != fleet.StateUp {
		return time.Duration(e.debounce) * e.unit // the supervisor will stop us
	}
	cfg := f.GoalExec.Resolved()
	if cfg.Mode == fleet.GoalExecOff {
		return time.Duration(cfg.MaxSec) * e.unit
	}
	pub := a.publisher()
	if pub == nil {
		return time.Duration(cfg.MinSec) * e.unit
	}
	now := time.Now().UnixMilli()

	in := fleetgoal.Input{Fleet: *f, Now: now, Config: cfg}
	if reply, err := board.Ask(a.nc, f.BoardID, board.Query{Since: now - goalExecBoardWindow.Milliseconds()}, goalExecBoardTimeout); err == nil {
		in.Threads, in.BoardOK = reply.Threads, true
	} else {
		slog.Debug("goal executor: board unreadable this cycle", "fleet", f.Name, "err", err)
	}

	fleetgoal.Observe(&e.mem, in)
	as := fleetgoal.Assess(in, &e.mem)
	plan := fleetgoal.MakePlan(as, in, &e.mem)

	for _, act := range plan.Actions {
		to := memberByPane(f, act.To)
		if to == nil {
			continue
		}
		cmdID := e.sendDirective(pub, f, to, act)
		if cmdID != "" && act.Ask {
			a.reg.GoalAskSent(f.Name, to.PaneID, cmdID, now)
		}
	}
	for _, text := range plan.Alerts {
		e.post(pub, f, fleetgoal.KindAlert, text)
	}
	if plan.Report != "" {
		e.post(pub, f, fleetgoal.KindReport, plan.Report)
	}
	for _, ma := range as.Members {
		e.mirror(pub, f, ma)
	}

	next := time.Duration(plan.NextSec) * e.unit
	gs := fleet.GoalStatus{State: as.State, Pct: as.Pct, FinishAt: as.Finish, Unknown: as.Unknown,
		Bottleneck: as.Bottleneck, Waiting: as.BottleneckWaiting, Why: as.Why, AssessedAt: now,
		NextAt: now + next.Milliseconds(), Sent: len(plan.Actions), Members: map[string]string{}}
	if plan.Report != "" {
		gs.Summary = firstLineOf(plan.Report)
	} else if old := f.GoalStatus; old != nil {
		gs.Summary = old.Summary
	}
	for _, ma := range as.Members {
		gs.Members[ma.PaneID] = ma.Primary
	}
	a.reg.SetGoalStatus(f.Name, gs)
	a.mu.Lock()
	persist := a.persist
	a.mu.Unlock()
	if persist != nil {
		if cur := a.reg.Get(f.Name); cur != nil {
			_ = persist.Save(cur)
		}
	}
	return next
}

// sendDirective posts one directive as a board cmd and returns its cmd id.
func (e *goalExecutor) sendDirective(pub *msg.NATSPublisher, f *fleet.Fleet, to *fleet.Member, act fleetgoal.Action) string {
	a := e.afa
	now := time.Now().UnixMilli()
	cmdID := "ga-" + strings.TrimPrefix(msg.MintCmdID(now), "cmd-")
	var b strings.Builder
	fmt.Fprintf(&b, "[FLEET %s | GOAL %s | FROM goal-executor | TO %s %s (pane %s) | %s]\n%s\n",
		f.Name, act.Kind, orNone(to.Role), memberPersona(to), shortID(to.PaneID), cmdID, act.Text)
	if act.Ask {
		fmt.Fprintf(&b, "Answer with ONE command — it records your status and answers this:\n  %s [--pct <0-100>] [--eta <20m>]",
			a.statusCommand(f, to))
	} else {
		fmt.Fprintf(&b, "No answer needed. To close it: %s", e.ackCommand(f, to, cmdID))
	}
	p := msg.NewBoardPost(fleetgoal.Identity, fleetgoal.Identity, msg.BoardKindCmd, b.String(), now)
	p.ToPaneID, p.ToPersona, p.CmdID = to.PaneID, memberPersona(to), cmdID
	p.ThreadID = msg.CmdThreadID(fleetgoal.Identity, cmdID)
	if err := msg.SendBoardPost(pub, f.BoardID, p); err != nil {
		slog.Warn("goal executor: could not post a directive", "fleet", f.Name, "to", to.Label, "err", err)
		return ""
	}
	return cmdID
}

func (e *goalExecutor) ackCommand(f *fleet.Fleet, to *fleet.Member, cmdID string) string {
	cmd := fmt.Sprintf("%s board ack %s --as %s --board %s", shellQuoteArg(e.afa.watch.bin), cmdID, to.PaneID, f.BoardID)
	if s := msg.SessionPrefix(); s != "" {
		cmd += " --session " + shellQuoteArg(s)
	}
	return cmd
}

// post files a goal-report or goal-alert on the fleet's board.
func (e *goalExecutor) post(pub *msg.NATSPublisher, f *fleet.Fleet, kind, text string) {
	p := msg.NewBoardPost(fleetgoal.Identity, fleetgoal.Identity, kind, text, time.Now().UnixMilli())
	if err := msg.SendBoardPost(pub, f.BoardID, p); err != nil {
		slog.Warn("goal executor: could not post", "fleet", f.Name, "kind", kind, "err", err)
	}
}

// mirror writes a member's goal verdict onto its pane meta, changed keys only.
func (e *goalExecutor) mirror(pub *msg.NATSPublisher, f *fleet.Fleet, ma fleetgoal.MemberAssessment) {
	want := map[string]string{MetaFleetGoalState: ma.Primary, MetaFleetGoal: "", MetaFleetGoalPct: "", MetaFleetGoalEta: ""}
	if ma.Goal != nil {
		want[MetaFleetGoal] = clip(ma.Goal.Text, 120)
	}
	if ma.Pct != nil {
		want[MetaFleetGoalPct] = strconv.Itoa(*ma.Pct)
	}
	if ma.EtaAt > 0 {
		want[MetaFleetGoalEta] = strconv.FormatInt(ma.EtaAt, 10)
	}
	have := e.mirrored[ma.PaneID]
	if have == nil {
		have = map[string]string{}
		e.mirrored[ma.PaneID] = have
	}
	for _, k := range []string{MetaFleetGoal, MetaFleetGoalState, MetaFleetGoalPct, MetaFleetGoalEta} {
		v := want[k]
		if old, ok := have[k]; (ok && old == v) || (!ok && v == "") {
			continue
		}
		have[k] = v
		_ = pub.Send(msg.T("pane", ma.PaneID, "inbox"), &msg.MsgPaneSetMeta{Key: k, Value: v})
	}
}

func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
