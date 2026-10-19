// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"strings"
	"testing"
)

// Goals, progress and the executor's config (design 055) in the registry.

func goalFleet(t *testing.T) *Registry {
	t.Helper()
	r := New()
	if _, err := r.Register(Fleet{Name: "ship"}); err != nil {
		t.Fatal(err)
	}
	for _, m := range []Member{{PaneID: "aaaa1111", Label: "lead"}, {PaneID: "bbbb2222", Label: "api", Parent: "aaaa1111"}} {
		if err := r.UpsertMember("ship", m); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func TestSetGoalRefusesWhatWouldMisjudgeTheFleet(t *testing.T) {
	r := goalFleet(t)
	for _, c := range []struct {
		gu   GoalUpdate
		want string
	}{
		{GoalUpdate{}, "needs --goal"},
		{GoalUpdate{State: GoalMet}, "no goal to mark met"},
		{GoalUpdate{Member: "nobody", Text: "x"}, "no member"},
		{GoalUpdate{Text: "x", State: "maybe"}, "unknown goal state"},
		{GoalUpdate{Text: "x", DueSec: -1}, "cannot be negative"},
	} {
		if _, err := r.SetGoal("ship", c.gu, 1000); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("SetGoal(%+v) = %v, want %q", c.gu, err, c.want)
		}
	}
	if _, err := r.SetGoal("nope", GoalUpdate{Text: "x"}, 0); err == nil {
		t.Error("a goal for an unknown fleet was accepted")
	}
}

func TestGoalLifecycleAndRestatementKeepsTheDeadline(t *testing.T) {
	r := goalFleet(t)
	f, err := r.SetGoal("ship", GoalUpdate{Text: "ship v2", DoneWhen: "e2e green", DueSec: 3600}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if f.Goal.DueAt != 1000+3600*1000 || !f.Goal.IsOpen() {
		t.Fatalf("goal = %+v", f.Goal)
	}
	// Restating the same goal without --due keeps its deadline.
	f, _ = r.SetGoal("ship", GoalUpdate{Text: "ship v2", DoneWhen: "e2e green on staging"}, 5000)
	if f.Goal.DueAt != 1000+3600*1000 || f.Goal.DoneWhen != "e2e green on staging" {
		t.Fatalf("restated goal = %+v", f.Goal)
	}
	f, _ = r.SetGoal("ship", GoalUpdate{State: GoalMet, Note: "staging green"}, 9000)
	if !f.Goal.IsMet() || f.Goal.MetAt != 9000 || f.Goal.Note != "staging green" || f.Goal.IsOpen() {
		t.Fatalf("met goal = %+v", f.Goal)
	}
	// A member's goal, by label.
	f, _ = r.SetGoal("ship", GoalUpdate{Member: "api", Text: "orders API v2"}, 9000)
	if f.Members[1].Goal == nil || f.Members[1].Goal.Text != "orders API v2" {
		t.Fatalf("member goal = %+v", f.Members[1].Goal)
	}
}

func TestGoalsSurviveReRegistrationAndUpsert(t *testing.T) {
	r := goalFleet(t)
	_, _ = r.SetGoal("ship", GoalUpdate{Text: "ship v2"}, 1)
	_, _ = r.SetGoal("ship", GoalUpdate{Member: "api", Text: "orders API v2"}, 1)
	_, _ = r.SetGoalExec("ship", GoalExec{Mode: GoalExecFixed, MinSec: 120})
	r.SetGoalStatus("ship", GoalStatus{State: "on-track", Pct: 10})
	// The driver restarts: re-registers and re-upserts with nothing.
	if _, err := r.Register(Fleet{Name: "ship", Source: "E7.md"}); err != nil {
		t.Fatal(err)
	}
	if err := r.UpsertMember("ship", Member{PaneID: "bbbb2222", Label: "api"}); err != nil {
		t.Fatal(err)
	}
	f := r.Get("ship")
	if f.Goal == nil || f.GoalExec == nil || f.GoalExec.Mode != GoalExecFixed || f.GoalStatus == nil || f.Members[1].Goal == nil {
		t.Fatalf("a restart of the driver forgot goals: %+v", f)
	}
	// And a Get is a copy.
	f.Goal.Text = "mutated"
	f.GoalStatus.Members = map[string]string{"x": "y"}
	if g := r.Get("ship"); g.Goal.Text != "ship v2" || g.GoalStatus.Members != nil {
		t.Fatal("Get handed out the registry's own goal")
	}
}

func TestProgressDescribesATask(t *testing.T) {
	r := goalFleet(t)
	pct := 40
	res, err := r.Declare("ship", "bbbb2222", StatusUpdate{Work: WorkProgress, Task: "endpoints", Pct: &pct, EtaSec: 600}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	st := res.Member.Status
	if *st.Pct != 40 || st.EtaAt != 1000+600_000 || st.EtaSetAt != 1000 {
		t.Fatalf("progress = %d%% eta %d set %d", *st.Pct, st.EtaAt, st.EtaSetAt)
	}
	// Same task, no new numbers: kept.
	res, _ = r.Declare("ship", "bbbb2222", StatusUpdate{Work: WorkProgress, Task: "endpoints"}, 2000)
	if res.Member.Status.Pct == nil || res.Member.Status.EtaAt == 0 {
		t.Fatal("re-declaring the same task dropped its progress")
	}
	// blocked keeps the task, and so its progress.
	res, _ = r.Declare("ship", "bbbb2222", StatusUpdate{Work: WorkBlocked, BlockedBy: []string{"human"}, KeepTask: true}, 3000)
	if res.Member.Status.Pct == nil {
		t.Fatal("blocked on the same task dropped its progress")
	}
	// A new task starts from nothing known.
	res, _ = r.Declare("ship", "bbbb2222", StatusUpdate{Work: WorkProgress, Task: "pagination"}, 4000)
	if res.Member.Status.Pct != nil || res.Member.Status.EtaAt != 0 {
		t.Fatalf("a new task inherited the old one's progress: %+v", res.Member.Status)
	}
	bad := 140
	if _, err := r.Declare("ship", "bbbb2222", StatusUpdate{Work: WorkProgress, Pct: &bad}, 5000); err == nil {
		t.Fatal("pct 140 accepted")
	}
}

func TestADeclarationAnswersTheExecutorsAsk(t *testing.T) {
	r := goalFleet(t)
	r.GoalAskSent("ship", "bbbb2222", "ga-1", 100)
	r.CheckinSent("ship", "bbbb2222", "ci-1")
	res, err := r.Declare("ship", "bbbb2222", StatusUpdate{Work: WorkProgress, Task: "x"}, 200)
	if err != nil {
		t.Fatal(err)
	}
	if res.AnsweredGoalAsk != "ga-1" {
		t.Fatalf("answered goal ask = %q", res.AnsweredGoalAsk)
	}
	if res.Member.Status.GoalAskCmd != "" {
		t.Fatal("the ask stayed pending after an answer")
	}
}

func TestGoalExecValidation(t *testing.T) {
	for _, c := range []struct {
		g    GoalExec
		want string
	}{
		{GoalExec{Mode: "often"}, "unknown goal executor mode"},
		{GoalExec{MinSec: 10}, "below the 60s floor"},
		{GoalExec{MinSec: 600, MaxSec: 300}, "below --min"},
		{GoalExec{MaxSec: 120}, "set --min too"},
	} {
		if err := c.g.Validate(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Validate(%+v) = %v, want %q", c.g, err, c.want)
		}
	}
	if r := (&GoalExec{MinSec: 120}).Resolved(); r.Mode != GoalExecAdaptive || r.MinSec != 120 || r.MaxSec != DefaultGoalExecMaxSec {
		t.Fatalf("Resolved = %+v", r)
	}
}

func TestRenderGoals(t *testing.T) {
	r := goalFleet(t)
	_, _ = r.SetGoal("ship", GoalUpdate{Text: "ship v2", DoneWhen: "e2e green"}, 1000)
	_, _ = r.SetGoal("ship", GoalUpdate{Member: "api", Text: "orders API v2"}, 1000)
	r.SetGoalStatus("ship", GoalStatus{State: "at-risk", Pct: 40, Bottleneck: "bbbb2222", Waiting: 2, AssessedAt: 1000,
		Members: map[string]string{"bbbb2222": "blocker"}})
	out := RenderGoals(r.Get("ship"), 61_000)
	for _, want := range []string{"goal executor: adaptive, every 5m–30m", "goal: ship v2 · done when: e2e green",
		"executor: AT-RISK · 40% · bottleneck api (2 waiting) · assessed 1m ago", "api", "blocker", "orders API v2"} {
		if !strings.Contains(out, want) {
			t.Errorf("RenderGoals lacks %q:\n%s", want, out)
		}
	}
}
