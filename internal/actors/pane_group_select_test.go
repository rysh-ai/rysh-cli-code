package actors

import "testing"

// TestStackedPaneSelect_JumpsToIndex confirms selecting a 0-based index makes
// that pane active without reordering the stack (paneRefs stay in creation order).
func TestStackedPaneSelect_JumpsToIndex(t *testing.T) {
	g := newTestGroup(4, 0) // order a,b,c,d ; active = a (index 0)
	g.stackedPaneSelect(2)  // jump to the pane shown as [3/4]
	if g.activePane != 2 {
		t.Fatalf("activePane = %d, want 2", g.activePane)
	}
	if got := order(g); got != "abcd" {
		t.Fatalf("order = %q, want unchanged abcd", got)
	}
	if g.paneRefs[g.activePane].id[:1] != "c" {
		t.Fatalf("active pane id = %q, want c", g.paneRefs[g.activePane].id[:1])
	}
}

// TestStackedPaneSelect_SameIndexIsStable selecting the already-active index is a no-op.
func TestStackedPaneSelect_SameIndexIsStable(t *testing.T) {
	g := newTestGroup(3, 1)
	g.stackedPaneSelect(1)
	if g.activePane != 1 || order(g) != "abc" {
		t.Fatalf("active=%d order=%q, want active=1 order=abc", g.activePane, order(g))
	}
}

// TestStackedPaneSelect_OutOfRangeIsNoop indices below 0 or >= len are ignored,
// leaving the active pane unchanged.
func TestStackedPaneSelect_OutOfRangeIsNoop(t *testing.T) {
	g := newTestGroup(3, 1) // active = b (index 1)
	g.stackedPaneSelect(-1)
	g.stackedPaneSelect(3) // == len, out of range
	g.stackedPaneSelect(99)
	if g.activePane != 1 {
		t.Fatalf("activePane = %d, want unchanged 1", g.activePane)
	}
}

// TestStackedPaneSelect_FirstAndLast covers the boundary indices.
func TestStackedPaneSelect_FirstAndLast(t *testing.T) {
	g := newTestGroup(3, 1)
	g.stackedPaneSelect(0)
	if g.activePane != 0 {
		t.Fatalf("activePane = %d, want 0", g.activePane)
	}
	g.stackedPaneSelect(2)
	if g.activePane != 2 {
		t.Fatalf("activePane = %d, want 2", g.activePane)
	}
}

// TestStackedPaneSelect_SinglePane selecting index 0 in a single-pane group is a no-op
// and out-of-range is ignored.
func TestStackedPaneSelect_SinglePane(t *testing.T) {
	g := newTestGroup(1, 0)
	g.stackedPaneSelect(0)
	g.stackedPaneSelect(1)
	if g.activePane != 0 || order(g) != "a" {
		t.Fatalf("single-pane group changed: active=%d order=%q", g.activePane, order(g))
	}
}
