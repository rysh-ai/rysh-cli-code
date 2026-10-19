// SPDX-License-Identifier: Apache-2.0

package fleet

// The status ledger (design 054): what every fleet member is doing, kept by the
// fleet actor and nobody else.
//
// WHY IT EXISTS. A fleet's agents talk reliably since designs 050/051, but
// nothing knew what any of them was DOING. Three failures recur in live fleets:
// agents sit idle with nobody noticing for hours; an agent's status lives only
// in the prose of its last report, so it cannot be queried; and "is it working,
// idle, or waiting on something?" has no answer short of reading its screen.
//
// TWO AXES, NEVER MERGED, and the separation is the design:
//
//   - WORK is DECLARED by the agent (`rysh fleet status <work>`): idle,
//     progress, blocked (by whom), done. Only the agent knows its intent.
//   - ACTIVITY is OBSERVED by the actor (the pane's screen): working or idle.
//     Only the outside can see what the agent is actually doing.
//
// HEALTH is what the actor derives from both, and it is where integrity comes
// from: an agent that says "progress" while its screen has been still for
// five minutes is STALLED, and neither axis alone could say so.
//
// BLOCKER_FOR IS DERIVED, NEVER DECLARED. An agent declares who blocks it; the
// ledger computes, for every member, whom it blocks. Two agents each keeping
// their half of one edge is exactly the shape that drifts — the blocker forgets
// to remove itself, or never learns it was added — and a derived index cannot
// disagree with the edges it is derived from.
//
// Pure like the rest of this package: every function takes `now` (unix
// millis) from its caller, so health and check-in timing are testable to the
// millisecond without a clock.

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Work states — declared by the agent.
const (
	WorkIdle     = "idle"     // no task: available for work
	WorkProgress = "progress" // working on Task
	WorkBlocked  = "blocked"  // cannot continue until every BlockedBy is resolved
	WorkDone     = "done"     // finished Task; waiting for the next one
)

// Activity — observed by the actor from the pane.
const (
	ActivityWorking = "working"
	ActivityIdle    = "idle"
	ActivityUnknown = "unknown" // the pane could not be read; never treated as idle
)

// Health — derived from work, activity and the blocked_by graph.
const (
	HealthOK           = "ok"
	HealthSilent       = "silent"       // never declared a status, idle, and past the check-in interval
	HealthStalled      = "stalled"      // says progress; idle on screen for >= the stall threshold
	HealthUnblocked    = "unblocked"    // says blocked; every blocker is resolved
	HealthDeadlock     = "deadlock"     // in a blocked_by cycle — nobody in it can move
	HealthAvailable    = "available"    // idle or done, and idle on screen: needs work
	HealthUnresponsive = "unresponsive" // ignored MaxMissedCheckins check-ins in a row
)

// Blocker refs that are not members.
const (
	// BlockerHuman is "waiting on the human". Common and legitimate: a founder
	// gate, a credential, a question only the human can answer.
	BlockerHuman = "human"
	// BlockerExternalPrefix marks anything else outside the fleet: "ext:ci",
	// "ext:D-14". Free text after the prefix, kept so the reason is visible.
	BlockerExternalPrefix = "ext:"
)

// Watch policies.
const (
	// PolicySmart (default) checks in only when something is wrong: a silent,
	// stalled or newly unblocked member. A member working with a fresh status
	// is never interrupted — every check-in costs a working agent a turn.
	PolicySmart = "smart"
	// PolicyAlways checks in with every member every interval.
	PolicyAlways = "always"
	// PolicyOff observes and derives health, and sends nothing.
	PolicyOff = "off"
)

// Defaults. The interval is the founder's "every 10 minutes or so".
const (
	DefaultCheckinIntervalSec = 600
	DefaultStallSec           = 300
	// MaxMissedCheckins is how many unanswered check-ins make a member
	// unresponsive and escalate to its parent.
	MaxMissedCheckins = 2
	// minIntervalSec bounds the interval from below: a check-in every few
	// seconds would keep every agent in the fleet permanently interrupted.
	minIntervalSec = 30
)

// Watch is a fleet's check-in policy. Zero values mean the defaults.
type Watch struct {
	Policy      string `json:"policy,omitempty"`
	IntervalSec int    `json:"interval_sec,omitempty"`
	StallSec    int    `json:"stall_sec,omitempty"`
	// Answer is who answers a status check-in: "agent" (default) or
	// "scout" — a cheap model reads the agent's screen and posts and answers
	// for it, and the agent is asked only when it cannot tell (design 056).
	Answer string `json:"answer,omitempty"`
	// ScoutModel is the scout's model (claude CLI name or alias); default haiku.
	ScoutModel string `json:"scout_model,omitempty"`
}

// Resolved returns the watch with every default filled in.
func (w *Watch) Resolved() Watch {
	out := Watch{Policy: PolicySmart, IntervalSec: DefaultCheckinIntervalSec, StallSec: DefaultStallSec,
		Answer: AnswerAgent, ScoutModel: DefaultScoutModel}
	if w == nil {
		return out
	}
	if w.Answer != "" {
		out.Answer = w.Answer
	}
	if w.ScoutModel != "" {
		out.ScoutModel = w.ScoutModel
	}
	if w.Policy != "" {
		out.Policy = w.Policy
	}
	if w.IntervalSec > 0 {
		out.IntervalSec = w.IntervalSec
	}
	if w.StallSec > 0 {
		out.StallSec = w.StallSec
	}
	return out
}

// Validate refuses a policy the actor cannot run.
func (w Watch) Validate() error {
	switch w.Policy {
	case "", PolicySmart, PolicyAlways, PolicyOff:
	default:
		return fmt.Errorf("unknown watch policy %q (want %s, %s or %s)", w.Policy, PolicySmart, PolicyAlways, PolicyOff)
	}
	if w.IntervalSec != 0 && w.IntervalSec < minIntervalSec {
		return fmt.Errorf("check-in interval %ds is below the %ds floor: every check-in costs a working agent a turn", w.IntervalSec, minIntervalSec)
	}
	if w.StallSec < 0 {
		return fmt.Errorf("stall threshold cannot be negative")
	}
	switch w.Answer {
	case "", AnswerAgent, AnswerScout:
	default:
		return fmt.Errorf("unknown --answer %q (want %s or %s)", w.Answer, AnswerAgent, AnswerScout)
	}
	if strings.ContainsAny(w.ScoutModel, " \t'\"") {
		return fmt.Errorf("--scout-model %q is not a model name", w.ScoutModel)
	}
	return nil
}

// MemberStatus is one member's row in the ledger.
type MemberStatus struct {
	// Declared.
	Work      string   `json:"work,omitempty"`
	Task      string   `json:"task,omitempty"`
	Note      string   `json:"note,omitempty"`
	BlockedBy []string `json:"blocked_by,omitempty"` // member pane ids, "human", or "ext:<what>"
	UpdatedAt int64    `json:"updated_at,omitempty"` // last declaration
	WorkSince int64    `json:"work_since,omitempty"` // when Work last changed

	// Derived from every member's BlockedBy. Never declared.
	BlockerFor []string `json:"blocker_for,omitempty"`

	// Observed.
	Activity   string `json:"activity,omitempty"`
	ActiveAt   int64  `json:"active_at,omitempty"`   // last time seen working
	ObservedAt int64  `json:"observed_at,omitempty"` // last observation
	JoinedAt   int64  `json:"joined_at,omitempty"`   // first time the ledger saw this member

	// Derived.
	Health      string `json:"health,omitempty"`
	HealthSince int64  `json:"health_since,omitempty"`

	// Check-in bookkeeping.
	CheckinAt     int64  `json:"checkin_at,omitempty"`     // last check-in sent
	CheckinCmd    string `json:"checkin_cmd,omitempty"`    // that check-in's cmd id, until answered
	CheckinMissed int    `json:"checkin_missed,omitempty"` // consecutive unanswered

	// Progress toward the CURRENT task (design 055), declared with
	// `--pct` / `--eta`. A new task text resets both: they described the old
	// one. Pct is a pointer because 0% is a real answer and "never said" is
	// a different one.
	Pct      *int  `json:"pct,omitempty"`
	EtaAt    int64 `json:"eta_at,omitempty"`     // unix millis the agent expects to finish
	EtaSetAt int64 `json:"eta_set_at,omitempty"` // when that estimate was given

	// The goal executor's pending ask (HURRY / REALIGN): answered, like a
	// check-in, by the member's next declaration.
	GoalAskAt  int64  `json:"goal_ask_at,omitempty"`
	GoalAskCmd string `json:"goal_ask_cmd,omitempty"`

	// The status scout (design 056): who made the last declaration ("" the
	// agent, "scout"), and the verdict the scout last answered — it answers
	// a verdict once; if it stands, the agent is asked.
	DeclaredBy  string `json:"declared_by,omitempty"`
	ScoutAt     int64  `json:"scout_at,omitempty"`
	ScoutHealth string `json:"scout_health,omitempty"`
}

// StatusUpdate is a declaration from an agent.
type StatusUpdate struct {
	Work      string   `json:"work"`
	Task      string   `json:"task,omitempty"`
	Note      string   `json:"note,omitempty"`
	BlockedBy []string `json:"blocked_by,omitempty"` // refs: pane id, id prefix, label, "human", "ext:<what>"
	// KeepTask leaves Task as it was when Task is empty, so `status blocked
	// --blocked-by mgr` does not erase what the agent is blocked ON.
	KeepTask bool `json:"keep_task,omitempty"`

	// Pct (0–100) and EtaSec (seconds from now) are progress toward the
	// task (design 055). Optional; nil / 0 leave what was declared before,
	// unless the task text changes.
	Pct    *int `json:"pct,omitempty"`
	EtaSec int  `json:"eta_sec,omitempty"`

	// By is who declares: "" the agent itself, "scout" the status scout on
	// its behalf (design 056). A scout's declaration answers nothing that was
	// asked of the agent: its pending check-in and the executor's ask stay
	// pending, so an agent never loses a question it was meant to answer.
	By string `json:"by,omitempty"`
}

// StatusResult is what a declaration did.
type StatusResult struct {
	Fleet  string
	Member *Member // a copy, as stored
	// AnsweredCheckin is the watch's check-in this declaration answered.
	AnsweredCheckin string
	// AnsweredGoalAsk is the goal executor's ask it answered.
	AnsweredGoalAsk string
}

// ValidWork reports whether w is a declarable work state.
func ValidWork(w string) bool {
	switch w {
	case WorkIdle, WorkProgress, WorkBlocked, WorkDone:
		return true
	}
	return false
}

// ResolveMember finds a member of f by pane id, pane id prefix (>= 4 chars),
// label, or label prefix — the addressing fleetctl already accepts. Ambiguity
// is an error, never a guess: a blocker attributed to the wrong agent is a
// wrong edge in the graph every nudge is derived from.
func ResolveMember(f *Fleet, ref string) (*Member, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, fmt.Errorf("empty member reference")
	}
	for i := range f.Members {
		if f.Members[i].PaneID == ref || f.Members[i].Label == ref {
			return &f.Members[i], nil
		}
	}
	var hits []*Member
	for i := range f.Members {
		m := &f.Members[i]
		if (len(ref) >= 4 && strings.HasPrefix(m.PaneID, ref)) ||
			(m.Label != "" && strings.HasPrefix(m.Label, ref)) {
			hits = append(hits, m)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return nil, fmt.Errorf("fleet %q has no member %q", f.Name, ref)
	}
	names := make([]string, 0, len(hits))
	for _, m := range hits {
		names = append(names, m.Label)
	}
	return nil, fmt.Errorf("%q is ambiguous in fleet %q: %s", ref, f.Name, strings.Join(names, ", "))
}

// SetStatus records a declaration from the member with paneID. fleetName may
// be empty: the pane's own fleet is used. Returns the fleet name, a copy of
// the member as stored, and the id of the check-in this declaration answered
// ("" if none was pending) — the actor files the status on that check-in's
// thread, so the board shows the check-in as answered.
//
// It REFUSES rather than repairs: blocked with no blocker, a blocker on a
// non-blocked state, self-blocking, an unknown blocker. Each of those, stored,
// is an edge in the graph that is wrong — and every nudge, escalation and
// deadlock verdict is derived from that graph.
func (r *Registry) SetStatus(fleetName, paneID string, u StatusUpdate, now int64) (string, *Member, string, error) {
	res, err := r.Declare(fleetName, paneID, u, now)
	if err != nil {
		return "", nil, "", err
	}
	return res.Fleet, res.Member, res.AnsweredCheckin, nil
}

// Declare is SetStatus with everything a declaration did: it also reports the
// goal executor's ask it answered (design 055).
func (r *Registry) Declare(fleetName, paneID string, u StatusUpdate, now int64) (StatusResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	f, m, err := r.memberLocked(fleetName, paneID)
	if err != nil {
		return StatusResult{}, err
	}
	u.Work = strings.TrimSpace(strings.ToLower(u.Work))
	if !ValidWork(u.Work) {
		return StatusResult{}, fmt.Errorf("unknown work state %q (want %s, %s, %s or %s)",
			u.Work, WorkIdle, WorkProgress, WorkBlocked, WorkDone)
	}
	if u.Pct != nil && (*u.Pct < 0 || *u.Pct > 100) {
		return StatusResult{}, fmt.Errorf("--pct %d is not a percentage (0–100)", *u.Pct)
	}
	if u.EtaSec < 0 {
		return StatusResult{}, fmt.Errorf("--eta cannot be negative")
	}

	var blockers []string
	seen := map[string]bool{}
	for _, raw := range u.BlockedBy {
		for _, ref := range strings.Split(raw, ",") {
			ref = strings.TrimSpace(ref)
			if ref == "" {
				continue
			}
			id, err := resolveBlocker(f, ref)
			if err != nil {
				return StatusResult{}, err
			}
			if id == m.PaneID {
				return StatusResult{}, fmt.Errorf("a member cannot block itself")
			}
			if !seen[id] {
				seen[id] = true
				blockers = append(blockers, id)
			}
		}
	}
	switch {
	case u.Work == WorkBlocked && len(blockers) == 0:
		return StatusResult{}, fmt.Errorf("blocked needs --blocked-by <who> (a member, %s, or %s<what>)",
			BlockerHuman, BlockerExternalPrefix)
	case u.Work != WorkBlocked && len(blockers) > 0:
		return StatusResult{}, fmt.Errorf("--blocked-by only goes with %q; you said %q", WorkBlocked, u.Work)
	}

	st := m.Status
	if st == nil {
		st = &MemberStatus{JoinedAt: now}
		m.Status = st
	}
	if st.Work != u.Work {
		st.WorkSince = now
	}
	st.Work = u.Work
	oldTask := st.Task
	if strings.TrimSpace(u.Task) != "" || !u.KeepTask {
		st.Task = strings.TrimSpace(u.Task)
	}
	// Progress describes a task: a different task starts from nothing known.
	if st.Task != oldTask {
		st.Pct, st.EtaAt, st.EtaSetAt = nil, 0, 0
	}
	if u.Pct != nil {
		p := *u.Pct
		st.Pct = &p
	}
	switch {
	case u.EtaSec > 0:
		st.EtaAt, st.EtaSetAt = now+int64(u.EtaSec)*1000, now
	case u.Work == WorkIdle:
		st.EtaAt, st.EtaSetAt = 0, 0 // nothing to finish
	}
	st.Note = strings.TrimSpace(u.Note)
	st.BlockedBy = blockers
	// UpdatedAt is the AGENT's last word. A scout's declaration must not
	// reset it: the stall rule reads it, and a scout that kept answering
	// "progress" for an idle pane would otherwise hide a real stall forever.
	if u.By == "" {
		st.UpdatedAt = now
	}
	// A declaration answers any pending check-in, and the goal executor's
	// pending ask: both asked "what is your status?". The scout's does not —
	// those were asked of the agent.
	res := StatusResult{Fleet: f.Name}
	st.DeclaredBy = u.By
	if u.By == "" {
		res.AnsweredCheckin, res.AnsweredGoalAsk = st.CheckinCmd, st.GoalAskCmd
		st.CheckinCmd, st.CheckinMissed = "", 0
		st.GoalAskCmd = ""
	}

	deriveBlockerFor(f)
	out := *m
	out.Status = copyStatus(m.Status)
	out.Goal = copyGoal(m.Goal)
	res.Member = &out
	return res, nil
}

// resolveBlocker maps a blocker ref to what is stored: a member's pane id,
// "human", or "ext:<what>".
func resolveBlocker(f *Fleet, ref string) (string, error) {
	switch {
	case strings.EqualFold(ref, BlockerHuman):
		return BlockerHuman, nil
	case strings.HasPrefix(ref, BlockerExternalPrefix):
		if strings.TrimSpace(strings.TrimPrefix(ref, BlockerExternalPrefix)) == "" {
			return "", fmt.Errorf("%q needs a name after the prefix, e.g. ext:ci", ref)
		}
		return ref, nil
	}
	m, err := ResolveMember(f, ref)
	if err != nil {
		return "", fmt.Errorf("blocked-by: %w (use %s or %s<what> for a blocker outside the fleet)",
			err, BlockerHuman, BlockerExternalPrefix)
	}
	return m.PaneID, nil
}

// memberLocked finds a member by pane id, in the named fleet or in whichever
// fleet holds the pane. Caller holds r.mu.
func (r *Registry) memberLocked(fleetName, paneID string) (*Fleet, *Member, error) {
	if strings.TrimSpace(paneID) == "" {
		return nil, nil, fmt.Errorf("a status needs the member's pane id (--as $RYSH_PANE)")
	}
	var f *Fleet
	if fleetName != "" {
		f = r.fleets[msg.NormalizeBoardID(fleetName)]
		if f == nil {
			return nil, nil, fmt.Errorf("no fleet named %q", fleetName)
		}
	} else {
		for _, cand := range r.fleets {
			for i := range cand.Members {
				if cand.Members[i].PaneID == paneID {
					f = cand
				}
			}
		}
		if f == nil {
			return nil, nil, fmt.Errorf("pane %s is not a member of any fleet", shortPane(paneID))
		}
	}
	for i := range f.Members {
		if f.Members[i].PaneID == paneID {
			return f, &f.Members[i], nil
		}
	}
	return nil, nil, fmt.Errorf("pane %s is not a member of fleet %q", shortPane(paneID), f.Name)
}

// deriveBlockerFor recomputes every member's BlockerFor from every member's
// BlockedBy. Only member blockers have a row to write to; "human" and "ext:"
// do not.
func deriveBlockerFor(f *Fleet) {
	for_ := map[string][]string{}
	for _, m := range f.Members {
		if m.Status == nil || m.Status.Work != WorkBlocked {
			continue
		}
		for _, b := range m.Status.BlockedBy {
			for_[b] = append(for_[b], m.PaneID)
		}
	}
	for i := range f.Members {
		m := &f.Members[i]
		list := for_[m.PaneID]
		sort.Strings(list)
		if m.Status == nil {
			if len(list) == 0 {
				continue
			}
			m.Status = &MemberStatus{}
		}
		m.Status.BlockerFor = list
	}
}

// Observe records what the actor saw in a member's pane. Unknown is recorded
// as the observation time only: it says nothing about whether the agent is
// working, and must not move ActiveAt or turn an agent idle.
func (r *Registry) Observe(paneID, activity string, now int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, f := range r.fleets {
		for i := range f.Members {
			m := &f.Members[i]
			if m.PaneID != paneID {
				continue
			}
			if m.Status == nil {
				m.Status = &MemberStatus{JoinedAt: now}
			}
			st := m.Status
			if st.JoinedAt == 0 {
				st.JoinedAt = now
			}
			st.ObservedAt = now
			st.Activity = activity
			if activity == ActivityWorking {
				st.ActiveAt = now
			}
		}
	}
}

// HealthChange is one member whose health moved.
type HealthChange struct {
	Fleet  string
	Member Member // a copy, status included
	From   string
	To     string
	// Cycle is set on a deadlock: the pane ids in the cycle, in order.
	Cycle []string
}

// Evaluate derives every member's health and returns the members whose health
// changed. The actor posts alerts for changes only, so a member that stays
// stalled is reported once, not every sweep.
func (r *Registry) Evaluate(now int64) []HealthChange {
	r.mu.Lock()
	defer r.mu.Unlock()

	var changes []HealthChange
	names := make([]string, 0, len(r.fleets))
	for n := range r.fleets {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		f := r.fleets[name]
		w := f.Watch.Resolved()
		deriveBlockerFor(f)
		cycles := blockedCycles(f)
		for i := range f.Members {
			m := &f.Members[i]
			if m.Status == nil {
				m.Status = &MemberStatus{JoinedAt: now}
			}
			h := memberHealth(f, m, w, cycles[m.PaneID] != nil, now)
			st := m.Status
			if h == st.Health {
				continue
			}
			ch := HealthChange{Fleet: f.Name, From: st.Health, To: h, Cycle: cycles[m.PaneID]}
			st.Health, st.HealthSince = h, now
			ch.Member = *m
			ch.Member.Status = copyStatus(st)
			changes = append(changes, ch)
		}
	}
	return changes
}

// memberHealth is the derivation, in priority order: the most actionable
// verdict wins.
func memberHealth(f *Fleet, m *Member, w Watch, inCycle bool, now int64) string {
	st := m.Status
	idle := st.Activity == ActivityIdle
	stall := int64(w.StallSec) * 1000
	interval := int64(w.IntervalSec) * 1000

	switch {
	case st.ObservedAt == 0 && st.UpdatedAt == 0:
		return "" // nothing seen and nothing said: no basis for a verdict
	case inCycle:
		return HealthDeadlock
	case st.CheckinMissed >= MaxMissedCheckins:
		return HealthUnresponsive
	case st.Work == WorkBlocked && blockersResolved(f, st.BlockedBy):
		return HealthUnblocked
	case st.Work == WorkProgress && idle &&
		now-lastNonZero(st.ActiveAt, st.JoinedAt) >= stall && now-st.UpdatedAt >= stall:
		return HealthStalled
	case (st.Work == WorkIdle || st.Work == WorkDone) && idle:
		return HealthAvailable
	case st.Work == "" && idle && now-st.JoinedAt >= interval:
		// Never declared anything and doing nothing. A member that DID
		// declare is judged by what it declared: blocked-and-idle is waiting
		// as it said (found live: a worker correctly blocked on a busy
		// blocker was flagged silent and pinged every interval).
		return HealthSilent
	}
	return HealthOK
}

func lastNonZero(a, b int64) int64 {
	if a != 0 {
		return a
	}
	return b
}

// blockersResolved reports whether every blocker of a member is done, idle or
// gone. "human" and "ext:" blockers are never resolved by the ledger — only
// the agent can say the human answered.
func blockersResolved(f *Fleet, blockers []string) bool {
	if len(blockers) == 0 {
		return false
	}
	for _, b := range blockers {
		if b == BlockerHuman || strings.HasPrefix(b, BlockerExternalPrefix) {
			return false
		}
		var bm *Member
		for i := range f.Members {
			if f.Members[i].PaneID == b {
				bm = &f.Members[i]
			}
		}
		if bm == nil {
			continue // the blocker's pane is gone: it will never unblock anyone
		}
		if bm.Status == nil || (bm.Status.Work != WorkDone && bm.Status.Work != WorkIdle) {
			return false
		}
	}
	return true
}

// blockedCycles finds every member that sits on a cycle of blocked_by edges,
// mapped to the cycle (pane ids, in edge order). Only members that are blocked
// carry edges, so an agent that unblocks itself breaks the cycle.
func blockedCycles(f *Fleet) map[string][]string {
	edges := map[string][]string{}
	for _, m := range f.Members {
		if m.Status != nil && m.Status.Work == WorkBlocked {
			edges[m.PaneID] = m.Status.BlockedBy
		}
	}
	out := map[string][]string{}
	const (
		white = iota
		grey
		black
	)
	colour := map[string]int{}
	var stack []string
	var visit func(n string)
	visit = func(n string) {
		colour[n] = grey
		stack = append(stack, n)
		for _, next := range edges[n] {
			switch colour[next] {
			case white:
				visit(next)
			case grey:
				// Back edge: the cycle is the stack from next to the top.
				for i := len(stack) - 1; i >= 0; i-- {
					if stack[i] == next {
						cyc := append([]string(nil), stack[i:]...)
						for _, id := range cyc {
							if out[id] == nil {
								out[id] = cyc
							}
						}
						break
					}
				}
			}
		}
		stack = stack[:len(stack)-1]
		colour[n] = black
	}
	keys := make([]string, 0, len(edges))
	for k := range edges {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if colour[k] == white {
			visit(k)
		}
	}
	return out
}

// Checkin is one check-in the actor should send.
type Checkin struct {
	Fleet   string
	BoardID string
	Member  Member // a copy, status included
	Reason  string // the member's health, or "scheduled" under PolicyAlways
	Missed  int    // unanswered check-ins before this one
}

// DueCheckins returns the check-ins to send now, and stamps them as sent: the
// caller sends each and then calls CheckinSent with the cmd id it minted.
//
// Rate-limited per member: at most one per interval — except that a member
// whose health CHANGED since its last check-in into stalled or unblocked is
// asked at once, because those are the two verdicts it can act on now. (Found
// live: re-asking every stall window counted two "misses" in a minute.) A
// member working with a fresh status is never pinged under the smart policy.
func (r *Registry) DueCheckins(now int64) []Checkin {
	r.mu.Lock()
	defer r.mu.Unlock()

	var out []Checkin
	names := make([]string, 0, len(r.fleets))
	for n := range r.fleets {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		f := r.fleets[name]
		if f.State != StateUp {
			continue // a registered or down fleet has nobody to ask
		}
		w := f.Watch.Resolved()
		if w.Policy == PolicyOff {
			continue
		}
		for i := range f.Members {
			m := &f.Members[i]
			st := m.Status
			if st == nil {
				continue // not observed yet: no basis for a verdict
			}
			reason := ""
			gap := int64(w.IntervalSec) * 1000
			switch {
			case w.Policy == PolicyAlways:
				reason = "scheduled"
			case st.Health == HealthSilent, st.Health == HealthUnresponsive:
				reason = st.Health
			case st.Health == HealthStalled, st.Health == HealthUnblocked:
				reason = st.Health
				if st.HealthSince > st.CheckinAt {
					gap = 0 // a new verdict: ask now
				}
			}
			if reason == "" || now-st.CheckinAt < gap {
				continue
			}
			c := Checkin{Fleet: f.Name, BoardID: f.BoardID, Reason: reason, Missed: st.CheckinMissed}
			if st.CheckinCmd != "" {
				// The previous check-in was never answered.
				st.CheckinMissed++
				c.Missed = st.CheckinMissed
			}
			st.CheckinAt = now
			c.Member = *m
			c.Member.Status = copyStatus(st)
			out = append(out, c)
		}
	}
	return out
}

// CheckinSent records the cmd id of a check-in just sent, so the next status
// declaration can answer it and an unanswered one can be counted.
func (r *Registry) CheckinSent(fleetName, paneID, cmdID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, m, err := r.memberLocked(fleetName, paneID); err == nil && m.Status != nil {
		m.Status.CheckinCmd = cmdID
	}
}

// SetWatch sets a fleet's check-in policy.
func (r *Registry) SetWatch(name string, w Watch) (*Fleet, error) {
	if err := w.Validate(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	f := r.fleets[msg.NormalizeBoardID(name)]
	if f == nil {
		return nil, fmt.Errorf("no fleet named %q", name)
	}
	cp := w
	f.Watch = &cp
	return copyFleet(f), nil
}

// MemberLabel is the display name of a pane in f: its label, else pane-<8>.
func MemberLabel(f *Fleet, paneID string) string {
	switch {
	case paneID == BlockerHuman, strings.HasPrefix(paneID, BlockerExternalPrefix):
		return paneID
	}
	for _, m := range f.Members {
		if m.PaneID == paneID && m.Label != "" {
			return m.Label
		}
	}
	return "pane-" + shortPane(paneID)
}

func shortPane(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func copyStatus(s *MemberStatus) *MemberStatus {
	if s == nil {
		return nil
	}
	out := *s
	out.BlockedBy = append([]string(nil), s.BlockedBy...)
	out.BlockerFor = append([]string(nil), s.BlockerFor...)
	if s.Pct != nil {
		p := *s.Pct
		out.Pct = &p
	}
	return &out
}

// IntervalText renders the check-in interval for a message: 10m0s → "10m",
// 1h0m0s → "1h", 90s → "1m30s", 20s → "20s".
func (w Watch) IntervalText() string {
	s := (time.Duration(w.IntervalSec) * time.Second).String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
