// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"testing"

	"github.com/asynkron/protoactor-go/actor"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// newMoveTestGroup builds a PaneGroupActor holding n panes with ids "a…", "b…"
// and so on, with no NATS or actor system behind it. It mirrors newTestGroup in
// pane_group_move_test.go but also populates the PID / actor maps, because a
// transfer moves those and not only the refs.
func newMoveTestGroup(id string, n, active int) *PaneGroupActor {
	return newMoveTestGroupFrom(id, 0, n, active)
}

// newMoveTestGroupFrom is newMoveTestGroup with the pane ids starting at a
// different letter, so a source and a destination group hold DISTINCT panes —
// adoptPane refuses a duplicate id, and reusing "a…" for both would trip that
// refusal instead of the behaviour under test.
func newMoveTestGroupFrom(id string, first, n, active int) *PaneGroupActor {
	g := NewPaneGroupActor(id, "tab-1", "lane-1", "", "", config.Config{}, nil, nil, nil, nil, nil)
	g.activePane = active
	for i := first; i < first+n; i++ {
		paneID := paneIDForIndex(i)
		g.paneRefs = append(g.paneRefs, &paneRefInGroup{id: paneID, title: paneID[:1]})
		g.panePIDs[paneID] = actor.NewPID("test", paneID)
		g.paneActors[paneID] = &PaneActor{id: paneID}
	}
	return g
}

func stackOrder(g *PaneGroupActor) string {
	s := ""
	for _, r := range g.paneRefs {
		s += r.id[:1]
	}
	return s
}

func TestReleasePaneKeepsTheActorAndItsBookkeepingTogether(t *testing.T) {
	g := newMoveTestGroup("g0", 3, 1) // a,b,c — active is b

	h, empty, ok := g.releasePane(paneIDForIndex(1))
	if !ok {
		t.Fatalf("releasePane(b) refused")
	}
	if empty {
		t.Fatalf("empty = true with two panes left")
	}
	if got, want := stackOrder(g), "ac"; got != want {
		t.Fatalf("order after release = %q, want %q", got, want)
	}
	// The handle has to carry all three, or the adopting group cannot serve the
	// pane: the ref renders it, the PID addresses it, the struct answers the
	// cross-actor reads (history, snapshot, output append).
	if h.ref == nil || h.pid == nil || h.pa == nil {
		t.Fatalf("handle is incomplete: ref=%v pid=%v pa=%v", h.ref, h.pid, h.pa)
	}
	if h.ID() != paneIDForIndex(1) {
		t.Fatalf("handle id = %q, want %q", h.ID(), paneIDForIndex(1))
	}
	// The source must forget it entirely — a lingering map entry would make the
	// old group stop a pane it no longer holds when IT is closed.
	for name, present := range map[string]bool{
		"panePIDs":     g.panePIDs[h.ID()] != nil,
		"paneActors":   g.paneActors[h.ID()] != nil,
		"paneSubjects": g.paneSubjects[h.ID()] != "",
	} {
		if present {
			t.Errorf("released pane is still in %s", name)
		}
	}
}

func TestReleasingTheLastPaneReportsTheGroupEmpty(t *testing.T) {
	// deletePaneByID refuses to empty a group; a MOVE must be allowed to, or a
	// pane could never leave the stack it is alone in. The lane drops the
	// emptied group afterwards.
	g := newMoveTestGroup("g0", 1, 0)

	h, empty, ok := g.releasePane(paneIDForIndex(0))
	if !ok || h == nil {
		t.Fatalf("releasePane refused the group's only pane")
	}
	if !empty {
		t.Fatalf("empty = false after releasing the only pane")
	}
	if len(g.paneRefs) != 0 {
		t.Fatalf("paneRefs = %d, want 0", len(g.paneRefs))
	}
}

func TestReleasePaneRefusesAnApprovalPane(t *testing.T) {
	// An approval pane is ephemeral UI owned by its group, with no PaneActor
	// behind it. Moving one would render an empty slot somewhere else and lose
	// the approval when the source group tore it down.
	g := newMoveTestGroup("g0", 2, 0)
	g.paneRefs[0].paneType = "approval"

	if _, _, ok := g.releasePane(paneIDForIndex(0)); ok {
		t.Fatalf("releasePane accepted an approval pane")
	}
	if len(g.paneRefs) != 2 {
		t.Fatalf("the refused release still mutated the group: %d panes", len(g.paneRefs))
	}
}

func TestAdoptPanePlacesAndFocuses(t *testing.T) {
	src := newMoveTestGroup("g0", 3, 0)
	dst := newMoveTestGroupFrom("g1", 23, 2, 0) // x, y

	h, _, ok := src.releasePane(paneIDForIndex(2)) // c
	if !ok {
		t.Fatalf("release refused")
	}
	if !dst.adoptPane(nil, h, 1) {
		t.Fatalf("adoptPane refused")
	}
	if got, want := stackOrder(dst), "xcy"; got != want {
		t.Fatalf("destination order = %q, want %q", got, want)
	}
	if dst.activePane != 1 {
		t.Fatalf("activePane = %d, want 1 (the arriving pane is what you want to see)", dst.activePane)
	}
	if dst.panePIDs[h.ID()] == nil || dst.paneActors[h.ID()] == nil || dst.paneSubjects[h.ID()] == "" {
		t.Fatalf("destination did not take over the pane's PID/actor/subject")
	}
}

func TestAdoptPaneAppendsWhenTheIndexIsOutOfRange(t *testing.T) {
	src := newMoveTestGroup("g0", 2, 0)

	h, _, _ := src.releasePane(paneIDForIndex(0))
	for _, index := range []int{-1, 99} {
		g := newMoveTestGroupFrom("g2", 23, 1, 0) // x
		if !g.adoptPane(nil, h, index) {
			t.Fatalf("adoptPane(index=%d) refused", index)
		}
		if got, want := stackOrder(g), "xa"; got != want {
			t.Fatalf("index=%d order = %q, want %q (append)", index, got, want)
		}
	}
}

func TestAdoptPaneRefusesAPaneItAlreadyHolds(t *testing.T) {
	g := newMoveTestGroup("g0", 2, 0)
	h := &paneHandle{
		ref: &paneRefInGroup{id: paneIDForIndex(0)},
		pid: actor.NewPID("test", "dup"),
		pa:  &PaneActor{id: paneIDForIndex(0)},
	}
	if g.adoptPane(nil, h, 0) {
		t.Fatalf("adoptPane accepted a duplicate pane id — the group would render it twice")
	}
}

func TestMovePaneInStackMovesANamedPaneNotTheActiveOne(t *testing.T) {
	// The keyboard path (moveStackedPaneUp) only ever moves the active pane.
	// ##move has to reorder a pane in a stack nobody is sitting in.
	cases := []struct {
		name   string
		pane   int
		dir    msg.Direction
		want   string
		wantOK bool
	}{
		{"third up while first is active", 2, msg.DirUp, "acb", true},
		{"first down while first is active", 0, msg.DirDown, "bac", true},
		{"first up is a no-op", 0, msg.DirUp, "abc", false},
		{"last down is a no-op", 2, msg.DirDown, "abc", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newMoveTestGroup("g0", 3, 0)
			ok := g.movePaneInStack(paneIDForIndex(tc.pane), tc.dir)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if got := stackOrder(g); got != tc.want {
				t.Fatalf("order = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMoveGroupReordersANamedStackWithinItsLane(t *testing.T) {
	cases := []struct {
		name   string
		group  int
		dir    msg.Direction
		want   string
		wantOK bool
	}{
		{"third up", 2, msg.DirUp, "acb", true},
		{"first down", 0, msg.DirDown, "bac", true},
		{"first up is a no-op", 0, msg.DirUp, "abc", false},
		{"unknown id is a no-op", -1, msg.DirUp, "abc", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := newTestLane(3, 0)
			id := "nope"
			if tc.group >= 0 {
				id = groupIDForIndex(tc.group)
			}
			if ok := l.moveGroup(id, tc.dir); ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if got := groupOrder(l); got != tc.want {
				t.Fatalf("order = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMoveLaneReordersANamedLaneWithinItsTab(t *testing.T) {
	newTab := func(n int) *TabActor {
		t := &TabActor{}
		for i := 0; i < n; i++ {
			t.laneRefs = append(t.laneRefs, &laneRef{id: groupIDForIndex(i), flex: 10})
		}
		return t
	}
	order := func(t *TabActor) string {
		s := ""
		for _, lr := range t.laneRefs {
			s += lr.id[:1]
		}
		return s
	}

	tab := newTab(3)
	if !tab.moveLane(groupIDForIndex(2), msg.DirLeft) {
		t.Fatalf("moveLane refused")
	}
	if got, want := order(tab), "acb"; got != want {
		t.Fatalf("order = %q, want %q", got, want)
	}
	if tab.moveLane(groupIDForIndex(0), msg.DirLeft) {
		t.Fatalf("moveLane moved the first lane further left")
	}
	if tab.moveLane("missing", msg.DirRight) {
		t.Fatalf("moveLane accepted an unknown lane id")
	}
}
