// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"reflect"
	"sort"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

func TestParseCmdArgs(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wScope   string
		wSel     cmdSelectors
		wCommand string
		wErr     bool
	}{
		{"simple lane", []string{"lane", "pwd"}, "lane", cmdSelectors{}, "pwd", false},
		{"pg alias", []string{"pg", "pwd"}, "panegroup", cmdSelectors{}, "pwd", false},
		{"stack alias", []string{"stack", "pwd"}, "panegroup", cmdSelectors{}, "pwd", false},
		{"ws alias", []string{"ws", "make"}, "workspace", cmdSelectors{}, "make", false},
		{"multiword command", []string{"tab", "git", "status", "-s"}, "tab", cmdSelectors{}, "git status -s", false},
		{"with selectors", []string{"lane", "--tab", "2", "--lane", "backend", "pwd"}, "lane", cmdSelectors{tab: "2", lane: "backend"}, "pwd", false},
		{"ws selector", []string{"tab", "--ws", "build", "ls"}, "tab", cmdSelectors{ws: "build"}, "ls", false},
		{"double-dash command with flags", []string{"lane", "--", "ls", "--color"}, "lane", cmdSelectors{}, "ls --color", false},
		{"command starting with verb keeps trailing flags", []string{"lane", "ls", "--color"}, "lane", cmdSelectors{}, "ls --color", false},
		{"no args", []string{}, "", cmdSelectors{}, "", true},
		{"unknown scope", []string{"galaxy", "pwd"}, "", cmdSelectors{}, "", true},
		{"missing command", []string{"lane"}, "", cmdSelectors{}, "", true},
		{"flag without value", []string{"lane", "--tab"}, "", cmdSelectors{}, "", true},
		{"unknown flag", []string{"lane", "--zzz", "x", "pwd"}, "", cmdSelectors{}, "", true},
	}
	for _, c := range cases {
		scope, sel, command, err := parseCmdArgs(c.args)
		if c.wErr {
			if err == nil {
				t.Errorf("%s: expected error, got scope=%q sel=%+v command=%q", c.name, scope, sel, command)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error: %v", c.name, err)
			continue
		}
		if scope != c.wScope || sel != c.wSel || command != c.wCommand {
			t.Errorf("%s: got (scope=%q sel=%+v command=%q), want (scope=%q sel=%+v command=%q)",
				c.name, scope, sel, command, c.wScope, c.wSel, c.wCommand)
		}
	}
}

// buildTestSnapshot constructs a 1-tab / 2-lane workspace:
//
//	tab-1 (active pane = L1G1P1)
//	  lane "left"  (active)  → group g1 [L1G1P1, L1G1P2], group g2 [L1G2P1]
//	  lane "right"           → group g3 [L2G1P1]
func buildTestSnapshot() *domain.WorkspaceSnapshot {
	return &domain.WorkspaceSnapshot{
		ActiveTabID:  "tab-1",
		ActivePaneID: "L1G1P1",
		Tabs: []domain.TabSnapshot{
			{
				ID:           "tab-1",
				Title:        "tab-1",
				ActivePaneID: "L1G1P1",
				Lanes: []domain.LaneSnapshot{
					{
						ID:           "lane-left",
						Name:         "left",
						ActivePaneID: "L1G1P1",
						PaneGroups: []domain.PaneGroupSnapshot{
							{ID: "g1", ActivePaneID: "L1G1P1", Panes: []domain.PaneSnapshot{{ID: "L1G1P1"}, {ID: "L1G1P2"}}},
							{ID: "g2", ActivePaneID: "L1G2P1", Panes: []domain.PaneSnapshot{{ID: "L1G2P1"}}},
						},
					},
					{
						ID:           "lane-right",
						Name:         "right",
						ActivePaneID: "L2G1P1",
						PaneGroups: []domain.PaneGroupSnapshot{
							{ID: "g3", ActivePaneID: "L2G1P1", Panes: []domain.PaneSnapshot{{ID: "L2G1P1"}}},
						},
					},
				},
			},
		},
	}
}

func TestCollectScopePaneIDs(t *testing.T) {
	snap := buildTestSnapshot()
	cases := []struct {
		name  string
		scope string
		sel   cmdSelectors
		want  []string
	}{
		{"active pane", "pane", cmdSelectors{}, []string{"L1G1P1"}},
		{"active group (full stack)", "panegroup", cmdSelectors{}, []string{"L1G1P1", "L1G1P2"}},
		{"active lane (all groups)", "lane", cmdSelectors{}, []string{"L1G1P1", "L1G1P2", "L1G2P1"}},
		{"active tab (all lanes)", "tab", cmdSelectors{}, []string{"L1G1P1", "L1G1P2", "L1G2P1", "L2G1P1"}},
		{"whole workspace", "workspace", cmdSelectors{}, []string{"L1G1P1", "L1G1P2", "L1G2P1", "L2G1P1"}},
		{"lane by name", "lane", cmdSelectors{lane: "right"}, []string{"L2G1P1"}},
		{"group by 1-based index", "panegroup", cmdSelectors{pg: "2"}, []string{"L1G2P1"}},
		{"pane by id", "pane", cmdSelectors{pane: "L1G1P2"}, []string{"L1G1P2"}},
	}
	for _, c := range cases {
		got, _, _, _, err := collectScopePaneIDs(snap, c.scope, c.sel)
		if err != nil {
			t.Errorf("%s: unexpected error: %v", c.name, err)
			continue
		}
		gs, ws := append([]string(nil), got...), append([]string(nil), c.want...)
		sort.Strings(gs)
		sort.Strings(ws)
		if !reflect.DeepEqual(gs, ws) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// TestCollectScopePaneIDs_ExcludesShared verifies that panes shared upstream
// with remote users are excluded from every broadcast scope and counted as
// skipped.
func TestCollectScopePaneIDs_ExcludesShared(t *testing.T) {
	snap := buildTestSnapshot()
	// Mark L1G1P2 (in the active lane/group) and L2G1P1 (the only pane of the
	// "right" lane) as shared.
	snap.Tabs[0].Lanes[0].PaneGroups[0].Panes[1].Sharing = true // L1G1P2
	snap.Tabs[0].Lanes[1].PaneGroups[0].Panes[0].Sharing = true // L2G1P1

	// tab scope: 4 panes total, 2 shared → 2 remain, 2 skipped (shared).
	ids, skipped, _, _, err := collectScopePaneIDs(snap, "tab", cmdSelectors{})
	if err != nil {
		t.Fatalf("tab: unexpected error: %v", err)
	}
	if skipped != 2 {
		t.Errorf("tab: skipped=%d, want 2", skipped)
	}
	gs := append([]string(nil), ids...)
	sort.Strings(gs)
	if want := []string{"L1G1P1", "L1G2P1"}; !reflect.DeepEqual(gs, want) {
		t.Errorf("tab: ids=%v, want %v", gs, want)
	}

	// pane scope explicitly targeting a shared pane → 0 targets, 1 skipped.
	ids, skipped, _, _, err = collectScopePaneIDs(snap, "pane", cmdSelectors{pane: "L1G1P2"})
	if err != nil {
		t.Fatalf("pane: unexpected error: %v", err)
	}
	if len(ids) != 0 || skipped != 1 {
		t.Errorf("pane: ids=%v skipped=%d, want [] / 1", ids, skipped)
	}

	// lane "right" has only a shared pane → 0 targets, 1 skipped.
	ids, skipped, _, _, err = collectScopePaneIDs(snap, "lane", cmdSelectors{lane: "right"})
	if err != nil {
		t.Fatalf("lane: unexpected error: %v", err)
	}
	if len(ids) != 0 || skipped != 1 {
		t.Errorf("lane right: ids=%v skipped=%d, want [] / 1", ids, skipped)
	}
}

// TestCollectScopePaneIDs_ExcludesPipeline verifies that panes in a
// pipeline-enabled tab are excluded from broadcasts and counted as
// skippedPipeline.
func TestCollectScopePaneIDs_ExcludesPipeline(t *testing.T) {
	snap := buildTestSnapshot()
	snap.Tabs[0].PipelineEnabled = true // tab-1 is now a pipeline (4 panes)

	// tab scope → 0 targets, all 4 counted as pipeline-skipped.
	ids, shared, pipeline, _, err := collectScopePaneIDs(snap, "tab", cmdSelectors{})
	if err != nil {
		t.Fatalf("tab: unexpected error: %v", err)
	}
	if len(ids) != 0 || shared != 0 || pipeline != 4 {
		t.Errorf("tab: ids=%v shared=%d pipeline=%d, want [] / 0 / 4", ids, shared, pipeline)
	}

	// lane scope within the pipeline tab → 0 targets, pipeline-skipped.
	ids, _, pipeline, _, err = collectScopePaneIDs(snap, "lane", cmdSelectors{lane: "left"})
	if err != nil {
		t.Fatalf("lane: unexpected error: %v", err)
	}
	if len(ids) != 0 || pipeline != 3 { // lane "left" has 3 panes
		t.Errorf("lane: ids=%v pipeline=%d, want [] / 3", ids, pipeline)
	}

	// pane scope in the pipeline tab → 0 targets, 1 pipeline-skipped.
	ids, _, pipeline, _, err = collectScopePaneIDs(snap, "pane", cmdSelectors{})
	if err != nil {
		t.Fatalf("pane: unexpected error: %v", err)
	}
	if len(ids) != 0 || pipeline != 1 {
		t.Errorf("pane: ids=%v pipeline=%d, want [] / 1", ids, pipeline)
	}
}

// TestCollectScopePaneIDs_WorkspaceMixedPipeline verifies that, for the
// workspace scope, panes in a pipeline tab are excluded while panes in
// non-pipeline tabs are still targeted.
func TestCollectScopePaneIDs_WorkspaceMixedPipeline(t *testing.T) {
	snap := buildTestSnapshot()
	// Add a second, non-pipeline tab with one lane/group/pane.
	snap.Tabs = append(snap.Tabs, domain.TabSnapshot{
		ID: "tab-2", Title: "tab-2", ActivePaneID: "T2P1",
		Lanes: []domain.LaneSnapshot{{
			ID: "lane-x", ActivePaneID: "T2P1",
			PaneGroups: []domain.PaneGroupSnapshot{{ID: "gx", ActivePaneID: "T2P1", Panes: []domain.PaneSnapshot{{ID: "T2P1"}}}},
		}},
	})
	snap.Tabs[0].PipelineEnabled = true // tab-1 (4 panes) is a pipeline

	ids, shared, pipeline, _, err := collectScopePaneIDs(snap, "workspace", cmdSelectors{})
	if err != nil {
		t.Fatalf("workspace: unexpected error: %v", err)
	}
	if want := []string{"T2P1"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("workspace: ids=%v, want %v", ids, want)
	}
	if shared != 0 || pipeline != 4 {
		t.Errorf("workspace: shared=%d pipeline=%d, want 0 / 4", shared, pipeline)
	}
}

func TestResolveCmdWorkspace(t *testing.T) {
	w := &WorkspaceActor{workspaceName: "main", workspaceNames: []string{"main", "build", "infra"}}
	cases := []struct {
		name        string
		arg         string
		wantTarget  string
		wantSibling bool
		wantErr     bool
	}{
		{"empty → local", "", "", false, false},
		{"own name → local", "main", "", false, false},
		{"own 1-based index → local", "1", "", false, false},
		{"sibling by name", "build", "build", true, false},
		{"sibling by index", "3", "infra", true, false},
		{"unknown name", "ghost", "", false, true},
		{"out-of-range index", "9", "", false, true},
	}
	for _, c := range cases {
		target, sibling, err := w.resolveCmdWorkspace(c.arg)
		if c.wantErr {
			if err == nil {
				t.Errorf("%s: expected error, got (%q, %v)", c.name, target, sibling)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error: %v", c.name, err)
			continue
		}
		if target != c.wantTarget || sibling != c.wantSibling {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", c.name, target, sibling, c.wantTarget, c.wantSibling)
		}
	}
}

func TestCollectScopePaneIDs_NotFound(t *testing.T) {
	snap := buildTestSnapshot()
	if _, _, _, _, err := collectScopePaneIDs(snap, "lane", cmdSelectors{lane: "nonexistent"}); err == nil {
		t.Errorf("expected error for unknown lane")
	}
	if _, _, _, _, err := collectScopePaneIDs(snap, "tab", cmdSelectors{tab: "99"}); err == nil {
		t.Errorf("expected error for out-of-range tab index")
	}
}

// buildTwoTabSnapshot models the reported bug: the active-tab marker points at
// "main" (which the user is NOT working in) while the command is issued from a
// pane ("R1") that lives in a second tab "other".
//
//	main  (ActiveTabID)  → lane "m" → group "mg" [M1]
//	other                → lane "o" → group "og" [R1, R2]
func buildTwoTabSnapshot() *domain.WorkspaceSnapshot {
	return &domain.WorkspaceSnapshot{
		ActiveTabID:  "main",
		ActivePaneID: "M1",
		Tabs: []domain.TabSnapshot{
			{
				ID: "main", Title: "main", ActivePaneID: "M1",
				Lanes: []domain.LaneSnapshot{{
					ID: "m", Name: "m", ActivePaneID: "M1",
					PaneGroups: []domain.PaneGroupSnapshot{
						{ID: "mg", ActivePaneID: "M1", Panes: []domain.PaneSnapshot{{ID: "M1"}}},
					},
				}},
			},
			{
				ID: "other", Title: "other", ActivePaneID: "R1",
				Lanes: []domain.LaneSnapshot{{
					ID: "o", Name: "o", ActivePaneID: "R1",
					PaneGroups: []domain.PaneGroupSnapshot{
						{ID: "og", ActivePaneID: "R1", Panes: []domain.PaneSnapshot{{ID: "R1"}, {ID: "R2"}}},
					},
				}},
			},
		},
	}
}

// TestAnchorSnapshotToPane is the regression test for `##cmd` running in the
// wrong tab: without anchoring, an omitted --tab selector falls back to
// ActiveTabID ("main"), so the command hits the tab the user is not in. After
// anchoring to the originating pane ("R1"), the active-entity default resolves
// to that pane's tab/lane/group ("other") instead.
func TestAnchorSnapshotToPane(t *testing.T) {
	// Baseline: with no anchor, the default tab scope targets "main".
	snap := buildTwoTabSnapshot()
	ids, _, _, _, err := collectScopePaneIDs(snap, "tab", cmdSelectors{})
	if err != nil {
		t.Fatalf("baseline tab: %v", err)
	}
	if !reflect.DeepEqual(ids, []string{"M1"}) {
		t.Fatalf("baseline tab scope = %v, want [M1] (the stale active tab)", ids)
	}

	// Anchor to R1 (in "other"): the default tab scope must now target "other".
	snap = buildTwoTabSnapshot()
	anchorSnapshotToPane(snap, "R1")
	if snap.ActiveTabID != "other" {
		t.Fatalf("after anchor ActiveTabID = %q, want \"other\"", snap.ActiveTabID)
	}
	ids, _, _, _, err = collectScopePaneIDs(snap, "tab", cmdSelectors{})
	if err != nil {
		t.Fatalf("anchored tab: %v", err)
	}
	gs := append([]string(nil), ids...)
	sort.Strings(gs)
	if want := []string{"R1", "R2"}; !reflect.DeepEqual(gs, want) {
		t.Errorf("anchored tab scope = %v, want %v", gs, want)
	}

	// lane / panegroup / pane scopes also follow the anchored pane.
	ids, _, _, _, err = collectScopePaneIDs(snap, "lane", cmdSelectors{})
	if err != nil {
		t.Fatalf("anchored lane: %v", err)
	}
	gs = append([]string(nil), ids...)
	sort.Strings(gs)
	if !reflect.DeepEqual(gs, []string{"R1", "R2"}) {
		t.Errorf("anchored lane scope = %v, want [R1 R2]", gs)
	}
	ids, _, _, _, err = collectScopePaneIDs(snap, "pane", cmdSelectors{})
	if err != nil {
		t.Fatalf("anchored pane: %v", err)
	}
	if !reflect.DeepEqual(ids, []string{"R1"}) {
		t.Errorf("anchored pane scope = %v, want [R1]", ids)
	}

	// An explicit --tab selector still wins over the anchor.
	snap = buildTwoTabSnapshot()
	anchorSnapshotToPane(snap, "R1")
	ids, _, _, _, err = collectScopePaneIDs(snap, "tab", cmdSelectors{tab: "main"})
	if err != nil {
		t.Fatalf("explicit tab: %v", err)
	}
	if !reflect.DeepEqual(ids, []string{"M1"}) {
		t.Errorf("explicit --tab main = %v, want [M1]", ids)
	}

	// Empty anchor (cross-workspace broadcast) leaves the snapshot untouched.
	snap = buildTwoTabSnapshot()
	anchorSnapshotToPane(snap, "")
	if snap.ActiveTabID != "main" {
		t.Errorf("empty anchor changed ActiveTabID to %q, want \"main\"", snap.ActiveTabID)
	}
	// An anchor pane that does not exist is also a no-op.
	anchorSnapshotToPane(snap, "ghost")
	if snap.ActiveTabID != "main" {
		t.Errorf("ghost anchor changed ActiveTabID to %q, want \"main\"", snap.ActiveTabID)
	}
}
