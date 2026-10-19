// SPDX-License-Identifier: Apache-2.0

// Package fleetgoal is the goal executor's mind (design 055): it observes a
// fleet, judges whether the fleet and each of its agents are on track to their
// goals, and plans the few messages that keep them moving in order.
//
// PURE, LIKE internal/fleet AND internal/board: no NATS, no clock, no I/O. The
// actor around it (internal/actors/fleet_goal_executor.go) reads the ledger and
// the board, hands both in with `now`, and sends what Plan returns. Every
// verdict below is therefore reproducible in a test to the millisecond, which
// is the point: a coordinator whose judgement cannot be replayed cannot be
// trusted to interrupt anyone.
//
// IT READS WHAT THE HUMAN READS. The assessment is built on board.BuildFlow,
// board.BuildFlowDeps and board.TeamRows — the model behind the team view (t)
// and the dependency tree — so anything the executor says about a bottleneck
// or a waiting count can be checked against the screen.
package fleetgoal

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
	"github.com/rysh-ai/rysh-cli-code/internal/fleet"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Identity is the board identity the executor posts and sends as. The fleet
// actor's: the executor is its child, and the flow view draws both as the one
// `fleet` node. The envelope says which of the two is speaking.
const Identity = "fleet"

// Board post kinds the executor writes.
const (
	KindReport = "goal-report"
	KindAlert  = "goal-alert"
)

// Conditions — per member, several may hold; the primary is the first in
// ConditionOrder that does.
const (
	CondDeadlock    = "deadlock"
	CondStressed    = "stressed"
	CondStrayed     = "strayed"
	CondStalled     = "stalled"
	CondBlocker     = "blocker"
	CondBlocked     = "blocked"
	CondUnblocked   = "unblocked"
	CondIdle        = "idle"
	CondProgressing = "progressing"
	CondMet         = "met"
)

// ConditionOrder is the priority of conditions: the most actionable first.
var ConditionOrder = []string{CondDeadlock, CondStressed, CondStrayed, CondStalled, CondBlocker,
	CondBlocked, CondUnblocked, CondIdle, CondProgressing, CondMet}

// Fleet states.
const (
	StateStarting = "starting" // nobody has declared anything yet
	StateOnTrack  = "on-track"
	StateAtRisk   = "at-risk"
	StateOffTrack = "off-track"
	StateStuck    = "stuck"
	StateMet      = "met"
	StateNoGoal   = "no-goal"
)

// stateRank orders fleet states by how bad they are, for "did it get worse?".
var stateRank = map[string]int{StateMet: 0, StateStarting: 0, StateOnTrack: 0, StateNoGoal: 0,
	StateAtRisk: 1, StateOffTrack: 2, StateStuck: 3}

// Thresholds. Variables, not constants, only so a test can name them; the
// actor never changes them.
var (
	// StressThreshold is the score at which an agent is stressed (§5.1).
	StressThreshold = 3
	// RecentWindow is how far back "recent prompts" reach.
	RecentWindow = 15 * time.Minute
	// WanderWindow bounds the "wandering" signal: task texts declared within it.
	WanderWindow = 30 * time.Minute
	// OverrunGrace is the least slack past an agent's own ETA before it is
	// called an overrun.
	OverrunGrace = 10 * time.Minute
	// sampleCap is how many observations per member the memory keeps.
	sampleCap = 12
)

// Input is one cycle's observation.
type Input struct {
	Fleet fleet.Fleet
	// Threads is the fleet board's recent window; nil with BoardOK false when
	// the recorder did not answer — pressure is then UNKNOWN, never zero.
	Threads []board.Thread
	BoardOK bool
	Now     int64
	Config  fleet.GoalExec // resolved
}

// Sample is one observation of a member, kept across cycles: drift is only
// visible over time.
type Sample struct {
	At       int64
	Work     string
	Task     string
	Activity string
	Pct      int // -1 when none declared
	Updated  int64
}

// MemberAssessment is one agent's verdict.
type MemberAssessment struct {
	PaneID string
	Label  string
	Role   string
	Parent string // pane id; may be outside the fleet

	Primary    string
	Conditions []string

	Work, Task, Activity, Health string
	Pct                          *int
	EtaAt                        int64
	Goal                         *fleet.Goal

	// Waiting is how many distinct agents wait on this one, directly or not —
	// the dependency tree's count.
	Waiting int
	// Waiters are the agents blocked directly on this one (pane ids).
	Waiters []string
	// BlockedBy are its blockers (pane ids, "human", "ext:…").
	BlockedBy []string

	// Pressure, from the board. -1 when the board could not be read.
	OpenPrompts   int
	RecentPrompts int

	StressScore int
	StressWhy   []string
	StrayWhy    []string

	// Finish is when it is expected to be done (critical path); 0 = unknown.
	Finish int64
}

// Has reports whether m has condition c.
func (m *MemberAssessment) Has(c string) bool {
	for _, x := range m.Conditions {
		if x == c {
			return true
		}
	}
	return false
}

// Assessment is one cycle's verdict on the whole fleet.
type Assessment struct {
	Fleet   string
	BoardID string
	State   string
	Why     []string

	// Pct is the mean declared pct over members with one; -1 when none.
	Pct int
	// Finish is the critical-path finish of the fleet; 0 when unknown.
	// Unknown counts the open paths with no estimate.
	Finish  int64
	Unknown int
	DueAt   int64

	Bottleneck        string // pane id ("" none)
	BottleneckWaiting int

	// Members in chain order — the team view's order.
	Members []MemberAssessment
	// Cycles are deadlocks, each in blocked_by order (pane ids).
	Cycles [][]string
	// Deps is the dependency tree exactly as the views render it.
	Deps []string
	// BoardOK is false when pressure could not be read.
	BoardOK bool
}

// Member finds a member's assessment by pane id.
func (a *Assessment) Member(id string) *MemberAssessment {
	for i := range a.Members {
		if a.Members[i].PaneID == id {
			return &a.Members[i]
		}
	}
	return nil
}

// Observe records one sample per member in mem. Call it once per cycle,
// before Assess: stray signals compare this cycle with the previous ones.
func Observe(mem *Memory, in Input) {
	mem.init()
	for _, m := range in.Fleet.Members {
		st := m.Status
		s := Sample{At: in.Now, Pct: -1}
		if st != nil {
			s.Work, s.Task, s.Activity, s.Updated = st.Work, st.Task, st.Activity, st.UpdatedAt
			if st.Pct != nil {
				s.Pct = *st.Pct
			}
		}
		list := append(mem.Samples[m.PaneID], s)
		if len(list) > sampleCap {
			list = list[len(list)-sampleCap:]
		}
		mem.Samples[m.PaneID] = list
	}
	// Forget members that left: their history must not haunt a new pane.
	live := map[string]bool{}
	for _, m := range in.Fleet.Members {
		live[m.PaneID] = true
	}
	for id := range mem.Samples {
		if !live[id] {
			delete(mem.Samples, id)
		}
	}
}

// FlowPanes renders a fleet's members as the flow model takes them, with
// exactly the pane meta the actors mirror — so the flow the executor builds is
// the flow the board pane draws.
func FlowPanes(f *fleet.Fleet) []board.FlowPane {
	out := make([]board.FlowPane, 0, len(f.Members))
	for _, m := range f.Members {
		meta := map[string]string{
			"board.id":     f.BoardID,
			"fleet.role":   m.Role,
			"fleet.parent": m.Parent,
		}
		if st := m.Status; st != nil {
			meta["fleet.work"] = st.Work
			meta["fleet.task"] = st.Task
			meta["fleet.blocked_by"] = strings.Join(st.BlockedBy, ",")
			meta["fleet.activity"] = st.Activity
			meta["fleet.health"] = st.Health
		}
		out = append(out, board.FlowPane{ID: m.PaneID, Name: m.Label, Meta: meta})
	}
	return out
}

// Assess judges the fleet. mem must have been fed this cycle by Observe.
func Assess(in Input, mem *Memory) Assessment {
	mem.init()
	f := &in.Fleet
	now := in.Now
	a := Assessment{Fleet: f.Name, BoardID: f.BoardID, Pct: -1, BoardOK: in.BoardOK}
	if f.Goal != nil {
		a.DueAt = f.Goal.DueAt
	}

	// The views. The flow is built from the board and the ledger exactly as
	// the board pane builds it; the tree and the table come from the flow.
	flow := board.BuildFlow(f.BoardID, in.Threads, FlowPanes(f))
	deps := board.BuildFlowDeps(flow)
	a.Deps = board.RenderFlowDeps(deps)
	if b := deps.Bottleneck(); b != nil && memberOf(f, b.ID) != nil {
		a.Bottleneck, a.BottleneckWaiting = b.ID, b.Waiting
	}

	waiters, waiting := waitGraph(f)
	open, recent := pressure(in, f)
	cycles := deadlockCycles(f)
	a.Cycles = cycles

	// Chain order: the table's, then anyone the table does not list.
	var order []string
	seen := map[string]bool{}
	for _, r := range board.TeamRows(flow) {
		if memberOf(f, r.ID) != nil && !seen[r.ID] {
			seen[r.ID] = true
			order = append(order, r.ID)
		}
	}
	for _, m := range f.Members {
		if !seen[m.PaneID] {
			seen[m.PaneID] = true
			order = append(order, m.PaneID)
		}
	}

	for _, id := range order {
		m := memberOf(f, id)
		ma := MemberAssessment{PaneID: m.PaneID, Label: label(m), Role: m.Role, Parent: m.Parent,
			Goal: m.Goal, Waiting: waiting[id], Waiters: waiters[id], OpenPrompts: open[id], RecentPrompts: recent[id]}
		if !in.BoardOK {
			ma.OpenPrompts, ma.RecentPrompts = -1, -1
		}
		st := m.Status
		if st != nil {
			ma.Work, ma.Task, ma.Activity, ma.Health = st.Work, st.Task, st.Activity, st.Health
			ma.Pct, ma.EtaAt = st.Pct, st.EtaAt
			ma.BlockedBy = append([]string(nil), st.BlockedBy...)
		}
		ma.StressScore, ma.StressWhy = stress(&ma, st, now)
		ma.StrayWhy = stray(&ma, st, f, mem.Samples[id], in, now)
		ma.Conditions = conditions(&ma, st)
		if len(ma.Conditions) > 0 {
			ma.Primary = ma.Conditions[0]
		}
		a.Members = append(a.Members, ma)
	}

	criticalPath(&a, f, now)
	fleetState(&a, f, now)
	return a
}

// waitGraph returns, per member, the agents blocked directly on it and how
// many wait on it directly or transitively — the dependency tree's definition.
func waitGraph(f *fleet.Fleet) (direct map[string][]string, total map[string]int) {
	direct = map[string][]string{}
	for _, m := range f.Members {
		if m.Status == nil || m.Status.Work != fleet.WorkBlocked {
			continue
		}
		for _, b := range m.Status.BlockedBy {
			direct[b] = append(direct[b], m.PaneID)
		}
	}
	for k := range direct {
		sort.Strings(direct[k])
	}
	total = map[string]int{}
	for id := range direct {
		seen := map[string]bool{id: true}
		stack := append([]string(nil), direct[id]...)
		n := 0
		for len(stack) > 0 {
			x := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if seen[x] {
				continue
			}
			seen[x] = true
			n++
			stack = append(stack, direct[x]...)
		}
		total[id] = n
	}
	return direct, total
}

// pressure counts, per member, the PEER prompts on the board still open for it
// and those received recently. The executor's and the fleet watch's own
// messages are excluded: counting them would make the coordinator the cause
// of the stress it reports.
func pressure(in Input, f *fleet.Fleet) (open, recent map[string]int) {
	open, recent = map[string]int{}, map[string]int{}
	if !in.BoardOK {
		return open, recent
	}
	reply := &board.QueryReply{Threads: in.Threads}
	cut := in.Now - RecentWindow.Milliseconds()
	for _, m := range f.Members {
		for _, it := range board.Inbox(reply, m.PaneID) {
			if it.FromPaneID == Identity {
				continue
			}
			if !it.Answered {
				open[m.PaneID]++
			}
			if it.TS >= cut {
				recent[m.PaneID]++
			}
		}
	}
	return open, recent
}

// stress scores an agent's pressure (design 055 §5.1).
func stress(ma *MemberAssessment, st *fleet.MemberStatus, now int64) (int, []string) {
	if st == nil || st.Work == fleet.WorkDone || (ma.Goal != nil && ma.Goal.IsMet()) {
		return 0, nil
	}
	score := 0
	var why []string
	switch {
	case ma.Waiting >= 3:
		score += 2
		why = append(why, fmt.Sprintf("%d agents wait on it", ma.Waiting))
	case ma.Waiting >= 1:
		score++
		why = append(why, fmt.Sprintf("%d agent%s wait%s on it", ma.Waiting, plural(ma.Waiting), verbS(ma.Waiting)))
	}
	switch {
	case ma.OpenPrompts >= 3:
		score += 2
		why = append(why, fmt.Sprintf("%d unanswered prompts", ma.OpenPrompts))
	case ma.OpenPrompts == 2:
		score++
		why = append(why, "2 unanswered prompts")
	}
	if ma.RecentPrompts >= 4 {
		score++
		why = append(why, fmt.Sprintf("%d prompts in the last %s", ma.RecentPrompts, fleet.DurationText(RecentWindow)))
	}
	if ma.Waiting >= 1 && st.EtaAt > 0 && now > st.EtaAt {
		score++
		why = append(why, "past its own ETA while others wait")
	}
	if st.CheckinMissed >= 1 {
		score++
		why = append(why, "missed a check-in")
	}
	return score, why
}

// stray reports the signals that the agent is busy but not converging on its
// goal (design 055 §5.2). Empty when it is on course or there is no basis.
func stray(ma *MemberAssessment, st *fleet.MemberStatus, f *fleet.Fleet, samples []Sample, in Input, now int64) []string {
	if st == nil || (ma.Goal != nil && ma.Goal.IsMet()) {
		return nil
	}
	var why []string
	minGap := int64(in.Config.MinSec) * 1000

	// Not converging: progress + working across >= 3 samples spanning >= two
	// cadences, same task, a declared pct that has not moved. Scaled to the
	// agent's own estimate while it is current: a 90-minute task does not
	// move its pct in two minutes (found live: a lead with a 90 m ETA was
	// called strayed after 2 m), so the still span must also reach a quarter
	// of that estimate.
	if n := len(samples); n >= 3 {
		tail := samples[n-3:]
		need := 2 * minGap
		if st.EtaAt > now && st.EtaSetAt > 0 {
			need = max(need, (st.EtaAt-st.EtaSetAt)/4)
		}
		ok := tail[2].At-tail[0].At >= need
		for _, s := range tail {
			if s.Work != fleet.WorkProgress || s.Activity != fleet.ActivityWorking || s.Pct < 0 ||
				s.Task != tail[0].Task || s.Pct != tail[0].Pct {
				ok = false
			}
		}
		if ok {
			why = append(why, fmt.Sprintf("working on %q for %s with no progress (still %d%%)",
				clip(tail[0].Task, 50), fleet.DurationText(time.Duration(tail[2].At-tail[0].At)*time.Millisecond), tail[0].Pct))
		}
	}

	// Wandering: >= 3 task texts within the window, no pct increase, no done.
	cut := now - WanderWindow.Milliseconds()
	tasks := map[string]bool{}
	var order []string
	maxPct, firstPct, done := -1, -2, false
	for _, s := range samples {
		if s.At < cut {
			continue
		}
		if s.Work == fleet.WorkDone {
			done = true
		}
		if s.Task != "" && !tasks[s.Task] {
			tasks[s.Task] = true
			order = append(order, s.Task)
		}
		if s.Pct >= 0 {
			if firstPct == -2 {
				firstPct = s.Pct
			}
			if s.Pct > maxPct {
				maxPct = s.Pct
			}
		}
	}
	if len(order) >= 3 && !done && (firstPct == -2 || maxPct <= firstPct) {
		why = append(why, fmt.Sprintf("switched task %d times in %s without progress (%s)",
			len(order)-1, fleet.DurationText(WanderWindow), clip(strings.Join(order, " → "), 90)))
	}

	// Undeclared work: working for two cycles while it says idle or done.
	// Blocked-and-working is legitimate side work — the executor suggests it.
	if n := len(samples); n >= 2 {
		a, b := samples[n-2], samples[n-1]
		if (b.Work == fleet.WorkIdle || b.Work == fleet.WorkDone) && b.Work == a.Work &&
			a.Activity == fleet.ActivityWorking && b.Activity == fleet.ActivityWorking {
			why = append(why, fmt.Sprintf("working on screen while it declared %s", b.Work))
		}
	}

	// Overrun: past its own estimate by more than the grace.
	if st.Work == fleet.WorkProgress && st.EtaAt > 0 {
		slack := max(OverrunGrace.Milliseconds(), (st.EtaAt-st.EtaSetAt)/2)
		if now > st.EtaAt+slack {
			why = append(why, fmt.Sprintf("%s past its own ETA", fleet.DurationText(time.Duration(now-st.EtaAt)*time.Millisecond)))
		}
	}

	// Off-goal is explanatory only: a lexical miss alone is too often just a
	// different phrasing, and a false stray costs a working agent its turn.
	if len(why) > 0 && offGoal(st.Task, ma.Goal, f.Goal) {
		why = append(why, "its task shares no word with its goal or the fleet goal")
	}
	return why
}

// offGoal reports whether task shares no content word with the goals.
func offGoal(task string, own, fleetGoal *fleet.Goal) bool {
	tw := contentWords(task)
	if len(tw) < 2 {
		return false
	}
	var ref strings.Builder
	for _, g := range []*fleet.Goal{own, fleetGoal} {
		if g != nil {
			ref.WriteString(g.Text + " " + g.DoneWhen + " ")
		}
	}
	gw := contentWords(ref.String())
	if len(gw) == 0 {
		return false
	}
	for w := range tw {
		if gw[w] {
			return false
		}
	}
	return true
}

var stopwords = map[string]bool{"with": true, "from": true, "that": true, "this": true, "into": true,
	"then": true, "than": true, "have": true, "will": true, "make": true, "done": true, "work": true,
	"task": true, "goal": true, "when": true, "what": true, "your": true, "their": true, "about": true}

func contentWords(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}) {
		if len(w) >= 4 && !stopwords[w] {
			out[strings.TrimSuffix(w, "s")] = true
		}
	}
	return out
}

// conditions lists what holds, in ConditionOrder.
func conditions(ma *MemberAssessment, st *fleet.MemberStatus) []string {
	has := map[string]bool{}
	met := ma.Goal != nil && ma.Goal.IsMet()
	if st != nil {
		switch st.Health {
		case fleet.HealthDeadlock:
			has[CondDeadlock] = true
		case fleet.HealthStalled, fleet.HealthUnresponsive:
			has[CondStalled] = true
		case fleet.HealthUnblocked:
			has[CondUnblocked] = true
		case fleet.HealthAvailable, fleet.HealthSilent:
			if !met {
				has[CondIdle] = true
			}
		}
		if ma.StressScore >= StressThreshold {
			has[CondStressed] = true
		}
		if len(ma.StrayWhy) > 0 {
			has[CondStrayed] = true
		}
		if ma.Waiting >= 1 && st.Work != fleet.WorkDone {
			has[CondBlocker] = true
		}
		switch st.Work {
		case fleet.WorkBlocked:
			has[CondBlocked] = true
		case fleet.WorkIdle, fleet.WorkDone:
			if !met {
				has[CondIdle] = true
			}
		case fleet.WorkProgress:
			if !has[CondStalled] {
				has[CondProgressing] = true
			}
		}
	}
	if met {
		has[CondMet] = true
	}
	var out []string
	for _, c := range ConditionOrder {
		if has[c] {
			out = append(out, c)
		}
	}
	return out
}

// deadlockCycles finds each blocked_by ring among blocked members, once, in
// edge order, starting from its first member in fleet order.
func deadlockCycles(f *fleet.Fleet) [][]string {
	edges := map[string][]string{}
	var ids []string
	for _, m := range f.Members {
		if m.Status != nil && m.Status.Work == fleet.WorkBlocked {
			edges[m.PaneID] = m.Status.BlockedBy
			ids = append(ids, m.PaneID)
		}
	}
	colour := map[string]int{}
	inCycle := map[string]bool{}
	var out [][]string
	var stack []string
	var visit func(n string)
	visit = func(n string) {
		colour[n] = 1
		stack = append(stack, n)
		for _, next := range edges[n] {
			switch colour[next] {
			case 0:
				if _, ok := edges[next]; ok {
					visit(next)
				}
			case 1:
				for i := len(stack) - 1; i >= 0; i-- {
					if stack[i] == next {
						ring := append([]string(nil), stack[i:]...)
						fresh := true
						for _, id := range ring {
							if inCycle[id] {
								fresh = false
							}
						}
						if fresh {
							for _, id := range ring {
								inCycle[id] = true
							}
							out = append(out, ring)
						}
						break
					}
				}
			}
		}
		stack = stack[:len(stack)-1]
		colour[n] = 2
	}
	for _, id := range ids {
		if colour[id] == 0 {
			visit(id)
		}
	}
	return out
}

// criticalPath estimates when each member, and the fleet, will be done
// (design 055 §6). Unknown propagates: an ETA is never invented.
func criticalPath(a *Assessment, f *fleet.Fleet, now int64) {
	const unknown = int64(-1)
	memo := map[string]int64{}
	visiting := map[string]bool{}
	var finish func(id string) int64
	finish = func(id string) int64 {
		if v, ok := memo[id]; ok {
			return v
		}
		m := memberOf(f, id)
		if m == nil || visiting[id] {
			return unknown
		}
		visiting[id] = true
		defer delete(visiting, id)
		st := m.Status
		v := unknown
		switch {
		case m.Goal != nil && m.Goal.IsMet():
			v = now
		case st == nil:
			v = unknown
		case st.Work == fleet.WorkDone:
			v = now
		case st.Work == fleet.WorkProgress && st.EtaAt > 0:
			v = max(st.EtaAt, now)
		case st.Work == fleet.WorkBlocked:
			start := now
			for _, b := range st.BlockedBy {
				fb := unknown
				if memberOf(f, b) != nil {
					fb = finish(b)
				}
				if fb == unknown {
					start = unknown
					break
				}
				start = max(start, fb)
			}
			if start != unknown && st.EtaAt > 0 && st.EtaSetAt > 0 {
				v = start + (st.EtaAt - st.EtaSetAt)
			}
		case st.Work == fleet.WorkIdle && (m.Goal == nil || !m.Goal.IsOpen()):
			v = now
		}
		memo[id] = v
		return v
	}
	for i := range a.Members {
		ma := &a.Members[i]
		v := finish(ma.PaneID)
		if v == unknown {
			ma.Finish = 0
			if !ma.Has(CondMet) {
				a.Unknown++
			}
			continue
		}
		ma.Finish = v
		a.Finish = max(a.Finish, v)
	}
	n, sum := 0, 0
	for _, ma := range a.Members {
		if ma.Pct != nil {
			n++
			sum += *ma.Pct
		}
	}
	if n > 0 {
		a.Pct = sum / n
	}
}

// fleetState derives the fleet's state and why (design 055 §6).
func fleetState(a *Assessment, f *fleet.Fleet, now int64) {
	var troubled, progressing, declared, finished int
	for _, ma := range a.Members {
		if ma.Work != "" {
			declared++
		}
		if ma.Has(CondStressed) || ma.Has(CondStrayed) || ma.Has(CondStalled) {
			troubled++
		}
		if ma.Has(CondProgressing) || ma.Activity == fleet.ActivityWorking {
			progressing++
		}
		if ma.Has(CondMet) || ma.Work == fleet.WorkDone {
			finished++
		}
	}
	add := func(s string) { a.Why = append(a.Why, s) }
	g := f.Goal
	switch {
	case g != nil && g.IsMet():
		a.State = StateMet
		return
	case declared == 0:
		a.State = StateStarting
		return
	case len(a.Cycles) > 0:
		a.State = StateStuck
		add(fmt.Sprintf("%d deadlock%s", len(a.Cycles), plural(len(a.Cycles))))
		return
	case progressing == 0 && finished < len(a.Members):
		a.State = StateStuck
		add("nobody is progressing")
		return
	case g == nil || !g.IsOpen():
		a.State = StateNoGoal
		add("no fleet goal set — `rysh fleet goal set " + f.Name + " --goal '…'`")
		return
	}

	off, risk := false, false
	if g.DueAt > 0 && now > g.DueAt {
		off = true
		add(fmt.Sprintf("past the due time by %s", fleet.DurationText(time.Duration(now-g.DueAt)*time.Millisecond)))
	}
	if g.DueAt > 0 && a.Finish > g.DueAt {
		span := g.DueAt - g.SetAt
		if span > 0 && a.Finish > g.DueAt+span/4 {
			off = true
		} else {
			risk = true
		}
		add(fmt.Sprintf("expected finish %s is past the due %s", clock(a.Finish), clock(g.DueAt)))
	}
	if len(a.Members) > 0 && troubled*2 >= len(a.Members) {
		off = true
		add(fmt.Sprintf("%d of %d agents are stressed, strayed or stalled", troubled, len(a.Members)))
	} else if troubled > 0 {
		risk = true
		add(fmt.Sprintf("%d agent%s stressed, strayed or stalled", troubled, plural(troubled)))
	}
	if a.Bottleneck != "" && a.BottleneckWaiting >= 2 {
		if b := a.Member(a.Bottleneck); b != nil && (b.EtaAt == 0 || now > b.EtaAt) {
			risk = true
			add(fmt.Sprintf("bottleneck %s has %d waiting and no current ETA", b.Label, a.BottleneckWaiting))
		}
	}
	switch {
	case off:
		a.State = StateOffTrack
	case risk:
		a.State = StateAtRisk
	default:
		a.State = StateOnTrack
	}
}

func memberOf(f *fleet.Fleet, id string) *fleet.Member {
	for i := range f.Members {
		if f.Members[i].PaneID == id {
			return &f.Members[i]
		}
	}
	return nil
}

func label(m *fleet.Member) string {
	if m.Label != "" {
		return m.Label
	}
	return msg.BoardPersona("", "", m.PaneID)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func verbS(n int) string {
	if n == 1 {
		return "s"
	}
	return ""
}

func clip(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n-1]) + "…"
}

func clock(ms int64) string {
	if ms <= 0 {
		return "?"
	}
	return time.UnixMilli(ms).Format("15:04")
}
