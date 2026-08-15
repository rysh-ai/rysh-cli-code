// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

// F-24 and F-27: two destructive verbs that reported success and did nothing.
//
// Neither had a test, and the reason is worth keeping: the rules lived inside
// CLI handlers that need a live actor system, so there was nowhere to assert
// them without standing up NATS. Both are now pure functions over snapshots and
// these tests need nothing but a struct literal.

func tab(id string, lanes ...domain.LaneSnapshot) *domain.TabSnapshot {
	return &domain.TabSnapshot{ID: id, Lanes: lanes}
}

func lane(id string, groups ...domain.PaneGroupSnapshot) domain.LaneSnapshot {
	return domain.LaneSnapshot{ID: id, PaneGroups: groups}
}

func group(id string, paneIDs ...string) domain.PaneGroupSnapshot {
	g := domain.PaneGroupSnapshot{ID: id}
	for _, p := range paneIDs {
		g.Panes = append(g.Panes, domain.PaneSnapshot{ID: p, Title: p})
	}
	return g
}

func siteOf(t *testing.T, snap *domain.TabSnapshot, paneID string) domain.PaneSite {
	t.Helper()
	s, ok := domain.LocatePaneInTab(snap, paneID)
	if !ok {
		t.Fatalf("pane %q is not in the fixture", paneID)
	}
	return s
}

// ---------------------------------------------------------------------------
// F-24
// ---------------------------------------------------------------------------

// The reported case: three `deleted` receipts over a pane that never moved.
// The pane is its group's only member, so PaneGroupActor.deletePaneByID drops
// the request — correctly, a group must not be left empty — and the handler had
// already decided to answer OK.
func TestDeletingAGroupsOnlyPaneIsRefusedRatherThanReportedDone(t *testing.T) {
	snap := tab("tab-1",
		lane("lane-1", group("grp-1", "pane-a")),
		lane("lane-2", group("grp-2", "pane-b", "pane-c")),
	)
	refusal := paneDeleteRefusal(siteOf(t, snap, "pane-a"), snap, 1)
	if refusal == "" {
		t.Fatal("a delete the pane group will silently drop was reported as done")
	}
	// A refusal nobody can act on is only a politer failure.
	if !strings.Contains(refusal, "pane-group delete") || !strings.Contains(refusal, "grp-1") {
		t.Errorf("the refusal does not name the verb that works, or the group id:\n%s", refusal)
	}
}

func TestDeletingOneOfSeveralPanesInAGroupIsStillAllowed(t *testing.T) {
	snap := tab("tab-1", lane("lane-1", group("grp-1", "pane-a", "pane-b")))
	if refusal := paneDeleteRefusal(siteOf(t, snap, "pane-a"), snap, 1); refusal != "" {
		t.Errorf("a deletable pane was refused: %s", refusal)
	}
}

// The pre-existing guard must keep its own, better message: this is the last
// pane anywhere, not merely the last in its group.
func TestTheVeryLastPaneKeepsItsOwnRefusal(t *testing.T) {
	snap := tab("tab-1", lane("lane-1", group("grp-1", "pane-a")))
	refusal := paneDeleteRefusal(siteOf(t, snap, "pane-a"), snap, 1)
	if refusal != "cannot delete the last pane" {
		t.Errorf("the last-pane guard lost its message: %q", refusal)
	}
}

// With a second tab the pane is no longer the last one in the session, so the
// group rule applies instead — and must still refuse rather than report done.
func TestASoleWithASecondTabIsRefusedByTheGroupRule(t *testing.T) {
	snap := tab("tab-1", lane("lane-1", group("grp-1", "pane-a")))
	refusal := paneDeleteRefusal(siteOf(t, snap, "pane-a"), snap, 2)
	if !strings.Contains(refusal, "pane-group delete") {
		t.Errorf("want the group refusal with a second tab present, got: %q", refusal)
	}
}

// ---------------------------------------------------------------------------
// F-27
// ---------------------------------------------------------------------------

// The reported case: a PANE uuid was handed to `pane-group delete` and it
// printed `deleted pane group 8e57f814…`. Nothing by that name exists as a
// group, and the verb has to say so instead of echoing the id back.
func TestAPaneIdIsNotAGroupId(t *testing.T) {
	snaps := []*domain.TabSnapshot{tab("tab-1", lane("lane-1", group("grp-1", "pane-a", "pane-b")))}
	if _, found := locatePaneGroup(snaps, "pane-a"); found {
		t.Error("a pane id resolved as a pane group — the id that names nothing must be refused")
	}
}

func TestARealGroupIsFoundWithItsLaneAndTab(t *testing.T) {
	snaps := []*domain.TabSnapshot{
		tab("tab-1", lane("lane-1", group("grp-1", "pane-a"))),
		tab("tab-2", lane("lane-2", group("grp-2", "pane-b", "pane-c"))),
	}
	site, found := locatePaneGroup(snaps, "grp-2")
	if !found {
		t.Fatal("an existing group was not found")
	}
	if site.TabID != "tab-2" || site.LaneID != "lane-2" || len(site.Group.Panes) != 2 {
		t.Errorf("wrong site: tab=%s lane=%s panes=%d", site.TabID, site.LaneID, len(site.Group.Panes))
	}
}

// The search is what establishes the target exists, so it must run over every
// tab rather than stopping at the first — a group in tab 2 is as real as one in
// tab 1, and the caller's --tab hint used to decide this by skipping the search
// altogether.
func TestTheSearchDoesNotStopAtTheFirstTab(t *testing.T) {
	snaps := []*domain.TabSnapshot{
		tab("tab-1", lane("lane-1", group("grp-1", "pane-a"))),
		nil, // an unreachable tab must not end the search
		tab("tab-3", lane("lane-3", group("grp-3", "pane-d"))),
	}
	if _, found := locatePaneGroup(snaps, "grp-3"); !found {
		t.Error("a group in a later tab was missed")
	}
}

func TestAnUnknownGroupIsNotFound(t *testing.T) {
	snaps := []*domain.TabSnapshot{tab("tab-1", lane("lane-1", group("grp-1", "pane-a")))}
	if _, found := locatePaneGroup(snaps, "grp-nope"); found {
		t.Error("an id that names nothing resolved to a group")
	}
}
