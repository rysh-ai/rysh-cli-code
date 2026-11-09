// SPDX-License-Identifier: Apache-2.0

package tui

// Regression tests for the busy-session claude→shell flip.
//
// Reported: with ~70 claude panes in one session, the claude pane being worked
// in kept switching to its shell view and back. A pane whose actor missed the
// group's 1s snapshot deadline was reported as a zero-valued placeholder —
// RawMode=false — and syncRawMode took that as "the program exited". The daemon
// now marks such entries Stale; carryStalePanes keeps the pane as last seen and
// syncRawMode does not act on a Stale pane it could not replace.

import (
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

func placeholder(id string) domain.PaneSnapshot {
	return domain.PaneSnapshot{ID: id, Title: id, Status: "unreachable", Output: "snapshot error: timeout", Stale: true}
}

// staleRawModel is a claude pane already in the live view, sharing its group with a
// sibling so syncRawMode stays on in-pane rendering (no relay).
func staleRawModel(t *testing.T) Model {
	t.Helper()
	b := startTestBus(t)
	m := paneModel(domain.PaneSnapshot{ID: "p1", RawMode: true, FullScreen: true, Program: "claude"})
	m.snapshot.ActiveTabID = "t1"
	m.snapshot.Tabs[0].Lanes[0].PaneGroups[0].Panes = append(m.snapshot.Tabs[0].Lanes[0].PaneGroups[0].Panes,
		domain.PaneSnapshot{ID: "p2"})
	m.mode = modeRaw
	m.bus = b
	m.pub = b.Publisher()
	m.syncPaneInputs()
	return m
}

func withPanes(m Model, panes ...domain.PaneSnapshot) domain.WorkspaceSnapshot {
	next := m.snapshot
	next.Tabs = []domain.TabSnapshot{{
		ID: "t1",
		Lanes: []domain.LaneSnapshot{{
			ID:         "l1",
			PaneGroups: []domain.PaneGroupSnapshot{{ID: "g1", ActivePaneID: "p1", Panes: panes}},
		}},
	}}
	return next
}

func TestStalePaneKeepsTheLiveView(t *testing.T) {
	m := staleRawModel(t)
	m.snapshot = carryStalePanes(m.snapshot, withPanes(m, placeholder("p1"), domain.PaneSnapshot{ID: "p2"}))
	if p := m.findPaneInSnapshot("p1"); p == nil || !p.RawMode || p.Stale {
		t.Fatalf("stale p1 should carry its previous state, got %+v", p)
	}
	m.syncRawMode()
	if m.mode != modeRaw {
		t.Fatalf("a pane that missed the snapshot deadline must not leave the live view; mode = %v", m.mode)
	}
}

func TestWholeStaleGroupCarriesItsSiblings(t *testing.T) {
	m := staleRawModel(t)
	// The lane-level timeout: one placeholder for the whole group.
	m.snapshot = carryStalePanes(m.snapshot, withPanes(m, placeholder("p1")))
	panes := m.snapshot.Tabs[0].Lanes[0].PaneGroups[0].Panes
	if len(panes) != 2 || panes[1].ID != "p2" {
		t.Fatalf("a timed-out group should keep its previous panes, got %+v", panes)
	}
}

func TestRealExitStillLeavesTheLiveView(t *testing.T) {
	m := staleRawModel(t)
	m.snapshot = carryStalePanes(m.snapshot, withPanes(m, domain.PaneSnapshot{ID: "p1"}, domain.PaneSnapshot{ID: "p2"}))
	m.syncRawMode()
	if m.mode != modeNormal {
		t.Fatalf("an answered RawMode=false is a real exit and must leave modeRaw; mode = %v", m.mode)
	}
}

func TestUnseenStalePaneDrivesNoTransition(t *testing.T) {
	m := staleRawModel(t)
	// A previous snapshot that never had p1: nothing to carry.
	m.snapshot = carryStalePanes(domain.WorkspaceSnapshot{}, withPanes(m, placeholder("p1"), domain.PaneSnapshot{ID: "p2"}))
	m.syncRawMode()
	if m.mode != modeRaw {
		t.Fatalf("a stale pane with no earlier state must not drive an exit; mode = %v", m.mode)
	}
}

func TestCarryStalePanesDoesNotMutateInput(t *testing.T) {
	prev := paneModel(domain.PaneSnapshot{ID: "p1", RawMode: true}).snapshot
	next := paneModel(placeholder("p1")).snapshot
	_ = carryStalePanes(prev, next)
	if !next.Tabs[0].Lanes[0].PaneGroups[0].Panes[0].Stale {
		t.Fatal("carryStalePanes wrote through to the caller's snapshot")
	}
}
