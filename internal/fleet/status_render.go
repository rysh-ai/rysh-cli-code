// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"fmt"
	"strings"
	"time"
)

// RenderStatus is the ledger of one fleet as a table: one row per member,
// declared work, observed activity, derived health, and the blocking graph in
// both directions. Pure, so `rysh fleet status` and `##fleet status` print the
// same thing and it is testable without a daemon.
func RenderStatus(f *Fleet, now int64) string {
	var b strings.Builder
	w := f.Watch.Resolved()
	answer := w.Answer
	if answer == AnswerScout {
		answer += " (" + w.ScoutModel + ")"
	}
	fmt.Fprintf(&b, "fleet %s  [%s]  board=%s  watch=%s every %s, stalled after %s, answered by %s\n",
		f.Name, f.State, f.BoardID, w.Policy, w.IntervalText(),
		Watch{IntervalSec: w.StallSec}.IntervalText(), answer)
	if g := f.Goal; g != nil {
		fmt.Fprintf(&b, "  goal: %s\n", goalLine(g, now))
	}
	if gs := f.GoalStatus; gs != nil {
		fmt.Fprintf(&b, "  executor: %s\n", executorLine(f, gs, now))
	}
	if len(f.Members) == 0 {
		b.WriteString("  no members\n")
		return b.String()
	}
	fmt.Fprintf(&b, "  %-24s %-9s %-9s %-12s %-8s %s\n", "MEMBER", "WORK", "ACTIVITY", "HEALTH", "UPDATED", "TASK / BLOCKING")
	counts := map[string]int{}
	for _, m := range f.Members {
		st := m.Status
		if st == nil {
			st = &MemberStatus{}
		}
		work := or(st.Work, "-")
		counts[or(st.Health, "unjudged")]++
		updated := "never"
		if st.UpdatedAt > 0 {
			updated = shortAge(now - st.UpdatedAt)
		}
		label := m.Label
		if label == "" {
			label = "pane-" + shortPane(m.PaneID)
		}
		fmt.Fprintf(&b, "  %-24s %-9s %-9s %-12s %-8s %s\n",
			clipText(label, 24), work, or(st.Activity, "-"), or(st.Health, "-"), updated, clipText(st.Task, 60))
		if len(st.BlockedBy) > 0 {
			fmt.Fprintf(&b, "  %-24s   blocked by: %s\n", "", labelsOf(f, st.BlockedBy))
		}
		if len(st.BlockerFor) > 0 {
			fmt.Fprintf(&b, "  %-24s   blocking:   %s\n", "", labelsOf(f, st.BlockerFor))
		}
		if st.Note != "" {
			fmt.Fprintf(&b, "  %-24s   note:       %s\n", "", clipText(st.Note, 100))
		}
		if st.DeclaredBy == DeclaredByScout {
			fmt.Fprintf(&b, "  %-24s   (status declared by the scout, not the agent)\n", "")
		}
		if st.CheckinCmd != "" {
			fmt.Fprintf(&b, "  %-24s   check-in %s unanswered (%d missed before it)\n", "", st.CheckinCmd, st.CheckinMissed)
		}
		if p := progressText(st, now); p != "" {
			fmt.Fprintf(&b, "  %-24s   progress:   %s\n", "", p)
		}
		if m.Goal != nil {
			fmt.Fprintf(&b, "  %-24s   goal:       %s\n", "", goalLine(m.Goal, now))
		}
		if gs := f.GoalStatus; gs != nil && gs.Members[m.PaneID] != "" {
			fmt.Fprintf(&b, "  %-24s   on goal:    %s\n", "", gs.Members[m.PaneID])
		}
		if st.GoalAskCmd != "" {
			fmt.Fprintf(&b, "  %-24s   goal executor ask %s unanswered\n", "", st.GoalAskCmd)
		}
	}
	var parts []string
	for _, h := range []string{HealthOK, HealthAvailable, HealthSilent, HealthStalled, HealthUnblocked, HealthDeadlock, HealthUnresponsive, "unjudged"} {
		if counts[h] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[h], h))
		}
	}
	fmt.Fprintf(&b, "  health: %s\n", strings.Join(parts, " · "))
	return b.String()
}

func labelsOf(f *Fleet, ids []string) string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, MemberLabel(f, id))
	}
	return strings.Join(out, ", ")
}

func or(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func clipText(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n-1]) + "…"
}

func shortAge(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// RenderGoals is `rysh fleet goal show`: the fleet goal, the executor's
// latest verdict and cadence, and every member's goal and progress.
func RenderGoals(f *Fleet, now int64) string {
	var b strings.Builder
	ge := f.GoalExec.Resolved()
	fmt.Fprintf(&b, "fleet %s  [%s]  goal executor: %s, every %s–%s\n", f.Name, f.State, ge.Mode,
		DurationText(time.Duration(ge.MinSec)*time.Second), DurationText(time.Duration(ge.MaxSec)*time.Second))
	if f.Goal == nil {
		fmt.Fprintf(&b, "  goal: none — set one: rysh fleet goal set %s --goal '<what done looks like>' --done-when '<check>' --due 4h\n", f.Name)
	} else {
		fmt.Fprintf(&b, "  goal: %s\n", goalLine(f.Goal, now))
	}
	if gs := f.GoalStatus; gs != nil {
		fmt.Fprintf(&b, "  executor: %s\n", executorLine(f, gs, now))
		for _, w := range gs.Why {
			fmt.Fprintf(&b, "    why: %s\n", w)
		}
	} else {
		b.WriteString("  executor: no verdict yet\n")
	}
	if len(f.Members) == 0 {
		b.WriteString("  no members\n")
		return b.String()
	}
	fmt.Fprintf(&b, "  %-24s %-12s %-16s %s\n", "MEMBER", "ON GOAL", "PROGRESS", "GOAL")
	for _, m := range f.Members {
		label := m.Label
		if label == "" {
			label = "pane-" + shortPane(m.PaneID)
		}
		cond := "-"
		if gs := f.GoalStatus; gs != nil && gs.Members[m.PaneID] != "" {
			cond = gs.Members[m.PaneID]
		}
		prog := "-"
		if m.Status != nil {
			prog = or(progressText(m.Status, now), "-")
		}
		goal := "-"
		if m.Goal != nil {
			goal = goalLine(m.Goal, now)
		}
		fmt.Fprintf(&b, "  %-24s %-12s %-16s %s\n", clipText(label, 24), cond, clipText(prog, 16), clipText(goal, 90))
	}
	return b.String()
}

func goalLine(g *Goal, now int64) string {
	s := g.Text
	if g.State != "" && g.State != GoalOpen {
		s += " [" + g.State + "]"
	}
	if g.DueAt > 0 {
		if now > g.DueAt && g.IsOpen() {
			s += fmt.Sprintf(" · due %s (OVERDUE %s)", clockText(g.DueAt), DurationText(time.Duration(now-g.DueAt)*time.Millisecond))
		} else {
			s += " · due " + clockText(g.DueAt)
		}
	}
	if g.DoneWhen != "" {
		s += " · done when: " + g.DoneWhen
	}
	return s
}

func executorLine(f *Fleet, gs *GoalStatus, now int64) string {
	s := strings.ToUpper(gs.State)
	if gs.Pct >= 0 {
		s += fmt.Sprintf(" · %d%%", gs.Pct)
	}
	if gs.FinishAt > 0 {
		s += " · finish ~" + clockText(gs.FinishAt)
		if gs.Unknown > 0 {
			s += fmt.Sprintf(" (%d unknown)", gs.Unknown)
		}
	}
	if gs.Bottleneck != "" {
		s += fmt.Sprintf(" · bottleneck %s (%d waiting)", MemberLabel(f, gs.Bottleneck), gs.Waiting)
	}
	s += " · assessed " + shortAge(now-gs.AssessedAt) + " ago"
	if gs.NextAt > now {
		s += ", next in " + shortAge(gs.NextAt-now)
	}
	return s
}

func progressText(st *MemberStatus, now int64) string {
	var parts []string
	if st.Pct != nil {
		parts = append(parts, fmt.Sprintf("%d%%", *st.Pct))
	}
	if st.EtaAt > 0 {
		if now > st.EtaAt {
			parts = append(parts, "OVERDUE "+DurationText(time.Duration(now-st.EtaAt)*time.Millisecond))
		} else {
			parts = append(parts, "eta "+DurationText(time.Duration(st.EtaAt-now)*time.Millisecond))
		}
	}
	return strings.Join(parts, " · ")
}

func clockText(ms int64) string { return time.UnixMilli(ms).Format("15:04") }
