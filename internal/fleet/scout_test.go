// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"strings"
	"testing"
)

// The status scout (design 056 §3): what it may answer, how its reply becomes
// a declaration, and that it can never hide a stall.

func scoutCheckin(reason, health string, scoutAt, scoutHealth, healthSince int64, sh string) Checkin {
	return Checkin{Reason: reason, Member: Member{PaneID: "p", Status: &MemberStatus{
		Health: health, HealthSince: healthSince, ScoutAt: scoutAt, ScoutHealth: sh}}}
}

func TestScoutEligibility(t *testing.T) {
	scout := Watch{Answer: AnswerScout}
	for _, c := range []struct {
		name string
		c    Checkin
		w    Watch
		want bool
	}{
		{"agent mode never scouts", scoutCheckin(HealthStalled, HealthStalled, 0, 0, 1, ""), Watch{}, false},
		{"a stalled verdict, first time", scoutCheckin(HealthStalled, HealthStalled, 0, 0, 1, ""), scout, true},
		{"a silent verdict, first time", scoutCheckin(HealthSilent, HealthSilent, 0, 0, 1, ""), scout, true},
		{"the routine check-in", scoutCheckin("scheduled", HealthOK, 0, 0, 1, ""), scout, true},
		{"the same stalled verdict the scout already answered goes to the agent",
			scoutCheckin(HealthStalled, HealthStalled, 50, 0, 10, HealthStalled), scout, false},
		{"a NEW stalled verdict after the scout's answer is the scout's again",
			scoutCheckin(HealthStalled, HealthStalled, 50, 0, 90, HealthStalled), scout, true},
		{"unblocked means resume: the agent", scoutCheckin(HealthUnblocked, HealthUnblocked, 0, 0, 1, ""), scout, false},
		{"unresponsive is the agent's own silence", scoutCheckin(HealthUnresponsive, HealthUnresponsive, 0, 0, 1, ""), scout, false},
	} {
		if got := ScoutEligible(c.c, c.w); got != c.want {
			t.Errorf("%s: ScoutEligible = %v", c.name, got)
		}
	}
}

func TestScoutAnswerParsingAndRefusals(t *testing.T) {
	a, err := ParseScoutAnswer("Here you go:\n```json\n{\"answerable\": true, \"work\": \"Progress\", \"task\": \"\", \"note\": \"go test running\", \"confidence\": 0.9}\n```")
	if err != nil || a.Work != "progress" {
		t.Fatalf("parse = %+v, %v", a, err)
	}
	u, err := ScoutUpdate(a, "haiku")
	if err != nil || u.By != DeclaredByScout || !u.KeepTask || !strings.Contains(u.Note, "answered by the status scout (haiku): go test running") {
		t.Fatalf("update = %+v, %v", u, err)
	}
	for _, c := range []struct {
		a    ScoutAnswer
		want string
	}{
		{ScoutAnswer{Answerable: false, Reason: "asks the human"}, "could not tell"},
		{ScoutAnswer{Answerable: true, Work: "progress", Confidence: 0.4}, "not sure enough"},
		{ScoutAnswer{Answerable: true, Work: "napping", Confidence: 0.9}, "unknown work state"},
		{ScoutAnswer{Answerable: true, Work: "blocked", Confidence: 0.9}, "without naming a blocker"},
	} {
		if _, err := ScoutUpdate(c.a, "haiku"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("ScoutUpdate(%+v) = %v, want %q", c.a, err, c.want)
		}
	}
	if _, err := ParseScoutAnswer("I cannot help with that."); err == nil {
		t.Error("a reply with no JSON was accepted")
	}
}

// The scout's declaration answers nothing asked of the agent, and does not
// reset the agent's own clock — so it cannot hide a stall.
func TestAScoutDeclarationCannotHideAStall(t *testing.T) {
	r := goalFleet(t)
	if _, err := r.Declare("ship", "bbbb2222", StatusUpdate{Work: WorkProgress, Task: "endpoints"}, 1000); err != nil {
		t.Fatal(err)
	}
	r.CheckinSent("ship", "bbbb2222", "ci-1")
	r.GoalAskSent("ship", "bbbb2222", "ga-1", 1500)
	res, err := r.Declare("ship", "bbbb2222", StatusUpdate{Work: WorkProgress, By: DeclaredByScout, KeepTask: true, Note: "scout"}, 9000)
	if err != nil {
		t.Fatal(err)
	}
	st := res.Member.Status
	if res.AnsweredCheckin != "" || res.AnsweredGoalAsk != "" || st.CheckinCmd != "ci-1" || st.GoalAskCmd != "ga-1" {
		t.Fatalf("the scout answered questions asked of the agent: %+v", res)
	}
	if st.UpdatedAt != 1000 {
		t.Fatalf("the scout reset the agent's own declaration clock: %d", st.UpdatedAt)
	}
	if st.DeclaredBy != DeclaredByScout || st.Task != "endpoints" {
		t.Fatalf("declared_by=%q task=%q", st.DeclaredBy, st.Task)
	}
	// The agent's own next word answers both and is the agent's again.
	res, _ = r.Declare("ship", "bbbb2222", StatusUpdate{Work: WorkProgress, KeepTask: true}, 9500)
	if res.AnsweredCheckin != "ci-1" || res.Member.Status.DeclaredBy != "" || res.Member.Status.UpdatedAt != 9500 {
		t.Fatalf("the agent's own declaration = %+v", res)
	}
}

func TestScoutPromptCarriesTheEvidence(t *testing.T) {
	r := goalFleet(t)
	_, _ = r.SetGoal("ship", GoalUpdate{Member: "api", Text: "orders API v2"}, 1)
	_, _ = r.Declare("ship", "bbbb2222", StatusUpdate{Work: WorkProgress, Task: "endpoints"}, 1000)
	f := r.Get("ship")
	p := BuildScoutPrompt(f, &f.Members[1], HealthStalled, []string{"$ go test ./...", "ok  pkg/a"}, []string{"progress: endpoints"}, true, 61_000)
	for _, want := range []string{"Member api", "Check-in reason: stalled", "Its goal: orders API v2", "work=progress task=\"endpoints\"",
		"declared 1m ago", "tool command running now: true", "Other members: lead", "progress: endpoints", "$ go test ./..."} {
		if !strings.Contains(p, want) {
			t.Errorf("scout prompt lacks %q:\n%s", want, p)
		}
	}
	if err := (Watch{Answer: "maybe"}).Validate(); err == nil {
		t.Error("an unknown --answer was accepted")
	}
	if w := (&Watch{Answer: AnswerScout}).Resolved(); w.ScoutModel != DefaultScoutModel {
		t.Errorf("default scout model = %q", w.ScoutModel)
	}
}
