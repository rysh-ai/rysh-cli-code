// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"reflect"
	"strings"
	"testing"
)

const (
	pLead = "aaaaaaaa-0000-4000-8000-000000000001"
	pMgr  = "bbbbbbbb-0000-4000-8000-000000000002"
	pW1   = "cccccccc-0000-4000-8000-000000000003"
	pW2   = "dddddddd-0000-4000-8000-000000000004"
)

const min = int64(60_000)

// ledger is a fleet "f" that is up, with a lead, a manager and two workers.
func ledger(t *testing.T) *Registry {
	t.Helper()
	r := New()
	if _, err := r.Register(Fleet{Name: "f"}); err != nil {
		t.Fatal(err)
	}
	if err := r.SetState("f", StateUp); err != nil {
		t.Fatal(err)
	}
	for _, m := range []Member{
		{PaneID: pLead, Label: "lead", Role: "ceo"},
		{PaneID: pMgr, Label: "mgr-01", Role: "manager", Parent: pLead},
		{PaneID: pW1, Label: "wkr-01", Role: "worker", Parent: pMgr},
		{PaneID: pW2, Label: "wkr-02", Role: "worker", Parent: pMgr},
	} {
		if err := r.UpsertMember("f", m); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func declare(t *testing.T, r *Registry, pane string, u StatusUpdate, now int64) *Member {
	t.Helper()
	_, m, _, err := r.SetStatus("", pane, u, now)
	if err != nil {
		t.Fatalf("SetStatus(%s, %+v): %v", pane, u, err)
	}
	return m
}

func member(r *Registry, pane string) Member {
	for _, m := range r.Get("f").Members {
		if m.PaneID == pane {
			return m
		}
	}
	return Member{}
}

func TestSetStatusRefusals(t *testing.T) {
	r := ledger(t)
	cases := []struct {
		name string
		u    StatusUpdate
		want string
	}{
		{"unknown work", StatusUpdate{Work: "working"}, "unknown work state"},
		{"blocked with nobody", StatusUpdate{Work: WorkBlocked}, "needs --blocked-by"},
		{"blocker on progress", StatusUpdate{Work: WorkProgress, BlockedBy: []string{"mgr-01"}}, "only goes with"},
		{"self", StatusUpdate{Work: WorkBlocked, BlockedBy: []string{"wkr-01"}}, "cannot block itself"},
		{"unknown blocker", StatusUpdate{Work: WorkBlocked, BlockedBy: []string{"nobody"}}, "no member"},
		{"ambiguous prefix", StatusUpdate{Work: WorkBlocked, BlockedBy: []string{"wkr"}}, "ambiguous"},
		{"empty ext", StatusUpdate{Work: WorkBlocked, BlockedBy: []string{"ext:"}}, "needs a name"},
	}
	for _, c := range cases {
		if _, _, _, err := r.SetStatus("", pW1, c.u, 1); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to mention %q", c.name, err, c.want)
		}
	}
	if _, _, _, err := r.SetStatus("", "not-a-member", StatusUpdate{Work: WorkIdle}, 1); err == nil {
		t.Error("a pane in no fleet was allowed to declare")
	}
	if _, _, _, err := r.SetStatus("other", pW1, StatusUpdate{Work: WorkIdle}, 1); err == nil {
		t.Error("a declaration naming the wrong fleet was accepted")
	}
}

func TestBlockerForIsDerived(t *testing.T) {
	r := ledger(t)
	// Both workers blocked on the manager; w2 also on the human; ref forms:
	// label, id prefix, comma list.
	declare(t, r, pW1, StatusUpdate{Work: WorkBlocked, Task: "parser", BlockedBy: []string{"mgr-01"}}, 1)
	m := declare(t, r, pW2, StatusUpdate{Work: WorkBlocked, BlockedBy: []string{"bbbbbbbb,human", "ext:ci"}}, 2)
	if want := []string{pMgr, BlockerHuman, "ext:ci"}; !reflect.DeepEqual(m.Status.BlockedBy, want) {
		t.Fatalf("w2 blocked_by = %v, want %v", m.Status.BlockedBy, want)
	}
	if got := member(r, pMgr).Status.BlockerFor; !reflect.DeepEqual(got, []string{pW1, pW2}) {
		t.Fatalf("mgr blocker_for = %v, want both workers", got)
	}
	// w1 moves on: the manager stops blocking it, without the manager doing
	// anything — the edge has one owner.
	declare(t, r, pW1, StatusUpdate{Work: WorkProgress, Task: "parser"}, 3)
	if got := member(r, pMgr).Status.BlockerFor; !reflect.DeepEqual(got, []string{pW2}) {
		t.Fatalf("after w1 unblocks, mgr blocker_for = %v, want only w2", got)
	}
	if got := member(r, pW1).Status.BlockedBy; len(got) != 0 {
		t.Fatalf("w1 still blocked_by %v after declaring progress", got)
	}
}

func TestBlockedKeepsItsTask(t *testing.T) {
	r := ledger(t)
	declare(t, r, pW1, StatusUpdate{Work: WorkProgress, Task: "parser"}, 1)
	m := declare(t, r, pW1, StatusUpdate{Work: WorkBlocked, BlockedBy: []string{"mgr-01"}, KeepTask: true}, 2)
	if m.Status.Task != "parser" {
		t.Fatalf("task = %q, a blocked declaration with no task must keep it", m.Status.Task)
	}
	if m.Status.WorkSince != 2 {
		t.Fatalf("work_since = %d, want 2", m.Status.WorkSince)
	}
}

func healthOf(r *Registry, pane string) string {
	if st := member(r, pane).Status; st != nil {
		return st.Health
	}
	return ""
}

func TestEvaluateHealth(t *testing.T) {
	r := ledger(t)
	t0 := int64(1_000_000)
	for _, p := range []string{pLead, pMgr, pW1, pW2} {
		r.Observe(p, ActivityWorking, t0)
	}
	declare(t, r, pLead, StatusUpdate{Work: WorkProgress, Task: "drive"}, t0)
	declare(t, r, pMgr, StatusUpdate{Work: WorkProgress, Task: "plan"}, t0)
	declare(t, r, pW1, StatusUpdate{Work: WorkProgress, Task: "parser"}, t0)
	declare(t, r, pW2, StatusUpdate{Work: WorkDone, Task: "lexer"}, t0)
	r.Evaluate(t0)
	for _, p := range []string{pLead, pMgr, pW1, pW2} {
		if h := healthOf(r, p); h != HealthOK {
			t.Fatalf("%s health = %q at t0 with everyone working", p, h)
		}
	}

	// w1 goes still: not stalled at 4 minutes, stalled at 5.
	r.Observe(pW1, ActivityIdle, t0+4*min)
	r.Evaluate(t0 + 4*min)
	if h := healthOf(r, pW1); h != HealthOK {
		t.Fatalf("w1 stalled too early: %q", h)
	}
	r.Observe(pW1, ActivityIdle, t0+5*min)
	ch := r.Evaluate(t0 + 5*min)
	if h := healthOf(r, pW1); h != HealthStalled {
		t.Fatalf("w1 health = %q after 5 idle minutes declaring progress, want stalled", h)
	}
	if len(ch) != 1 || ch[0].To != HealthStalled {
		t.Fatalf("changes = %+v, want exactly w1 → stalled", ch)
	}
	// A second evaluation reports nothing: changes, not states.
	if ch := r.Evaluate(t0 + 6*min); len(ch) != 0 {
		t.Fatalf("re-evaluation reported %+v; a stalled member must be reported once", ch)
	}

	// w2 is done and idle: available.
	r.Observe(pW2, ActivityIdle, t0+6*min)
	r.Evaluate(t0 + 6*min)
	if h := healthOf(r, pW2); h != HealthAvailable {
		t.Fatalf("w2 health = %q, want available", h)
	}

	// Unknown activity never makes anyone stalled or idle.
	r.Observe(pMgr, ActivityUnknown, t0+30*min)
	r.Evaluate(t0 + 30*min)
	if h := healthOf(r, pMgr); h != HealthOK {
		t.Fatalf("mgr health = %q on an unreadable pane, want ok", h)
	}

	// The lead never re-declares and goes still past the interval while
	// declaring progress: stalled wins over silent (more actionable).
	r.Observe(pLead, ActivityIdle, t0+31*min)
	r.Evaluate(t0 + 31*min)
	if h := healthOf(r, pLead); h != HealthStalled {
		t.Fatalf("lead health = %q, want stalled", h)
	}
}

func TestSilentNeedsIdleAndAnInterval(t *testing.T) {
	r := ledger(t)
	t0 := int64(5_000_000)
	r.Observe(pW1, ActivityIdle, t0) // joins, never declares
	r.Evaluate(t0 + 9*min)
	if h := healthOf(r, pW1); h != HealthOK {
		t.Fatalf("silent before the interval: %q", h)
	}
	r.Observe(pW1, ActivityIdle, t0+10*min)
	r.Evaluate(t0 + 10*min)
	if h := healthOf(r, pW1); h != HealthSilent {
		t.Fatalf("health = %q after 10 undeclared idle minutes, want silent", h)
	}
	// Declared blocked and idle is waiting as it said, not silent.
	declare(t, r, pMgr, StatusUpdate{Work: WorkBlocked, BlockedBy: []string{"human"}}, t0)
	r.Observe(pMgr, ActivityIdle, t0+30*min)
	r.Evaluate(t0 + 30*min)
	if h := healthOf(r, pMgr); h != HealthOK {
		t.Fatalf("a member blocked on the human for 30 idle minutes is %q, want ok", h)
	}
	// Working but undeclared is not silent: it is not interrupted.
	r.Observe(pW2, ActivityWorking, t0)
	r.Observe(pW2, ActivityWorking, t0+20*min)
	r.Evaluate(t0 + 20*min)
	if h := healthOf(r, pW2); h != HealthOK {
		t.Fatalf("a working undeclared member is %q, want ok", h)
	}
}

func TestUnblockedWhenEveryBlockerIsDone(t *testing.T) {
	r := ledger(t)
	declare(t, r, pW1, StatusUpdate{Work: WorkBlocked, Task: "parser", BlockedBy: []string{"mgr-01", "wkr-02"}}, 1)
	declare(t, r, pMgr, StatusUpdate{Work: WorkDone}, 2)
	r.Evaluate(3)
	if h := healthOf(r, pW1); h == HealthUnblocked {
		t.Fatal("unblocked while wkr-02 has not finished")
	}
	declare(t, r, pW2, StatusUpdate{Work: WorkDone}, 4)
	r.Evaluate(5)
	if h := healthOf(r, pW1); h != HealthUnblocked {
		t.Fatalf("w1 health = %q with every blocker done, want unblocked", h)
	}
	// A human blocker is never resolved by the ledger.
	declare(t, r, pW2, StatusUpdate{Work: WorkBlocked, BlockedBy: []string{"human"}}, 6)
	r.Evaluate(7)
	if h := healthOf(r, pW2); h == HealthUnblocked {
		t.Fatal("a human blocker was treated as resolved")
	}
	// A blocker whose pane is gone will never answer: resolved.
	declare(t, r, pLead, StatusUpdate{Work: WorkBlocked, BlockedBy: []string{"wkr-01"}}, 8)
	r.Reconcile(map[string]bool{pLead: true, pMgr: true, pW2: true})
	r.Evaluate(9)
	if h := healthOf(r, pLead); h != HealthUnblocked {
		t.Fatalf("lead health = %q once its blocker's pane is gone, want unblocked", h)
	}
}

func TestDeadlockIsACycle(t *testing.T) {
	r := ledger(t)
	declare(t, r, pW1, StatusUpdate{Work: WorkBlocked, BlockedBy: []string{"wkr-02"}}, 1)
	declare(t, r, pW2, StatusUpdate{Work: WorkBlocked, BlockedBy: []string{"mgr-01"}}, 2)
	declare(t, r, pLead, StatusUpdate{Work: WorkBlocked, BlockedBy: []string{"wkr-01"}}, 2)
	r.Evaluate(3)
	if h := healthOf(r, pW1); h == HealthDeadlock {
		t.Fatal("deadlock reported for a chain with no cycle")
	}
	declare(t, r, pMgr, StatusUpdate{Work: WorkBlocked, BlockedBy: []string{"wkr-01"}}, 4)
	ch := r.Evaluate(5)
	for _, p := range []string{pMgr, pW1, pW2} {
		if h := healthOf(r, p); h != HealthDeadlock {
			t.Fatalf("%s health = %q in the cycle mgr→w1→w2→mgr, want deadlock", p, h)
		}
	}
	if h := healthOf(r, pLead); h == HealthDeadlock {
		t.Fatal("lead waits ON the cycle but is not in it; it must not be called deadlocked")
	}
	var cyc []string
	for _, c := range ch {
		if c.To == HealthDeadlock {
			cyc = c.Cycle
		}
	}
	if len(cyc) != 3 {
		t.Fatalf("cycle = %v, want the three members", cyc)
	}
	// One member breaks out: the cycle is gone.
	declare(t, r, pW2, StatusUpdate{Work: WorkProgress, Task: "workaround"}, 6)
	r.Evaluate(7)
	if h := healthOf(r, pMgr); h == HealthDeadlock {
		t.Fatal("deadlock persisted after the cycle was broken")
	}
}

func TestDueCheckinsSmart(t *testing.T) {
	r := ledger(t)
	t0 := int64(10_000_000)
	for _, p := range []string{pLead, pMgr, pW1, pW2} {
		r.Observe(p, ActivityWorking, t0)
		declare(t, r, p, StatusUpdate{Work: WorkProgress, Task: "x"}, t0)
	}
	r.Evaluate(t0)
	if due := r.DueCheckins(t0 + 20*min); len(due) != 0 {
		t.Fatalf("smart policy pinged %d working members with fresh status", len(due))
	}
	// w1 stalls: it is checked in with, once per stall window.
	r.Observe(pW1, ActivityIdle, t0+6*min)
	r.Evaluate(t0 + 6*min)
	due := r.DueCheckins(t0 + 6*min)
	if len(due) != 1 || due[0].Member.PaneID != pW1 || due[0].Reason != HealthStalled {
		t.Fatalf("due = %+v, want one stalled check-in to w1", due)
	}
	r.CheckinSent("f", pW1, "ci-1")
	r.Evaluate(t0 + 11*min)
	if due := r.DueCheckins(t0 + 11*min); len(due) != 0 {
		t.Fatalf("w1 re-asked about the same stall within the interval: %+v", due)
	}
	// Unanswered: the next one, an interval later, counts a miss; two misses
	// → unresponsive.
	due = r.DueCheckins(t0 + 16*min)
	if len(due) != 1 || due[0].Missed != 1 {
		t.Fatalf("due = %+v, want a second check-in with 1 missed", due)
	}
	r.CheckinSent("f", pW1, "ci-2")
	r.DueCheckins(t0 + 26*min)
	r.CheckinSent("f", pW1, "ci-3")
	r.Evaluate(t0 + 26*min)
	if h := healthOf(r, pW1); h != HealthUnresponsive {
		t.Fatalf("w1 health = %q after two unanswered check-ins, want unresponsive", h)
	}
	// Answering resets everything and names the check-in it answered.
	_, m, answered, err := r.SetStatus("f", pW1, StatusUpdate{Work: WorkBlocked, BlockedBy: []string{"human"}}, t0+27*min)
	if err != nil {
		t.Fatal(err)
	}
	if answered != "ci-3" || m.Status.CheckinMissed != 0 || m.Status.CheckinCmd != "" {
		t.Fatalf("answered=%q missed=%d pending=%q", answered, m.Status.CheckinMissed, m.Status.CheckinCmd)
	}
}

func TestDueCheckinsPolicies(t *testing.T) {
	r := ledger(t)
	t0 := int64(20_000_000)
	for _, p := range []string{pLead, pMgr, pW1, pW2} {
		r.Observe(p, ActivityWorking, t0)
	}
	if _, err := r.SetWatch("f", Watch{Policy: PolicyAlways, IntervalSec: 120}); err != nil {
		t.Fatal(err)
	}
	r.Evaluate(t0)
	if due := r.DueCheckins(t0); len(due) != 4 {
		t.Fatalf("always: %d check-ins, want every member", len(due))
	}
	if due := r.DueCheckins(t0 + min); len(due) != 0 {
		t.Fatalf("always: re-pinged within the interval: %d", len(due))
	}
	if due := r.DueCheckins(t0 + 2*min); len(due) != 4 {
		t.Fatalf("always: %d check-ins after one interval, want 4", len(due))
	}
	if _, err := r.SetWatch("f", Watch{Policy: PolicyOff}); err != nil {
		t.Fatal(err)
	}
	if due := r.DueCheckins(t0 + 60*min); len(due) != 0 {
		t.Fatalf("off: %d check-ins", len(due))
	}
	if _, err := r.SetWatch("f", Watch{IntervalSec: 5}); err == nil {
		t.Fatal("a 5s interval was accepted")
	}
	if _, err := r.SetWatch("f", Watch{Policy: "sometimes"}); err == nil {
		t.Fatal("an unknown policy was accepted")
	}
	// A fleet that is not up is never pinged.
	_ = r.SetState("f", StateDown)
	_, _ = r.SetWatch("f", Watch{Policy: PolicyAlways, IntervalSec: 60})
	if due := r.DueCheckins(t0 + 100*min); len(due) != 0 {
		t.Fatalf("down fleet pinged: %d", len(due))
	}
}

func TestStatusSurvivesReupsertAndReregister(t *testing.T) {
	r := ledger(t)
	declare(t, r, pW1, StatusUpdate{Work: WorkProgress, Task: "parser"}, 1)
	_, _ = r.SetWatch("f", Watch{Policy: PolicyAlways, IntervalSec: 300})
	// fleetctl re-registers and re-upserts on every driver restart.
	if _, err := r.Register(Fleet{Name: "f", Source: "x.md"}); err != nil {
		t.Fatal(err)
	}
	if err := r.UpsertMember("f", Member{PaneID: pW1, Label: "wkr-01", Role: "worker"}); err != nil {
		t.Fatal(err)
	}
	f := r.Get("f")
	if f.Watch == nil || f.Watch.Policy != PolicyAlways {
		t.Fatalf("watch lost on re-register: %+v", f.Watch)
	}
	if st := member(r, pW1).Status; st == nil || st.Task != "parser" {
		t.Fatalf("status lost on re-upsert: %+v", st)
	}
	// A copy handed out is not the registry.
	f.Members[0].Status = &MemberStatus{Work: WorkDone}
	if st := member(r, pLead).Status; st != nil && st.Work == WorkDone {
		t.Fatal("mutating a copy changed the registry")
	}
}

func TestRenderStatus(t *testing.T) {
	r := ledger(t)
	declare(t, r, pW1, StatusUpdate{Work: WorkBlocked, Task: "parser", BlockedBy: []string{"mgr-01", "human"}, Note: "need the grammar"}, 1)
	r.Observe(pW1, ActivityIdle, 1)
	r.Evaluate(1)
	out := RenderStatus(r.Get("f"), 3*min)
	for _, want := range []string{"watch=smart every 10m, stalled after 5m", "wkr-01", "blocked", "blocked by: mgr-01, human",
		"blocking:   wkr-01", "note:       need the grammar", "2m", "1 ok · 3 unjudged"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q:\n%s", want, out)
		}
	}
}

func TestIntervalText(t *testing.T) {
	for sec, want := range map[int]string{20: "20s", 60: "1m", 90: "1m30s", 600: "10m", 3600: "1h", 5400: "1h30m", 0: "0s"} {
		if got := (Watch{IntervalSec: sec}).IntervalText(); got != want {
			t.Errorf("IntervalText(%ds) = %q, want %q", sec, got, want)
		}
	}
}
