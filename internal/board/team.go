// SPDX-License-Identifier: Apache-2.0

package board

import (
	"fmt"
	"strings"
)

// The team view (key t): the fleet status table and the dependency tree on
// one screen — the board pane's twin of `rysh fleet status`, read from the
// same ledger the fleet actor mirrors onto pane meta (design 054), so the
// table and the flow view's badges cannot disagree.
//
// Unlike the flow view's dependency section, the tree here is ALWAYS drawn:
// "dependencies · none blocked" when nobody waits. A section that vanishes
// when it is empty reads as a feature that is missing.

// TeamRow is one fleet member as the team table shows it.
type TeamRow struct {
	ID        string
	Label     string
	Role      string
	Work      string
	Activity  string
	Health    string
	Task      string
	BlockedBy []string // labels, not ids
	LastPost  int64    // unix millis of the member's newest post; 0 = none

	// The goal executor's verdict (design 055); empty with no executor.
	GoalState string
	GoalPct   *int
	GoalEta   int64
}

// TeamRows lists the board's fleet members in chain order (each parent before
// its children). A member is a pane that carries a fleet role or a ledger row;
// a pane that only relayed a message (a human's coordinator) is not one.
func TeamRows(f Flow) []TeamRow {
	label := map[string]string{}
	for _, n := range f.Nodes {
		label[n.ID] = n.Label
	}
	last := map[string]int64{}
	for _, e := range f.Events {
		if e.TS > last[e.From] {
			last[e.From] = e.TS
		}
	}

	var members []FlowNode
	isMember := map[string]bool{}
	for _, n := range f.Nodes {
		if n.Kind == FlowNodePane && (n.Role != "" || n.Work != "" || n.Health != "" || n.GoalState != "") {
			members = append(members, n)
			isMember[n.ID] = true
		}
	}
	children := map[string][]FlowNode{}
	var roots []FlowNode
	for _, n := range members {
		if n.Parent != "" && isMember[n.Parent] && n.Parent != n.ID {
			children[n.Parent] = append(children[n.Parent], n)
		} else {
			roots = append(roots, n)
		}
	}

	var out []TeamRow
	seen := map[string]bool{}
	var walk func(n FlowNode)
	walk = func(n FlowNode) {
		if seen[n.ID] {
			return
		}
		seen[n.ID] = true
		r := TeamRow{ID: n.ID, Label: SanitiseInline(n.Label), Role: n.Role, Work: n.Work,
			Activity: n.Activity, Health: n.Health, Task: n.Task, LastPost: last[n.ID],
			GoalState: n.GoalState, GoalPct: n.GoalPct, GoalEta: n.GoalEta}
		for _, b := range n.BlockedBy {
			l := label[b]
			if l == "" {
				l = b // "human", "ext:<what>", or a pane outside this board
			}
			r.BlockedBy = append(r.BlockedBy, SanitiseInline(l))
		}
		out = append(out, r)
		for _, c := range children[n.ID] {
			walk(c)
		}
	}
	for _, n := range roots {
		walk(n)
	}
	for _, n := range members { // a parent cycle leaves members unreached
		walk(n)
	}
	return out
}

// RenderTeam draws the team view as plain lines: the table, a health summary,
// then the dependency tree in at most depRows lines (0 = no limit). nowMS
// dates the LAST POST column.
//
//	MEMBER                  WORK      ACTIVITY  HEALTH     LAST POST  TASK / BLOCKING
//	lead                    blocked   idle      ok         12s        ⧗ wkr-2 · waiting for SDK
//	└─ wkr-1                progress  working   ok         40s        schema v3
//	health: 2 ok · 1 blocked
//
//	dependencies · 1 blocked · bottleneck: wkr-2 (1 waiting)
//	  ▣ wkr-2 {progress·working}  ← 1 waiting
//	    └─ lead {blocked·idle}
func RenderTeam(f Flow, nowMS int64, depRows int) []string {
	rows := TeamRows(f)
	if len(rows) == 0 {
		return []string{
			"no fleet members on this board — the team view lists panes that carry fleet.role",
			"(a fleet started with fleetctl up stamps it); s p a f show the board's messages.",
		}
	}

	names := make([]string, len(rows))
	nameW := len("MEMBER")
	depth := teamDepths(rows, f)
	for i, r := range rows {
		names[i] = r.Label
		if depth[i] > 0 {
			names[i] = strings.Repeat("   ", depth[i]-1) + "└─ " + r.Label
		}
		nameW = max(nameW, len([]rune(names[i])))
	}
	dash := func(s string) string {
		if s == "" {
			return "-"
		}
		return s
	}
	// The GOAL column (design 055) only when a goal executor runs: a board
	// without one renders byte-identically to before.
	goals := false
	for _, r := range rows {
		if r.GoalState != "" {
			goals = true
		}
	}
	line := func(name, work, act, health, goal, lastPost, task string) string {
		pad := nameW - len([]rune(name))
		if goals {
			return strings.TrimRight(fmt.Sprintf("%s%s  %-9s %-9s %-12s %-22s %-10s %s",
				name, strings.Repeat(" ", max(0, pad)), work, act, health, goal, lastPost, task), " ")
		}
		return strings.TrimRight(fmt.Sprintf("%s%s  %-9s %-9s %-12s %-10s %s",
			name, strings.Repeat(" ", max(0, pad)), work, act, health, lastPost, task), " ")
	}

	var out []string
	if rep := TeamGoalReport(f); rep != "" {
		out = append(out, "goal executor · "+rep)
	}
	out = append(out, line("MEMBER", "WORK", "ACTIVITY", "HEALTH", "GOAL", "LAST POST", "TASK / BLOCKING"))
	var healthOrder []string
	healthN := map[string]int{}
	ledger := false
	for i, r := range rows {
		task := r.Task
		if len(r.BlockedBy) > 0 {
			task = "⧗ " + strings.Join(r.BlockedBy, ", ") + teamSep(task) + task
		}
		h := r.Health
		if h != "" && h != "ok" {
			h = strings.ToUpper(h)
		}
		out = append(out, line(names[i], dash(r.Work), dash(r.Activity), dash(h), dash(TeamGoalCell(r, nowMS)), teamAge(r.LastPost, nowMS), task))
		if r.Work != "" || r.Health != "" {
			ledger = true
		}
		key := r.Health
		if key == "" {
			key = "no status"
		}
		if healthN[key] == 0 {
			healthOrder = append(healthOrder, key)
		}
		healthN[key]++
	}
	if ledger {
		parts := make([]string, 0, len(healthOrder))
		for _, k := range healthOrder {
			parts = append(parts, fmt.Sprintf("%d %s", healthN[k], k))
		}
		out = append(out, "health: "+strings.Join(parts, " · "))
	} else {
		out = append(out, "no status declared yet — members report it with `rysh fleet status` (design 054)")
	}

	out = append(out, "")
	deps := BuildFlowDeps(f)
	if deps.Blocked == 0 {
		return append(out, "dependencies · none blocked")
	}
	if depRows > 0 {
		return append(out, RenderFlowDepsFit(deps, depRows)...)
	}
	return append(out, RenderFlowDeps(deps)...)
}

// TeamCounts is what the team view's header says: members and how many wait.
func TeamCounts(f Flow) (members, blocked int) {
	return len(TeamRows(f)), BuildFlowDeps(f).Blocked
}

func teamSep(task string) string {
	if task == "" {
		return ""
	}
	return " · "
}

// teamDepths is each row's depth in the chain, for the └─ indent.
func teamDepths(rows []TeamRow, f Flow) []int {
	parent := map[string]string{}
	for _, n := range f.Nodes {
		parent[n.ID] = n.Parent
	}
	in := map[string]bool{}
	for _, r := range rows {
		in[r.ID] = true
	}
	out := make([]int, len(rows))
	for i, r := range rows {
		d, p := 0, parent[r.ID]
		for seen := map[string]bool{r.ID: true}; p != "" && in[p] && !seen[p]; p = parent[p] {
			seen[p] = true
			d++
		}
		out[i] = d
	}
	return out
}

// teamAge renders how long ago a post was: 12s, 4m, 3h, 2d; "-" for none.
func teamAge(tsMS, nowMS int64) string {
	if tsMS <= 0 {
		return "-"
	}
	s := (nowMS - tsMS) / 1000
	switch {
	case s < 0:
		return "0s"
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm", s/60)
	case s < 86400:
		return fmt.Sprintf("%dh", s/3600)
	}
	return fmt.Sprintf("%dd", s/86400)
}

// TeamGoalCell renders a member's goal verdict: "blocker 40% eta 12m",
// "STRESSED", "progressing OVERDUE 5m". The conditions that need someone to
// act shout, like unhealthy health does.
func TeamGoalCell(r TeamRow, nowMS int64) string {
	if r.GoalState == "" {
		return ""
	}
	s := r.GoalState
	switch s {
	case "stressed", "strayed", "deadlock", "stalled":
		s = strings.ToUpper(s)
	}
	if r.GoalPct != nil {
		s += fmt.Sprintf(" %d%%", *r.GoalPct)
	}
	if r.GoalEta > 0 {
		if nowMS > r.GoalEta {
			s += " OVERDUE " + teamAge(r.GoalEta, nowMS)
		} else {
			s += " eta " + teamSpan(r.GoalEta-nowMS)
		}
	}
	return s
}

// TeamGoalReport is the first line of the newest goal-report on the board
// (design 055), "" when no goal executor has reported.
func TeamGoalReport(f Flow) string {
	for i := len(f.Events) - 1; i >= 0; i-- {
		if e := f.Events[i]; e.Kind == "goal-report" {
			return e.Summary
		}
	}
	return ""
}

// teamSpan renders a duration the way teamAge renders an age: 12s, 4m, 3h, 2d.
func teamSpan(ms int64) string { return teamAge(1, 1+ms) }
