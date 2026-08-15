// SPDX-License-Identifier: Apache-2.0

package actors

import "testing"

// Hidden panes and the stack (design 027 §5.1).
//
// The renderer half is tested in internal/domain. This is the half that decides
// where a human's KEYSTROKES go, which is the half that can strand them.

func hide(g *PaneGroupActor, idx int) {
	g.paneRefs[idx].hidden = true
}

func TestFocusCyclingSkipsAHiddenPane(t *testing.T) {
	g := newTestGroup(3, 0)
	hide(g, 1) // the board claude sits in the middle of the stack

	g.stackedPaneNext()
	if g.activePane != 2 {
		t.Fatalf("next from 0 with 1 hidden: want 2, got %d", g.activePane)
	}
	g.stackedPaneNext()
	if g.activePane != 0 {
		t.Fatalf("next from 2 should wrap to 0, got %d", g.activePane)
	}
	g.stackedPanePrev()
	if g.activePane != 2 {
		t.Fatalf("prev from 0 should wrap past the hidden pane to 2, got %d", g.activePane)
	}
}

// The whole group hidden is a legitimate state (every pane off screen). Landing
// somewhere arbitrary would be worse than staying put, and spinning forever
// would be worst of all.
func TestCyclingAnEntirelyHiddenGroupStandsStill(t *testing.T) {
	g := newTestGroup(3, 1)
	hide(g, 0)
	hide(g, 1)
	hide(g, 2)

	g.stackedPaneNext()
	if g.activePane != 1 {
		t.Fatalf("want the active index untouched at 1, got %d", g.activePane)
	}
}

// [n/N] is printed over drawn panes, so the index a human types back has to
// select over drawn panes too.
func TestStackSelectCountsDrawnPanesOnly(t *testing.T) {
	g := newTestGroup(3, 0)
	hide(g, 1)

	g.stackedPaneSelect(1) // "the second pane I can see" = index 2 underneath
	if g.activePane != 2 {
		t.Fatalf("select(1) with pane 1 hidden: want 2, got %d", g.activePane)
	}

	g.stackedPaneSelect(0)
	if g.activePane != 0 {
		t.Fatalf("select(0): want 0, got %d", g.activePane)
	}

	// Only two panes are drawn, so there is no third to select.
	g.stackedPaneSelect(2)
	if g.activePane != 0 {
		t.Fatalf("select(2) with two drawn panes must be a no-op, got %d", g.activePane)
	}
}

// THE HAZARD. Hiding the focused pane leaves focus in a pane that is excluded
// from cycling and drawn nowhere: it keeps taking every keystroke while
// appearing not to exist, which reads to a human as a frozen terminal.
func TestHidingTheFocusedPaneMovesFocusOffItFirst(t *testing.T) {
	g := newTestGroup(3, 1)

	g.paneRefs[1].hidden = false
	if ok := g.setPaneHiddenLocal(g.paneRefs[1].id, true); !ok {
		t.Fatal("setPaneHidden did not find the pane")
	}

	if g.activePane == 1 {
		t.Fatal("focus was left on the pane that was just hidden — keystrokes now go somewhere nothing draws")
	}
	if g.paneRefs[g.activePane].hidden {
		t.Fatalf("focus moved to another HIDDEN pane (index %d)", g.activePane)
	}
}

func TestHidingAnUnfocusedPaneLeavesFocusAlone(t *testing.T) {
	g := newTestGroup(3, 0)

	g.setPaneHiddenLocal(g.paneRefs[2].id, true)

	if g.activePane != 0 {
		t.Fatalf("hiding an unfocused pane moved focus to %d", g.activePane)
	}
}

func TestRevealingDoesNotMoveFocus(t *testing.T) {
	g := newTestGroup(3, 0)
	g.paneRefs[2].hidden = true

	g.setPaneHiddenLocal(g.paneRefs[2].id, false)

	if g.activePane != 0 {
		t.Fatalf("revealing a pane stole focus to %d — showing is not jumping", g.activePane)
	}
	if g.paneRefs[2].hidden {
		t.Fatal("the pane is still marked hidden")
	}
}

func TestSetPaneHiddenReportsAnUnknownPane(t *testing.T) {
	g := newTestGroup(2, 0)
	if g.setPaneHiddenLocal("not-a-pane-in-this-group", true) {
		t.Fatal("reported success for a pane this group does not hold")
	}
}
