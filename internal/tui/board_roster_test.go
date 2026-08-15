// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// TestSeedExcludesNonAgentPanes: the board and approval panes are not agents.
func TestSeedExcludesNonAgentPanes(t *testing.T) {
	mk := func(id, name, ptype string) domain.PaneSnapshot {
		return domain.PaneSnapshot{ID: id, GivenName: name, PaneType: ptype}
	}
	m := buildBoardModel(board.New(0))
	m.snapshot = domain.WorkspaceSnapshot{Tabs: []domain.TabSnapshot{{
		ID: "t", Lanes: []domain.LaneSnapshot{{ID: "l", PaneGroups: []domain.PaneGroupSnapshot{{
			ID: "g", Panes: []domain.PaneSnapshot{
				mk("p1", "planner-agent", domain.PaneTypeNormal),
				mk("p2", "builder-agent", domain.PaneTypeNormal),
				mk("p3", "the-board", domain.PaneTypeAgentsBoard),
				mk("p4", "approval", domain.PaneTypeApproval),
			},
		}}}},
	}}}
	m.seedRosterFromSnapshot()
	got := m.boardStore("").Roster()
	if len(got) != 2 {
		for _, r := range got {
			t.Logf("roster entry: pane=%s persona=%s", r.PaneID, r.Persona)
		}
		t.Fatalf("roster = %d entries, want 2 (board and approval panes must be excluded)", len(got))
	}
}

// TestSeedEvictsGhostPanes pins the defect the live board found: registrations
// are persistent (gate 2), so a pane that registered and then closed — or that
// belonged to an earlier session — keeps its roster entry forever. Observed as
// "3 agents" in a two-agent session, the third a pane id from a previous run.
//
// Live panes are authoritative for WHO EXISTS; registrations stay authoritative
// for WHAT THEY ARE CALLED.
func TestSeedEvictsGhostPanes(t *testing.T) {
	st := board.New(0)
	// A ghost: registered, but no longer present in any snapshot.
	st.Register(&msgpkg.MsgBoardRegister{
		V: msgpkg.BoardSchemaVersion, PaneID: "ghost-from-a-previous-session",
		Persona: "civil-sturgeon", TS: 1,
	})
	m := buildBoardModel(st)
	m.snapshot = domain.WorkspaceSnapshot{Tabs: []domain.TabSnapshot{{
		ID: "t", Lanes: []domain.LaneSnapshot{{ID: "l", PaneGroups: []domain.PaneGroupSnapshot{{
			ID: "g", Panes: []domain.PaneSnapshot{
				{ID: "p1", GivenName: "planner-agent", PaneType: domain.PaneTypeNormal},
				{ID: "p2", GivenName: "builder-agent", PaneType: domain.PaneTypeNormal},
			},
		}}}},
	}}}
	m.seedRosterFromSnapshot()

	got := m.boardStore("").Roster()
	if len(got) != 2 {
		for _, r := range got {
			t.Logf("roster entry: pane=%s persona=%s", r.PaneID, r.Persona)
		}
		t.Fatalf("roster = %d, want 2 — a pane that no longer exists must not be counted", len(got))
	}
	for _, r := range got {
		if r.Persona == "civil-sturgeon" {
			t.Error("the ghost survived reconciliation")
		}
	}
}

// An empty snapshot means "caller does not know", not "nobody is here" — a
// momentary empty snapshot must not wipe a good roster.
func TestEmptySnapshotDoesNotWipeTheRoster(t *testing.T) {
	st := board.New(0)
	st.Register(&msgpkg.MsgBoardRegister{
		V: msgpkg.BoardSchemaVersion, PaneID: "p1", Persona: "planner-agent", TS: 1,
	})
	m := buildBoardModel(st)
	m.snapshot = domain.WorkspaceSnapshot{}
	m.seedRosterFromSnapshot()
	if n := len(m.boardStore("").Roster()); n != 1 {
		t.Fatalf("roster = %d, want 1 — an empty snapshot must not clear the roster", n)
	}
}
