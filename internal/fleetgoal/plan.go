// SPDX-License-Identifier: Apache-2.0

package fleetgoal

// The plan: from a verdict to the FEW messages that move the fleet in order
// (design 055 §7). Most of this file is about what NOT to send.
//
// THE COMMUNICATION PROBLEM, stated: agents already talk — every blocked agent
// asks its blocker when it will be done, every parent pings its children. In a
// fleet of N that is O(N²) interruptions landing hardest on the agent the most
// others wait for. The executor makes it hub-and-spoke: the blocker is asked
// once, the waiters are told by the executor and told not to ask. Then it
// rations even its own voice — a budget per agent, nothing to an agent that is
// on track, once per onset and then a ladder, never on top of the fleet
// watch's own check-in.

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/fleet"
)

// Directive kinds.
const (
	KindSequence = "SEQUENCE" // a deadlock: one breaker goes first, the rest hold
	KindHurry    = "HURRY"    // a blocker with waiters: finish this first, give an ETA
	KindWait     = "WAIT"     // a waiter: whom you wait on, until when; do not ask them
	KindShield   = "SHIELD"   // a stressed agent: messages held, one thing to do
	KindRealign  = "REALIGN"  // a strayed agent: back to the goal
	KindPull     = "PULL"     // an idle agent's parent: give it goal work, or pair it
	KindRisk     = "RISK"     // the head: the fleet's state got worse
	KindConfirm  = "CONFIRM"  // the head: every member goal is met; is the fleet's?
)

// kindPriority orders a cycle's directives: under the budget the later ones
// wait for the next cycle.
var kindPriority = map[string]int{KindSequence: 0, KindHurry: 1, KindShield: 2, KindRealign: 3,
	KindWait: 4, KindPull: 5, KindRisk: 6, KindConfirm: 7}

// Budget and ladder. Variables only so a test can name them.
var (
	// MaxPerHour bounds the executor's messages to one agent.
	MaxPerHour = 4
	// MaxPerHourStressed bounds them for a stressed agent.
	MaxPerHourStressed = 1
	// LadderTop is the highest escalation level: 1 the agent, 2 + its parent,
	// 3 + the head and a board alert for the human.
	LadderTop = 3
)

// Memory is the executor's own state across cycles. In-process: warm-up
// (Plan's first cycle sends nothing) is the guard against a restarted daemon
// re-sending everything it said before.
type Memory struct {
	Samples map[string][]Sample
	Ladder  map[string]*Rung
	Sent    map[string][]int64 // pane -> send times within the last hour
	Cycles  int

	LastState    string
	LastReport   string // the last report's fingerprint
	LastReportAt int64
	IntervalSec  int // the adaptive cadence's current interval
}

// Rung is one (kind, agent) on the escalation ladder.
type Rung struct {
	Fingerprint string
	Level       int
	SentAt      int64
}

func (m *Memory) init() {
	if m.Samples == nil {
		m.Samples = map[string][]Sample{}
	}
	if m.Ladder == nil {
		m.Ladder = map[string]*Rung{}
	}
	if m.Sent == nil {
		m.Sent = map[string][]int64{}
	}
}

// Action is one directive to send.
type Action struct {
	Kind  string
	To    string // pane id
	Label string // display name of To
	// About is the agent the directive concerns (== To unless it is a copy
	// to a parent or the head).
	About string
	Text  string
	// Ask: the recipient's next `fleet status` answers it (HURRY, REALIGN,
	// SEQUENCE to the breaker). Otherwise informational: `board ack` closes it.
	Ask   bool
	Level int
	Key   string
}

// Plan is one cycle's output.
type Plan struct {
	Actions []Action
	// Alerts are board `goal-alert` posts (level-3 escalations, waits on the
	// human) — for the human, who reads the board.
	Alerts []string
	// Report is a `goal-report` status post, or "" when nothing changed.
	Report string
	// NextSec is when to look again.
	NextSec int
	// Held counts directives the budget deferred to a later cycle.
	Held int
}

// candidate is a directive before the ladder and the budget.
type candidate struct {
	kind  string
	about string // the member it concerns
	to    string // recipient at level 1
	fp    string // fingerprint: a changed one is a new onset
	ask   bool
	text  string
	alert string // a board alert at level 1 (waits on the human)
	// escalatesTo "parent": the recipient is already the parent / head, so
	// level 2 does not add another copy to it.
	escalatesTo string
	// once: informational, sent once per fingerprint and never escalated —
	// a legitimate wait is not a problem to climb a ladder about (found
	// live: docs, correctly waiting, was re-sent its WAIT two cadences on).
	once bool
}

// MakePlan plans one cycle. It mutates mem (ladder, budget, cadence, report).
func MakePlan(a Assessment, in Input, mem *Memory) Plan {
	mem.init()
	now := in.Now
	f := &in.Fleet
	cfg := in.Config
	warm := mem.Cycles == 0
	mem.Cycles++

	cands := candidates(a, f, now, cfg)
	sort.SliceStable(cands, func(i, j int) bool {
		if kindPriority[cands[i].kind] != kindPriority[cands[j].kind] {
			return kindPriority[cands[i].kind] < kindPriority[cands[j].kind]
		}
		// Within a kind: the agent most others wait on first.
		wi, wj := 0, 0
		if m := a.Member(cands[i].about); m != nil {
			wi = m.Waiting
		}
		if m := a.Member(cands[j].about); m != nil {
			wj = m.Waiting
		}
		return wi > wj
	})

	// Conditions that cleared leave the ladder: a recurrence is a new onset.
	live := map[string]bool{}
	for _, c := range cands {
		live[ladderKey(c)] = true
	}
	for k := range mem.Ladder {
		if !live[k] && !strings.HasPrefix(k, "CONFIRM|") { // CONFIRM manages its own rung
			delete(mem.Ladder, k)
		}
	}

	var p Plan
	heads := headsOf(f)
	perCycle := map[string]bool{}
	gap := int64(cfg.MinSec) * 1000
	for _, c := range cands {
		key := ladderKey(c)
		rung := mem.Ladder[key]
		level := 1
		switch {
		case rung == nil || rung.Fingerprint != c.fp:
			level = 1
		case !c.once && rung.Level < LadderTop && now-rung.SentAt >= 2*gap:
			level = rung.Level + 1
		default:
			continue // said, and not yet time to say it louder
		}
		if warm {
			// The first cycle after a (re)start only observes: memory is
			// in-process, and a restarted daemon must not re-send everything.
			continue
		}
		if c.to != "" && deconflict(f, c, now, gap) {
			continue
		}

		recipients := []string{}
		if c.to != "" {
			recipients = append(recipients, c.to)
		}
		ab := a.Member(c.about)
		if level >= 2 && ab != nil && ab.Parent != "" && memberOf(f, ab.Parent) != nil && ab.Parent != c.to && c.escalatesTo != "parent" {
			recipients = append(recipients, ab.Parent)
		}
		if level >= 3 {
			for _, h := range heads {
				if h != c.to && (ab == nil || h != ab.Parent || c.escalatesTo == "parent") && !contains(recipients, h) {
					recipients = append(recipients, h)
				}
			}
		}

		// An escalation that reaches nobody new is not sent again: the same
		// words to the same agent are noise (found live: a PULL to a lead with
		// nobody above it was simply repeated at level 2). The rung still
		// climbs, so level 3 raises its board alert for the human.
		if level > 1 && len(recipients) <= 1 && (len(recipients) == 0 || recipients[0] == c.to) && c.to != "" {
			mem.Ladder[key] = &Rung{Fingerprint: c.fp, Level: level, SentAt: now}
			if level >= 3 {
				p.Alerts = append(p.Alerts, fmt.Sprintf("ESCALATED (%s, level 3): %s", c.kind, firstLine(c.text)))
			}
			continue
		}

		// Budget: the primary recipient must have room, or the directive
		// waits for a later cycle (the ladder is not advanced, so it is
		// retried). Copies to parents and heads are best-effort.
		if c.to != "" && !budgetOK(mem, a, c, c.to, now, perCycle) {
			p.Held++
			continue
		}
		sentAny := false
		for i, to := range recipients {
			if i > 0 && !budgetOK(mem, a, c, to, now, perCycle) {
				continue
			}
			text := c.text
			if to != c.to {
				text = copyPreamble(a, c, to, level) + text
			}
			p.Actions = append(p.Actions, Action{Kind: c.kind, To: to, Label: labelOf(f, to), About: c.about,
				Text: text, Ask: c.ask && to == c.to, Level: level, Key: key})
			spend(mem, to, now, perCycle, c.kind)
			sentAny = true
		}
		if c.alert != "" && level == 1 {
			p.Alerts = append(p.Alerts, c.alert)
			sentAny = true
		}
		if level >= 3 {
			p.Alerts = append(p.Alerts, fmt.Sprintf("ESCALATED (%s, level 3): %s", c.kind, firstLine(c.text)))
			sentAny = true
		}
		if sentAny {
			mem.Ladder[key] = &Rung{Fingerprint: c.fp, Level: level, SentAt: now}
		}
	}

	// The fleet's own state: RISK on a worsening, CONFIRM when all member
	// goals are met but the fleet's is open. Tracked even in warm-up so a
	// restart does not report the current state as a fresh worsening.
	if !warm {
		p.Actions = append(p.Actions, fleetActions(a, f, mem, heads, now, perCycle)...)
	}
	mem.LastState = a.State

	p.Report = report(a, f, mem, p, now, cfg)
	p.NextSec = cadence(a, mem, p, cfg)
	return p
}

// candidates lists every directive the verdict calls for, before the ladder,
// the budget and deconfliction thin them.
func candidates(a Assessment, f *fleet.Fleet, now int64, cfg fleet.GoalExec) []candidate {
	var out []candidate
	goalOpen := f.Goal != nil && f.Goal.IsOpen()
	gap := int64(cfg.MinSec) * 1000

	// SEQUENCE — one breaker per deadlock.
	inCycle := map[string]bool{}
	for _, ring := range a.Cycles {
		for _, id := range ring {
			inCycle[id] = true
		}
		breaker := ring[0]
		for _, id := range ring[1:] {
			mb, mi := a.Member(breaker), a.Member(id)
			if mi == nil || mb == nil {
				continue
			}
			if mi.Waiting > mb.Waiting || (mi.Waiting == mb.Waiting && workSince(f, id) < workSince(f, breaker)) {
				breaker = id
			}
		}
		fp := strings.Join(sortedCopy(ring), ",")
		names := labelsOf(f, append(ring, ring[0]))
		bl := labelOf(f, breaker)
		for _, id := range ring {
			var b strings.Builder
			fmt.Fprintf(&b, "DEADLOCK: %s — every agent in this ring waits on the next, so none can move.\n", strings.Join(names, " → "))
			if id == breaker {
				fmt.Fprintf(&b, "You go first. Proceed on a stated assumption about what you were waiting for "+
					"(write it in --note so the others can check it), and declare progress.")
			} else {
				fmt.Fprintf(&b, "%s goes first and will proceed on a stated assumption. Hold: stay blocked, do not message the ring; "+
					"you will be told when it is through.", bl)
			}
			out = append(out, candidate{kind: KindSequence, about: id, to: id, fp: fp + "|" + breaker, ask: id == breaker, text: b.String()})
		}
	}

	for _, ma := range a.Members {
		if inCycle[ma.PaneID] || ma.Has(CondMet) {
			continue
		}
		m := memberOf(f, ma.PaneID)
		goalLine := goalText(m, f)

		// HURRY — a blocker with waiters and no current ETA. With a current
		// ETA it is left alone: the waiters are told instead.
		// Only a blocker that can MOVE is hurried — a root of the dependency
		// tree. A blocked blocker cannot finish first; its waiters are told
		// whom it waits on instead. A stressed blocker is SHIELDed instead:
		// hurrying it is the pressure.
		if ma.Waiting >= 1 && ma.Work != fleet.WorkDone && ma.Work != fleet.WorkBlocked && !ma.Has(CondStressed) &&
			(ma.EtaAt == 0 || now > ma.EtaAt) {
			fp := "no-eta"
			why := "have given no ETA"
			if ma.EtaAt > 0 {
				fp = fmt.Sprintf("overdue:%d", ma.EtaAt)
				why = fmt.Sprintf("are past your ETA (%s)", clock(ma.EtaAt))
			}
			var b strings.Builder
			fmt.Fprintf(&b, "%d agent%s wait%s on you: %s. You %s.\n", ma.Waiting, plural(ma.Waiting), verbS(ma.Waiting),
				strings.Join(labelsOf(f, ma.Waiters), ", ")+more(ma.Waiting-len(ma.Waiters)), why)
			if ma.Task != "" {
				fmt.Fprintf(&b, "Finish %q before anything else — it is what they need. ", clip(ma.Task, 80))
			} else {
				b.WriteString("Finish what they need before anything else. ")
			}
			b.WriteString("Declare an honest --eta so the executor can tell them; they have been asked not to message you.")
			if goalLine != "" {
				fmt.Fprintf(&b, "\n%s", goalLine)
			}
			if ma.Has(CondStalled) {
				b.WriteString("\nYour pane is idle while they wait: if you are waiting on someone yourself, declare blocked --blocked-by <who>.")
			}
			out = append(out, candidate{kind: KindHurry, about: ma.PaneID, to: ma.PaneID, fp: fp, ask: true, text: b.String()})
		}

		// SHIELD — stressed: to the agent and to its parent.
		if ma.Has(CondStressed) {
			why := strings.Join(ma.StressWhy, ", ")
			var b strings.Builder
			fmt.Fprintf(&b, "You are under load: %s. The executor is holding its own non-urgent messages to you and has told "+
				"the agents waiting on you not to message you.\n", why)
			if ma.Task != "" {
				fmt.Fprintf(&b, "Do the one thing that unblocks the most: %q, and declare --eta when you can.", clip(ma.Task, 80))
			} else {
				b.WriteString("Do the one thing that unblocks the most, and declare it with --task and --eta.")
			}
			// Once to the agent: re-sending it is more pressure. If the stress
			// persists, the ladder climbs on the parent's copy below.
			out = append(out, candidate{kind: KindShield, about: ma.PaneID, to: ma.PaneID, fp: "stressed", text: b.String(), once: true})
			if p := ma.Parent; p != "" && memberOf(f, p) != nil {
				t := fmt.Sprintf("%s is STRESSED: %s.\nYou are its parent: split its task, move a waiter to other work, or "+
					"take an answer off its plate — and keep new requests away from it until it reports.", ma.Label, why)
				out = append(out, candidate{kind: KindShield, about: ma.PaneID, to: p, fp: "stressed-parent", text: t, escalatesTo: "parent"})
			}
		}

		// REALIGN — strayed: back to the goal, asked.
		if ma.Has(CondStrayed) {
			var b strings.Builder
			fmt.Fprintf(&b, "You look off course: %s.\n", strings.Join(ma.StrayWhy, "; "))
			if goalLine != "" {
				fmt.Fprintf(&b, "%s\n", goalLine)
			}
			b.WriteString("Either declare the task that serves the goal now (--task, --pct, --eta), or keep this one and say how it " +
				"serves the goal in --note. If you are stuck, declare blocked --blocked-by <who>.")
			fp := strings.Join(sortedCopy(signalNames(ma.StrayWhy)), ",") + "|" + ma.Task
			out = append(out, candidate{kind: KindRealign, about: ma.PaneID, to: ma.PaneID, fp: fp, ask: true, text: b.String()})
		}

		// WAIT — blocked on members: whom, until when, do not ask them.
		if ma.Work == fleet.WorkBlocked {
			var fps, lines []string
			var outside []string
			pending := 0
			for _, b := range ma.BlockedBy {
				bm := a.Member(b)
				if bm == nil {
					outside = append(outside, b)
					fps = append(fps, b)
					continue
				}
				eta := "no ETA yet"
				bucket := "none"
				if bm.Work != fleet.WorkDone {
					pending++
				}
				if bm.Work == fleet.WorkDone {
					eta = "done"
					bucket = "done"
				} else if bm.EtaAt > 0 {
					eta = "ETA " + clock(bm.EtaAt)
					bucket = fmt.Sprint(bm.EtaAt / (5 * 60 * 1000))
				}
				others := bm.Waiting - 1
				if bm.Work == fleet.WorkBlocked && len(bm.BlockedBy) > 0 {
					eta = "itself waiting on " + strings.Join(labelsOf(f, bm.BlockedBy), ", ")
				}
				line := fmt.Sprintf("%s (%s, %s", bm.Label, orDash(bm.Work), eta)
				if others > 0 {
					line += fmt.Sprintf(", %d other%s also waiting", others, plural(others))
				}
				lines = append(lines, line+")")
				fps = append(fps, b+":"+bm.Work+":"+bucket)
			}
			// Every member blocker done is the watch's `unblocked`, not a wait.
			if pending > 0 && !ma.Has(CondUnblocked) {
				var b strings.Builder
				fmt.Fprintf(&b, "You are waiting on %s.\n", strings.Join(lines, "; "))
				b.WriteString("Do not message them about it — the executor has asked them once and will tell you the moment they " +
					"are done or their ETA changes. Meanwhile, advance the parts of your goal that do not depend on them")
				if goalLine != "" {
					fmt.Fprintf(&b, " (%s)", strings.TrimPrefix(goalLine, "Your goal: "))
				}
				b.WriteString(".")
				out = append(out, candidate{kind: KindWait, about: ma.PaneID, to: ma.PaneID, fp: strings.Join(fps, ";"), text: b.String(), once: true})
			}
			if len(outside) > 0 {
				// Waiting on the human or on something outside the fleet:
				// the board is where the human looks.
				alert := fmt.Sprintf("%s is waiting on %s (task: %s).", ma.Label, strings.Join(outside, ", "), orDash(ma.Task))
				out = append(out, candidate{kind: KindWait, about: ma.PaneID, fp: "outside:" + strings.Join(outside, ","),
					alert: alert, text: alert, to: ""})
			}
		}

		// PULL — idle while the goal is open: to its parent (the head if it
		// has none), one cycle after the watch's own `available` escalation.
		if ma.Has(CondIdle) && (goalOpen || (ma.Goal != nil && ma.Goal.IsOpen())) && ma.Primary == CondIdle {
			// Give the watch's own `available` escalation a full cadence first.
			// Its verdict lags the declaration by up to a watch tick, so the
			// declaration's age is the clock (found live: PULL reached the lead
			// six seconds BEFORE the watch's escalation — two messages, one fact).
			st := m.Status
			if st != nil && (now-st.WorkSince < gap || (st.HealthSince > 0 && now-st.HealthSince < gap)) {
				continue
			}
			to := ma.Parent
			if to == "" || memberOf(f, to) == nil {
				if hs := headsOf(f); len(hs) > 0 && hs[0] != ma.PaneID {
					to = hs[0]
				} else {
					to = ""
				}
			}
			if to == "" {
				continue
			}
			var b strings.Builder
			fmt.Fprintf(&b, "%s is idle (%s) while the goal is open.\n", ma.Label, orDash(ma.Work))
			if a.Bottleneck != "" && a.Bottleneck != ma.PaneID {
				if bn := a.Member(a.Bottleneck); bn != nil {
					fmt.Fprintf(&b, "The bottleneck is %s with %d waiting (%s): pair %s with it, or ", bn.Label, a.BottleneckWaiting, orDash(bn.Task), ma.Label)
				}
			}
			if f.Goal != nil && f.Goal.IsOpen() {
				fmt.Fprintf(&b, "give it the next piece of the fleet goal: %q.", clip(f.Goal.Text, 100))
			} else {
				b.WriteString("give it its next work order.")
			}
			out = append(out, candidate{kind: KindPull, about: ma.PaneID, to: to, fp: "idle", text: b.String(), escalatesTo: "parent"})
		}
	}
	return out
}

// fleetActions are RISK and CONFIRM, to the head(s).
func fleetActions(a Assessment, f *fleet.Fleet, mem *Memory, heads []string, now int64, perCycle map[string]bool) []Action {
	var out []Action
	worse := stateRank[a.State] > stateRank[mem.LastState] && mem.LastState != ""
	if worse && a.State != StateNoGoal {
		text := fmt.Sprintf("The fleet is %s (was %s): %s.", strings.ToUpper(a.State), mem.LastState, strings.Join(a.Why, "; "))
		if a.Bottleneck != "" {
			if b := a.Member(a.Bottleneck); b != nil {
				text += fmt.Sprintf("\nBottleneck: %s, %d waiting.", b.Label, a.BottleneckWaiting)
			}
		}
		text += "\nYou are the head: decide — re-plan, re-assign, cut scope, or move the due time (`rysh fleet goal set … --due`)."
		for _, h := range heads {
			if budgetOKFleet(mem, h, now) {
				out = append(out, Action{Kind: KindRisk, To: h, Label: labelOf(f, h), Text: text, Key: "RISK|" + f.Name, Level: 1})
				spend(mem, h, now, perCycle, KindRisk)
			}
		}
	}
	allMet := len(a.Members) > 0
	for _, ma := range a.Members {
		if ma.Goal == nil || !ma.Goal.IsMet() {
			allMet = false
		}
	}
	if allMet && f.Goal != nil && f.Goal.IsOpen() {
		key := "CONFIRM|" + f.Name
		if mem.Ladder[key] == nil {
			text := fmt.Sprintf("Every member goal is met. Is the fleet goal met — %q?", clip(f.Goal.Text, 120))
			if f.Goal.DoneWhen != "" {
				text += fmt.Sprintf("\nDone when: %s.", f.Goal.DoneWhen)
			}
			text += fmt.Sprintf("\nIf so: `rysh fleet goal met %s --note '<evidence>'`. If not, set the goals that remain.", f.Name)
			for _, h := range heads {
				out = append(out, Action{Kind: KindConfirm, To: h, Label: labelOf(f, h), Text: text, Key: key, Ask: false, Level: 1})
				spend(mem, h, now, perCycle, KindConfirm)
			}
			mem.Ladder[key] = &Rung{Fingerprint: "all-met", Level: 1, SentAt: now}
		}
	} else {
		delete(mem.Ladder, "CONFIRM|"+f.Name)
	}
	return out
}

// deconflict: no executor directive on top of the fleet watch's own recent
// check-in to the same agent (design 055 §7.6). SEQUENCE is exempt — the
// watch never sequences a deadlock.
func deconflict(f *fleet.Fleet, c candidate, now, gap int64) bool {
	if c.kind == KindSequence || c.kind == KindPull || c.escalatesTo == "parent" {
		return false
	}
	m := memberOf(f, c.to)
	if m == nil || m.Status == nil {
		return false
	}
	return m.Status.CheckinAt > 0 && now-m.Status.CheckinAt < gap
}

// budgetOK reports whether `to` may receive one more executor message now.
func budgetOK(mem *Memory, a Assessment, c candidate, to string, now int64, perCycle map[string]bool) bool {
	if perCycle[to] {
		return false
	}
	limit := MaxPerHour
	// A stressed agent gets one message an hour — and its SHIELD is that
	// message, whatever it was sent before it (found live: an earlier HURRY
	// used up the hour, and the SHIELD went only to the parent).
	if m := a.Member(to); m != nil && m.Has(CondStressed) && !(c.kind == KindShield && to == c.about) {
		limit = MaxPerHourStressed
	}
	return sentInHour(mem, to, now) < limit
}

func budgetOKFleet(mem *Memory, to string, now int64) bool {
	return sentInHour(mem, to, now) < MaxPerHour+1 // a fleet-level warning may exceed the agent budget by one
}

func sentInHour(mem *Memory, to string, now int64) int {
	cut := now - time.Hour.Milliseconds()
	keep := mem.Sent[to][:0]
	for _, t := range mem.Sent[to] {
		if t >= cut {
			keep = append(keep, t)
		}
	}
	mem.Sent[to] = keep
	return len(keep)
}

func spend(mem *Memory, to string, now int64, perCycle map[string]bool, kind string) {
	mem.Sent[to] = append(mem.Sent[to], now)
	if kind != KindRisk && kind != KindConfirm {
		perCycle[to] = true
	}
}

// cadence is the adaptive interval (design 055 §8).
func cadence(a Assessment, mem *Memory, p Plan, cfg fleet.GoalExec) int {
	if cfg.Mode == fleet.GoalExecFixed {
		mem.IntervalSec = cfg.MinSec
		return cfg.MinSec
	}
	calm := len(p.Actions) == 0 && len(p.Alerts) == 0 && p.Held == 0 &&
		(a.State == StateOnTrack || a.State == StateMet || a.State == StateNoGoal || a.State == StateStarting)
	for _, m := range a.Members {
		if m.Primary == CondStressed || m.Primary == CondStrayed || m.Primary == CondStalled || m.Primary == CondDeadlock {
			calm = false
		}
	}
	switch {
	case !calm || mem.IntervalSec == 0:
		mem.IntervalSec = cfg.MinSec
	default:
		mem.IntervalSec = min(mem.IntervalSec*2, cfg.MaxSec)
	}
	return mem.IntervalSec
}

// report renders the goal-report post, or "" when nothing changed since the
// last one and the last one is younger than the max cadence.
func report(a Assessment, f *fleet.Fleet, mem *Memory, p Plan, now int64, cfg fleet.GoalExec) string {
	counts := map[string]int{}
	for _, m := range a.Members {
		if m.Primary != "" {
			counts[m.Primary]++
		}
	}
	var parts []string
	for _, c := range ConditionOrder {
		if counts[c] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[c], c))
		}
	}
	pct := "pct ?"
	if a.Pct >= 0 {
		pct = fmt.Sprintf("%d%%", a.Pct)
	}
	pctBucket := a.Pct / 10
	fp := fmt.Sprintf("%s|%d|%s|%s", a.State, pctBucket, a.Bottleneck, strings.Join(parts, ","))
	if fp == mem.LastReport && now-mem.LastReportAt < int64(cfg.MaxSec)*1000 && len(p.Actions) == 0 && len(p.Alerts) == 0 {
		return ""
	}
	mem.LastReport, mem.LastReportAt = fp, now

	var b strings.Builder
	head := fmt.Sprintf("%s · %s", strings.ToUpper(a.State), pct)
	if a.Finish > 0 {
		head += " · finish ~" + clock(a.Finish)
		if a.Unknown > 0 {
			head += fmt.Sprintf(" (%d unknown)", a.Unknown)
		}
	}
	if a.DueAt > 0 {
		head += " · due " + clock(a.DueAt)
	}
	if a.Bottleneck != "" {
		if bn := a.Member(a.Bottleneck); bn != nil {
			head += fmt.Sprintf(" · bottleneck %s (%d waiting)", bn.Label, a.BottleneckWaiting)
		}
	}
	b.WriteString(head)
	if f.Goal != nil && f.Goal.Text != "" {
		fmt.Fprintf(&b, "\ngoal: %s", f.Goal.Text)
	}
	if len(parts) > 0 {
		fmt.Fprintf(&b, "\nagents: %s", strings.Join(parts, " · "))
	}
	if len(a.Why) > 0 {
		fmt.Fprintf(&b, "\nwhy: %s", strings.Join(a.Why, "; "))
	}
	if len(p.Actions) > 0 {
		var acts []string
		for _, x := range p.Actions {
			acts = append(acts, x.Kind+" → "+x.Label)
		}
		fmt.Fprintf(&b, "\nsent: %s", strings.Join(acts, ", "))
	}
	if p.Held > 0 {
		fmt.Fprintf(&b, "\nheld by budget: %d", p.Held)
	}
	if !a.BoardOK {
		b.WriteString("\nboard unreadable this cycle: pressure unknown")
	}
	return b.String()
}

// headsOf lists the fleet's heads: members whose parent is not a member.
func headsOf(f *fleet.Fleet) []string {
	var out []string
	for _, m := range f.Members {
		if m.Parent == "" || memberOf(f, m.Parent) == nil {
			out = append(out, m.PaneID)
		}
	}
	return out
}

func copyPreamble(a Assessment, c candidate, to string, level int) string {
	who := c.about
	if m := a.Member(c.about); m != nil {
		who = m.Label
	}
	return fmt.Sprintf("(copy, escalation level %d: about %s — the message it got follows)\n", level, who)
}

func ladderKey(c candidate) string { return c.kind + "|" + c.about + "|" + c.to }

func goalText(m *fleet.Member, f *fleet.Fleet) string {
	switch {
	case m != nil && m.Goal != nil && m.Goal.IsOpen():
		s := "Your goal: " + clip(m.Goal.Text, 120)
		if m.Goal.DoneWhen != "" {
			s += " (done when: " + clip(m.Goal.DoneWhen, 80) + ")"
		}
		if f.Goal != nil && f.Goal.IsOpen() {
			s += ". Fleet goal: " + clip(f.Goal.Text, 100)
		}
		return s
	case f.Goal != nil && f.Goal.IsOpen():
		return "Fleet goal: " + clip(f.Goal.Text, 120)
	}
	return ""
}

func labelOf(f *fleet.Fleet, id string) string { return fleet.MemberLabel(f, id) }

func labelsOf(f *fleet.Fleet, ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, labelOf(f, id))
	}
	return out
}

func workSince(f *fleet.Fleet, id string) int64 {
	if m := memberOf(f, id); m != nil && m.Status != nil {
		return m.Status.WorkSince
	}
	return 0
}

func signalNames(why []string) []string {
	out := make([]string, 0, len(why))
	for _, w := range why {
		switch {
		case strings.HasPrefix(w, "working on"):
			out = append(out, "not-converging")
		case strings.HasPrefix(w, "switched task"):
			out = append(out, "wandering")
		case strings.HasPrefix(w, "working on screen"):
			out = append(out, "undeclared")
		case strings.Contains(w, "past its own ETA"):
			out = append(out, "overrun")
		}
	}
	return out
}

func sortedCopy(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

func contains(s []string, x string) bool {
	for _, v := range s {
		if v == x {
			return true
		}
	}
	return false
}

func more(n int) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf(" and %d more behind them", n)
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
