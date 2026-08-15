// SPDX-License-Identifier: Apache-2.0

package domain

import "testing"

// Hidden panes (design 027 §5.1) — the rendering half of the rule.
//
// The claim these tests carry is narrow and worth stating: a hidden pane is
// gone from what the TUI DRAWS and from nothing else. PanesInTab, which is what
// ANSA addresses over, must still see it — a board claude nobody can message is
// not a hidden agent, it is a broken one.

func tabWith(panes ...PaneSnapshot) TabSnapshot {
	active := ""
	if len(panes) > 0 {
		active = panes[0].ID
	}
	return TabSnapshot{
		Lanes: []LaneSnapshot{{
			ID: "lane-1",
			PaneGroups: []PaneGroupSnapshot{{
				ID:           "group-1",
				ActivePaneID: active,
				Panes:        panes,
			}},
		}},
	}
}

func TestHiddenPaneIsNotDrawn(t *testing.T) {
	ts := tabWith(
		PaneSnapshot{ID: "visible-1"},
		PaneSnapshot{ID: "board-agent", Hidden: true},
		PaneSnapshot{ID: "visible-2"},
	)

	for _, p := range ts.FlatPanes() {
		if p.ID == "board-agent" {
			t.Fatalf("FlatPanes drew a hidden pane: %+v", p)
		}
	}
	for _, lane := range ts.FlatLanes() {
		for _, p := range lane.VisiblePanes {
			if p.ID == "board-agent" {
				t.Fatalf("FlatLanes drew a hidden pane: %+v", p)
			}
		}
	}
	if got := len(ts.FlatPanes()); got != 2 {
		t.Fatalf("want 2 drawn panes, got %d", got)
	}
}

// The property that makes hiding safe rather than destructive.
func TestHiddenPaneIsStillAddressable(t *testing.T) {
	ts := tabWith(
		PaneSnapshot{ID: "visible-1"},
		PaneSnapshot{ID: "board-agent", Hidden: true},
	)

	seen := map[string]bool{}
	for p := range PanesInTab(&ts) {
		seen[p.ID] = true
	}
	if !seen["board-agent"] {
		t.Fatal("PanesInTab dropped a hidden pane — ANSA could no longer reach the board claude")
	}
}

// A stack that reports [1/2] while drawing one pane is advertising a pane the
// human cannot get to.
func TestHiddenPanesDoNotCountTowardTheStack(t *testing.T) {
	ts := tabWith(
		PaneSnapshot{ID: "visible-1"},
		PaneSnapshot{ID: "board-agent", Hidden: true},
		PaneSnapshot{ID: "visible-2"},
	)

	for _, p := range ts.FlatPanes() {
		if p.StackTotal != 2 {
			t.Fatalf("pane %s reports StackTotal=%d, want 2", p.ID, p.StackTotal)
		}
	}
}

// StackPosition is what the title bar prints as [n/N] and what a human types
// back to select a pane, so it has to count the same panes the renderer draws.
func TestStackPositionsRenumberAroundAHiddenPane(t *testing.T) {
	ts := tabWith(
		PaneSnapshot{ID: "visible-1"},
		PaneSnapshot{ID: "board-agent", Hidden: true},
		PaneSnapshot{ID: "visible-2"},
	)

	want := map[string]int{"visible-1": 0, "visible-2": 1}
	for _, p := range ts.FlatPanes() {
		if want[p.ID] != p.StackPosition {
			t.Fatalf("pane %s has StackPosition=%d, want %d", p.ID, p.StackPosition, want[p.ID])
		}
	}
}

// A session with no hidden panes must render exactly as it did before the
// field existed.
func TestNothingChangesWhenNothingIsHidden(t *testing.T) {
	ts := tabWith(PaneSnapshot{ID: "a"}, PaneSnapshot{ID: "b"})

	panes := ts.FlatPanes()
	if len(panes) != 2 {
		t.Fatalf("want 2 panes, got %d", len(panes))
	}
	if panes[0].StackCollapsed {
		t.Fatal("the active pane must not be collapsed")
	}
	if !panes[1].StackCollapsed {
		t.Fatal("an inactive stacked pane must be collapsed")
	}
	if panes[1].StackTotal != 2 {
		t.Fatalf("want StackTotal 2, got %d", panes[1].StackTotal)
	}
}
