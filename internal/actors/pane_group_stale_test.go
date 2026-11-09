// SPDX-License-Identifier: Apache-2.0

package actors

// A pane that misses its group's snapshot deadline is served as its last
// answer, marked Stale — not as a zero-valued placeholder whose RawMode=false
// told every client a running claude had exited (the busy-session
// claude→shell flip).

import (
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

func TestSubstituteStaleServesLastAnswer(t *testing.T) {
	g := &PaneGroupActor{paneRefs: []*paneRefInGroup{{id: "p1"}, {id: "p2"}}}
	have := []bool{true, true}

	answered := []domain.PaneSnapshot{
		{ID: "p1", RawMode: true, FullScreen: true, Program: "claude", Output: "big buffer", VTScreen: []string{"x"}},
		{ID: "p2"},
	}
	g.substituteStale(answered, have, []bool{true, true})

	timedOut := []domain.PaneSnapshot{
		{ID: "p1", Status: "unreachable", Stale: true},
		{ID: "p2"},
	}
	got := g.substituteStale(timedOut, have, []bool{false, true})

	p1 := got[0]
	if !p1.Stale || !p1.RawMode || !p1.FullScreen || p1.Program != "claude" {
		t.Fatalf("p1 should be its last answer marked Stale, got %+v", p1)
	}
	if p1.Output != "" || p1.VTScreen != nil {
		t.Fatalf("the kept copy must not carry content, got Output=%q VTScreen=%v", p1.Output, p1.VTScreen)
	}
	if got[1].Stale {
		t.Fatal("an answered pane must not be marked Stale")
	}
}

func TestSubstituteStaleWithoutHistoryKeepsMarkedPlaceholder(t *testing.T) {
	g := &PaneGroupActor{paneRefs: []*paneRefInGroup{{id: "p1"}}}
	got := g.substituteStale([]domain.PaneSnapshot{{ID: "p1", Status: "unreachable", Stale: true}}, []bool{true}, []bool{false})
	if !got[0].Stale || got[0].Status != "unreachable" {
		t.Fatalf("with nothing recorded the placeholder stays, marked Stale; got %+v", got[0])
	}
}

func TestSubstituteStalePrunesClosedPanes(t *testing.T) {
	g := &PaneGroupActor{paneRefs: []*paneRefInGroup{{id: "p1"}}}
	g.substituteStale([]domain.PaneSnapshot{{ID: "p1"}}, []bool{true}, []bool{true})
	g.paneRefs = []*paneRefInGroup{{id: "p2"}}
	g.substituteStale([]domain.PaneSnapshot{{ID: "p2"}}, []bool{true}, []bool{true})
	if _, ok := g.lastAnswered["p1"]; ok {
		t.Fatal("a closed pane's kept snapshot must be dropped")
	}
}

func TestSubstituteStaleGroupsServesLastGroup(t *testing.T) {
	l := &LaneActor{groupRefs: []*laneGroupRef{{id: "g1", rowFlex: 2}}}
	l.substituteStaleGroups([]domain.PaneGroupSnapshot{{
		ID: "g1", ActivePaneID: "p1",
		Panes: []domain.PaneSnapshot{{ID: "p1", RawMode: true}, {ID: "p2"}},
	}}, []bool{true})

	got := l.substituteStaleGroups([]domain.PaneGroupSnapshot{{
		ID: "g1", ActivePaneID: "p2",
		Panes: []domain.PaneSnapshot{{ID: "p2", Status: "unreachable", Stale: true}},
	}}, []bool{false})

	g := got[0]
	if len(g.Panes) != 2 || !g.Panes[0].RawMode || !g.Panes[0].Stale || !g.Panes[1].Stale {
		t.Fatalf("timed-out group should be its last answer with panes marked Stale, got %+v", g.Panes)
	}
	if g.ActivePaneID != "p2" || g.RowFlex != 2 {
		t.Fatalf("ActivePaneID/RowFlex must be the lane's current bookkeeping, got %q/%d", g.ActivePaneID, g.RowFlex)
	}
}
