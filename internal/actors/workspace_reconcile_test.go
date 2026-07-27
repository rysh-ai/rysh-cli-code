package actors

import (
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// oneTab builds a minimal single-pane tab snapshot for reconcile tests.
func oneTab(tabID, paneID string) domain.TabSnapshot {
	return domain.TabSnapshot{
		ID: tabID,
		Lanes: []domain.LaneSnapshot{{
			ID: "lane-" + tabID,
			PaneGroups: []domain.PaneGroupSnapshot{{
				ID:    "grp-" + tabID,
				Panes: []domain.PaneSnapshot{{ID: paneID}},
			}},
		}},
	}
}

// TestReconcileActiveTabFromSnapshots covers the pure (snapshot-slice) reconcile
// used by collectSnapshot. The focused pane is authoritative: activeTabIdx must
// follow it to the tab that holds it, and the reconcile is a no-op for mirror
// selections, missing focus, and unknown panes.
func TestReconcileActiveTabFromSnapshots(t *testing.T) {
	tabs := []domain.TabSnapshot{oneTab("A", "pA"), oneTab("B", "pB")}

	// Drift: activeTabIdx points at B but the focused pane is in A → heal to 0.
	w := &WorkspaceActor{
		tabs:         []*tabInfo{{id: "A"}, {id: "B"}},
		activeTabIdx: 1,
		activePaneID: "pA",
	}
	w.reconcileActiveTabFromSnapshots(tabs)
	if w.activeTabIdx != 0 {
		t.Errorf("focused pane in A: activeTabIdx = %d, want 0", w.activeTabIdx)
	}

	// Focused pane in B, stale index 0 → heal to 1.
	w.activeTabIdx = 0
	w.activePaneID = "pB"
	w.reconcileActiveTabFromSnapshots(tabs)
	if w.activeTabIdx != 1 {
		t.Errorf("focused pane in B: activeTabIdx = %d, want 1", w.activeTabIdx)
	}

	// No drift: already consistent → unchanged.
	w.activeTabIdx = 1
	w.activePaneID = "pB"
	w.reconcileActiveTabFromSnapshots(tabs)
	if w.activeTabIdx != 1 {
		t.Errorf("no drift: activeTabIdx = %d, want 1", w.activeTabIdx)
	}

	// Empty focus → no-op (leave the selection alone).
	w.activeTabIdx = 1
	w.activePaneID = ""
	w.reconcileActiveTabFromSnapshots(tabs)
	if w.activeTabIdx != 1 {
		t.Errorf("empty focus: activeTabIdx = %d, want 1 (unchanged)", w.activeTabIdx)
	}

	// Unknown pane → no-op.
	w.activeTabIdx = 1
	w.activePaneID = "ghost"
	w.reconcileActiveTabFromSnapshots(tabs)
	if w.activeTabIdx != 1 {
		t.Errorf("unknown pane: activeTabIdx = %d, want 1 (unchanged)", w.activeTabIdx)
	}

	// Mirror tab active → never touch the (mirror) selection, even though the
	// activePaneID would match a real tab.
	w.tabs = []*tabInfo{{id: "A"}, {id: "B"}}
	w.mirrorTabs = []*mirrorTab{{shareID: "s"}}
	w.activeTabIdx = len(w.tabs) // selects the first mirror tab
	w.activePaneID = "pA"
	w.reconcileActiveTabFromSnapshots(tabs)
	if w.activeTabIdx != len(w.tabs) {
		t.Errorf("mirror active: activeTabIdx = %d, want %d (unchanged)", w.activeTabIdx, len(w.tabs))
	}
}

// TestReconcileActiveTab covers the dispatch-time reconcile (used by
// handleSubmitInput), which resolves the focused pane's tab over a live NATS bus.
func TestReconcileActiveTab(t *testing.T) {
	nc := startInProcessNATS(t)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)

	tabSnapshotResponder(t, nc, codecs, "tab-A", "pA")
	tabSnapshotResponder(t, nc, codecs, "tab-B", "pB")

	w := &WorkspaceActor{
		pub:          pub,
		tabs:         []*tabInfo{{id: "tab-A"}, {id: "tab-B"}},
		activeTabIdx: 1, // drifted: points at B
		activePaneID: "pA",
	}

	// Drift heals: focused pane pA lives in tab-A (index 0).
	w.reconcileActiveTab()
	if w.activeTabIdx != 0 {
		t.Fatalf("after reconcile activeTabIdx = %d, want 0", w.activeTabIdx)
	}

	// No drift (fast path): already consistent → unchanged.
	w.activeTabIdx = 0
	w.activePaneID = "pA"
	w.reconcileActiveTab()
	if w.activeTabIdx != 0 {
		t.Fatalf("no-drift fast path: activeTabIdx = %d, want 0", w.activeTabIdx)
	}

	// Focused pane in B with stale index 0 → heal to 1.
	w.activeTabIdx = 0
	w.activePaneID = "pB"
	w.reconcileActiveTab()
	if w.activeTabIdx != 1 {
		t.Fatalf("focused pane in B: activeTabIdx = %d, want 1", w.activeTabIdx)
	}

	// Unknown pane → no-op.
	w.activeTabIdx = 1
	w.activePaneID = "ghost"
	w.reconcileActiveTab()
	if w.activeTabIdx != 1 {
		t.Fatalf("unknown pane: activeTabIdx = %d, want 1 (unchanged)", w.activeTabIdx)
	}
}
