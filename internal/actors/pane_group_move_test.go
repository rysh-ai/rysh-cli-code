package actors

import "testing"

// newTestGroup builds a PaneGroupActor populated with n panes (ids "p0".."pn-1")
// and the given active index, with no NATS/actor dependencies.
func newTestGroup(n, active int) *PaneGroupActor {
	g := &PaneGroupActor{id: "group-0000000000000000000000000000", activePane: active}
	for i := 0; i < n; i++ {
		g.paneRefs = append(g.paneRefs, &paneRefInGroup{id: paneIDForIndex(i)})
	}
	return g
}

func paneIDForIndex(i int) string {
	return string(rune('a'+i)) + "00000000000000000000000000000000" // 8+ chars so [:8] logging is safe
}

// order returns the current pane id order joined for easy comparison.
func order(g *PaneGroupActor) string {
	s := ""
	for _, r := range g.paneRefs {
		s += r.id[:1]
	}
	return s
}

func TestMoveStackedPaneUp(t *testing.T) {
	g := newTestGroup(3, 2) // order a,b,c ; active = c (index 2)
	g.moveStackedPaneUp()
	if got, want := order(g), "acb"; got != want {
		t.Fatalf("order after up = %q, want %q", got, want)
	}
	if g.activePane != 1 {
		t.Fatalf("activePane = %d, want 1 (moved pane stays active)", g.activePane)
	}
	if g.paneRefs[g.activePane].id[:1] != "c" {
		t.Fatalf("active pane id = %q, want c", g.paneRefs[g.activePane].id[:1])
	}
}

func TestMoveStackedPaneUp_AtFrontIsNoop(t *testing.T) {
	g := newTestGroup(3, 0) // active at front
	g.moveStackedPaneUp()
	if got, want := order(g), "abc"; got != want {
		t.Fatalf("order = %q, want unchanged %q", got, want)
	}
	if g.activePane != 0 {
		t.Fatalf("activePane = %d, want 0", g.activePane)
	}
}

func TestMoveStackedPaneDown(t *testing.T) {
	g := newTestGroup(3, 0) // order a,b,c ; active = a (index 0)
	g.moveStackedPaneDown()
	if got, want := order(g), "bac"; got != want {
		t.Fatalf("order after down = %q, want %q", got, want)
	}
	if g.activePane != 1 {
		t.Fatalf("activePane = %d, want 1", g.activePane)
	}
	if g.paneRefs[g.activePane].id[:1] != "a" {
		t.Fatalf("active pane id = %q, want a", g.paneRefs[g.activePane].id[:1])
	}
}

func TestMoveStackedPaneDown_AtBackIsNoop(t *testing.T) {
	g := newTestGroup(3, 2) // active at back
	g.moveStackedPaneDown()
	if got, want := order(g), "abc"; got != want {
		t.Fatalf("order = %q, want unchanged %q", got, want)
	}
	if g.activePane != 2 {
		t.Fatalf("activePane = %d, want 2", g.activePane)
	}
}

func TestMoveStackedPane_SinglePaneIsNoop(t *testing.T) {
	g := newTestGroup(1, 0)
	g.moveStackedPaneUp()
	g.moveStackedPaneDown()
	if order(g) != "a" || g.activePane != 0 {
		t.Fatalf("single-pane group changed: order=%q active=%d", order(g), g.activePane)
	}
}

// TestMoveStackedPane_FullTraversal walks the active pane from back to front
// and back down, confirming the reorder is reversible.
func TestMoveStackedPane_FullTraversal(t *testing.T) {
	g := newTestGroup(3, 2) // a,b,c active=c
	g.moveStackedPaneUp()   // a,c,b active=1
	g.moveStackedPaneUp()   // c,a,b active=0
	if got := order(g); got != "cab" {
		t.Fatalf("after two ups: order=%q, want cab", got)
	}
	g.moveStackedPaneDown() // a,c,b active=1
	g.moveStackedPaneDown() // a,b,c active=2
	if got := order(g); got != "abc" {
		t.Fatalf("after two downs: order=%q, want abc", got)
	}
	if g.activePane != 2 {
		t.Fatalf("activePane = %d, want 2", g.activePane)
	}
}
