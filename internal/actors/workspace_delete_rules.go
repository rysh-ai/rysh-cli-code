// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

// The two delete verbs used to answer OK for work they had not done.
//
// `##pane delete <id>` printed `deleted` three times over a pane that never
// moved (F-24), and `pane-group delete --id <a PANE id>` printed
// `deleted pane group <id>` for a target that does not exist as a group (F-27).
// Same disease as F-16 (`##pane info` answering about the wrong pane) and F-22
// (a manifest record with no message): a receipt with nothing behind it — and
// worse here, because these are DESTRUCTIVE verbs, so a teardown script that
// trusts them leaks exactly what it believes it removed. The petstore recipe
// already tells operators to verify removals against `##ansa who` rather than
// the receipt, which is a workaround standing in for this fix.
//
// Both rules live here as pure functions over snapshots, deliberately:
//
//  1. They are decisions, not plumbing, and the handlers that used to hold them
//     need a live actor system to run — so the rules could not be tested where
//     they were, which is why neither defect had a test.
//  2. ONE implementation each. The `[n/N]` split (design 027 §5.1) and F-18 are
//     both what happens when the same rule gets a second copy.

// paneDeleteRefusal reports why `pane delete` cannot remove this pane, or "".
//
// The refusal is not a policy invented here — it mirrors what the pane group
// actually does. PaneGroupActor.deletePaneByID drops the request when the pane
// is its group's last member, because a group must never be left empty. That
// drop is correct and it is SILENT, so the only defect is claiming otherwise:
// the CLI handler had already committed to OK: true before the group ever saw
// the message.
//
// It refuses rather than cascading into a group delete. Cascading would let
// `pane delete` remove a lane, and on the last group in the last lane a tab —
// a blast radius the caller did not ask for and cannot see from the verb they
// typed. Naming the verb that does work is the smaller, honest answer.
func paneDeleteRefusal(site domain.PaneSite, tabSnap *domain.TabSnapshot, tabCount int) string {
	lastEverywhere := len(site.Group.Panes) <= 1 &&
		len(site.Lane.PaneGroups) <= 1 &&
		len(tabSnap.Lanes) <= 1
	if lastEverywhere && tabCount <= 1 {
		return "cannot delete the last pane"
	}
	if len(site.Group.Panes) <= 1 {
		return fmt.Sprintf(
			"pane %q is the only pane in pane group %s, and a pane group cannot be left empty — "+
				"the delete would be silently dropped. Delete the group instead: "+
				"rysh pane-group delete --id %s",
			site.Pane.ID, site.Group.ID, site.Group.ID)
	}
	return ""
}

// paneGroupSite is where a pane group was found.
type paneGroupSite struct {
	TabID  string
	LaneID string
	Group  *domain.PaneGroupSnapshot
}

// locatePaneGroup finds a pane group across tabs, or reports that it does not
// exist.
//
// The caller may pass --tab and --lane, and the handler used to treat those as
// permission to skip the search entirely. So an id that named no group was
// published to the lane, dropped there with nothing to match, and reported as
// deleted. The hints are worth nothing as authorisation: only the search can
// say whether the target is real, so it always runs and the hints are used to
// CHECK the answer rather than to replace it.
func locatePaneGroup(tabs []*domain.TabSnapshot, groupID string) (paneGroupSite, bool) {
	for _, t := range tabs {
		if t == nil {
			continue
		}
		for lane, g := range domain.GroupsInTab(t) {
			if g.ID == groupID {
				return paneGroupSite{TabID: t.ID, LaneID: lane.ID, Group: g}, true
			}
		}
	}
	return paneGroupSite{}, false
}
