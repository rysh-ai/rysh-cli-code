// SPDX-License-Identifier: Apache-2.0

package board

import (
	"strings"
	"testing"
)

func teamFlow() Flow {
	return Flow{
		Board: "demo",
		Nodes: []FlowNode{
			{ID: "w2", Kind: FlowNodePane, Label: "wkr-2", Role: "worker", Parent: "lead", Work: "blocked", Activity: "idle", Health: "ok", Task: "sdk", BlockedBy: []string{"w1"}},
			{ID: "lead", Kind: FlowNodePane, Label: "lead", Role: "portfolio-lead", Work: "blocked", Activity: "working", Health: "ok", Task: "summary", BlockedBy: []string{"w2"}},
			{ID: "w1", Kind: FlowNodePane, Label: "wkr-1", Role: "worker", Parent: "lead", Work: "progress", Activity: "working", Health: "ok", Task: "schema"},
			{ID: "coord", Kind: FlowNodePane, Label: "coord"}, // relayed a message; not a member
			{ID: FlowNodeFleet, Kind: FlowNodeFleet, Label: "fleet"},
		},
		Events: []FlowEvent{{TS: 100_000, From: "w1"}, {TS: 160_000, From: "w1"}, {TS: 10_000, From: "lead"}},
	}
}

func TestTeamRowsChainOrderMembersOnly(t *testing.T) {
	rows := TeamRows(teamFlow())
	var got []string
	for _, r := range rows {
		got = append(got, r.Label)
	}
	if strings.Join(got, ",") != "lead,wkr-2,wkr-1" {
		t.Fatalf("rows = %v, want the lead before its workers and no non-members", got)
	}
	if rows[1].BlockedBy[0] != "wkr-1" {
		t.Errorf("blocked_by is not resolved to a label: %v", rows[1].BlockedBy)
	}
	if rows[2].LastPost != 160_000 {
		t.Errorf("last post = %d, want the newest event", rows[2].LastPost)
	}
}

func TestRenderTeamTableAndTree(t *testing.T) {
	got := strings.Join(RenderTeam(teamFlow(), 170_000, 0), "\n")
	for _, want := range []string{
		"MEMBER    WORK      ACTIVITY  HEALTH       LAST POST  TASK / BLOCKING",
		"lead      blocked   working   ok           2m         ⧗ wkr-2 · summary",
		"└─ wkr-2  blocked   idle      ok           -          ⧗ wkr-1 · sdk",
		"└─ wkr-1  progress  working   ok           10s        schema",
		"health: 3 ok",
		"dependencies · 2 blocked · bottleneck: wkr-1 (2 waiting)",
		"  ▣ wkr-1 {progress·working}  ← 2 waiting",
		"    └─ wkr-2 {blocked·idle}  ← 1 waiting",
		"       └─ lead {blocked·working}",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("team view missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "coord") {
		t.Errorf("a pane with no fleet role is listed as a member:\n%s", got)
	}
}

// The tree is always drawn: an empty section says so instead of vanishing.
func TestRenderTeamNoneBlocked(t *testing.T) {
	f := teamFlow()
	for i := range f.Nodes {
		f.Nodes[i].BlockedBy = nil
		if f.Nodes[i].Work == "blocked" {
			f.Nodes[i].Work = "done"
		}
	}
	got := RenderTeam(f, 170_000, 0)
	if last := got[len(got)-1]; last != "dependencies · none blocked" {
		t.Errorf("last line = %q, want the empty-tree line:\n%s", last, strings.Join(got, "\n"))
	}
	if m, b := TeamCounts(f); m != 3 || b != 0 {
		t.Errorf("TeamCounts = %d members, %d blocked; want 3, 0", m, b)
	}
}

func TestRenderTeamNoFleet(t *testing.T) {
	got := strings.Join(RenderTeam(Flow{Nodes: []FlowNode{{ID: "x", Kind: FlowNodePane, Label: "x"}}}, 0, 0), "\n")
	if !strings.Contains(got, "no fleet members on this board") {
		t.Errorf("a board with no fleet does not say so:\n%s", got)
	}
}

func TestRenderTeamNoLedgerYet(t *testing.T) {
	f := Flow{Nodes: []FlowNode{{ID: "a", Kind: FlowNodePane, Label: "a", Role: "worker"}}}
	got := strings.Join(RenderTeam(f, 0, 0), "\n")
	if !strings.Contains(got, "no status declared yet") {
		t.Errorf("members with no ledger rows do not say so:\n%s", got)
	}
}

func TestParseViewTeam(t *testing.T) {
	for _, s := range []string{"team", "t", " T "} {
		if v, err := ParseView(s); err != nil || v != ViewTeam {
			t.Errorf("ParseView(%q) = %q, %v", s, v, err)
		}
	}
}

// The goal executor's verdict (design 055): a GOAL column and the newest
// report in the header — and neither without an executor.
func TestRenderTeamGoalColumnAndReport(t *testing.T) {
	f := teamFlow()
	if got := strings.Join(RenderTeam(f, 170_000, 0), "\n"); strings.Contains(got, "GOAL") || strings.Contains(got, "goal executor") {
		t.Fatalf("a board with no executor grew its columns:\n%s", got)
	}
	pct := 40
	for i := range f.Nodes {
		switch f.Nodes[i].ID {
		case "w1":
			f.Nodes[i].GoalState, f.Nodes[i].GoalPct, f.Nodes[i].GoalEta = "blocker", &pct, 170_000+12*60_000
		case "w2":
			f.Nodes[i].GoalState = "stressed"
		case "lead":
			f.Nodes[i].GoalState, f.Nodes[i].GoalEta = "progressing", 110_000
		}
	}
	f.Events = append(f.Events, FlowEvent{TS: 165_000, From: "fleet", Kind: "goal-report", Summary: "AT-RISK · 40% · bottleneck wkr-1 (2 waiting)"})
	got := strings.Join(RenderTeam(f, 170_000, 0), "\n")
	for _, want := range []string{
		"goal executor · AT-RISK · 40% · bottleneck wkr-1 (2 waiting)",
		"MEMBER    WORK      ACTIVITY  HEALTH       GOAL                   LAST POST  TASK / BLOCKING",
		"└─ wkr-1  progress  working   ok           blocker 40% eta 12m    10s        schema",
		"└─ wkr-2  blocked   idle      ok           STRESSED               -          ⧗ wkr-1 · sdk",
		"lead      blocked   working   ok           progressing OVERDUE 1m 2m",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("team view missing %q:\n%s", want, got)
		}
	}
}
