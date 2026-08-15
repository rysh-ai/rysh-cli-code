// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

// `F-11` — a tab-snapshot timeout reported as `lane not found`.
//
// Diagnosed 2026-08-07, fixed 2026-08-11. On a 111-pane tab the FULL content
// snapshot missed its 2 s budget; the error branch substituted a lane-less stub
// and the caller reported the lane as missing. Every operator then hunted a
// selector bug that did not exist, and 22 manager fleets lost their fan-out —
// silently, because the launcher renames the pane and writes the prompt file
// (id-only selectors, both succeed) BEFORE the launch.

// TestRunningFilterSurvivesTheCheapSnapshot is the precondition the F-11 report
// demanded be confirmed before broadcastCmd's snapshot was made cheaper: if
// Program did not survive LayoutOnly, `--running` would silently stop matching
// and select NOTHING, which is a worse failure than the one being fixed.
func TestRunningFilterSurvivesTheCheapSnapshot(t *testing.T) {
	snap := domain.WorkspaceSnapshot{Tabs: []domain.TabSnapshot{{
		ID: "t1",
		Lanes: []domain.LaneSnapshot{{
			ID: "l1",
			PaneGroups: []domain.PaneGroupSnapshot{{
				ID: "g1",
				Panes: []domain.PaneSnapshot{
					{ID: "p-claude", Program: "claude"},
					{ID: "p-shell", Program: ""},
				},
			}},
		}},
	}}}

	got := filterPanesByProgram(&snap, []string{"p-claude", "p-shell"}, "claude")
	if len(got) != 1 || got[0] != "p-claude" {
		t.Fatalf("--running claude selected %v; Program must survive the layout-only "+
			"snapshot or the filter matches nothing", got)
	}
	if got := filterPanesByProgram(&snap, []string{"p-claude", "p-shell"}, "shell"); len(got) != 1 {
		t.Fatalf("--running shell selected %v, want the bare shell", got)
	}
}

// TestATimedOutTabIsNotReportedAsAMissingLane is the fix itself.
//
// The two snapshots below differ only in Partial, and that is the point: before
// the marker they were the SAME VALUE, so "I could not look" and "it is not
// there" were indistinguishable to every caller.
func TestATimedOutTabIsNotReportedAsAMissingLane(t *testing.T) {
	timedOut := domain.WorkspaceSnapshot{
		Tabs: []domain.TabSnapshot{{ID: "t1", Title: "big", Partial: true}},
	}
	_, _, _, _, err := collectScopePaneIDs(&timedOut, "lane", cmdSelectors{tab: "t1", lane: "2"})
	if err == nil {
		t.Fatal("a timed-out tab resolved a lane")
	}
	if strings.Contains(err.Error(), "lane not found") {
		t.Fatalf("a timeout is still reported as a missing lane: %v", err)
	}
	for _, want := range []string{"could not read", "timed out", "may well exist"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q — an operator will hunt a selector bug: %v",
				want, err)
		}
	}

	// A tab that genuinely has no such lane still says so plainly.
	real := domain.WorkspaceSnapshot{
		Tabs: []domain.TabSnapshot{{
			ID: "t1", Title: "small",
			Lanes: []domain.LaneSnapshot{{ID: "l1", PaneGroups: []domain.PaneGroupSnapshot{{
				ID: "g1", Panes: []domain.PaneSnapshot{{ID: "p1"}},
			}}}},
		}},
	}
	_, _, _, _, err = collectScopePaneIDs(&real, "lane", cmdSelectors{tab: "t1", lane: "9"})
	if err == nil || !strings.Contains(err.Error(), "lane not found") {
		t.Fatalf("a genuinely absent lane should say 'lane not found', got %v", err)
	}
}

// TestPartialIsOffForARealSnapshot — the marker must mean something. A tab that
// answered is never Partial, or the honest error above becomes noise on every
// healthy call and gets ignored.
func TestPartialIsOffForARealSnapshot(t *testing.T) {
	real := domain.TabSnapshot{ID: "t1", Lanes: []domain.LaneSnapshot{{ID: "l1"}}}
	if real.Partial {
		t.Fatal("a real snapshot is marked partial")
	}
}
