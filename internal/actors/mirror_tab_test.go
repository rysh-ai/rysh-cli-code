// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func sampleTab() domain.TabSnapshot {
	return domain.TabSnapshot{
		ID:           "tab-1",
		Title:        "work",
		ActivePaneID: "p1",
		Lanes: []domain.LaneSnapshot{
			{
				ID:           "lane-1",
				Flex:         7,
				Name:         "left",
				ActivePaneID: "p1",
				PaneGroups: []domain.PaneGroupSnapshot{
					{
						ID:           "grp-1",
						RowFlex:      10,
						ActivePaneID: "p1",
						Panes: []domain.PaneSnapshot{
							{
								ID:           "p1",
								Title:        "editor",
								GivenName:    "edit",
								Mode:         "shell",
								Status:       "idle",
								LastCommand:  "ls",
								ProviderName: "claude",
								Output:       "hello\nworld\n",
								// non-interactive pane: VTScreen dropped; ShellHistory carried.
								VTScreen:     []string{"raw"},
								ShellHistory: []string{"a", "b"},
							},
						},
					},
				},
			},
			{
				ID:           "lane-2",
				Flex:         3,
				ActivePaneID: "p2",
				PaneGroups: []domain.PaneGroupSnapshot{
					{
						ID:           "grp-2",
						RowFlex:      10,
						ActivePaneID: "p2",
						Panes: []domain.PaneSnapshot{
							{ID: "p2", Title: "logs", Mode: "shell", Output: "log line\n"},
						},
					},
				},
			},
		},
	}
}

func TestTrimTabForMirrorKeepsStructureDropsHeavyFields(t *testing.T) {
	out := trimTabForMirror(sampleTab())

	if out.ID != "tab-1" || out.Title != "work" || out.ActivePaneID != "p1" {
		t.Fatalf("tab identity not preserved: %+v", out)
	}
	if len(out.Lanes) != 2 {
		t.Fatalf("want 2 lanes, got %d", len(out.Lanes))
	}
	if out.Lanes[0].Flex != 7 || out.Lanes[0].Name != "left" {
		t.Errorf("lane flex/name not preserved: %+v", out.Lanes[0])
	}
	p := out.Lanes[0].PaneGroups[0].Panes[0]
	if p.ID != "p1" || p.Title != "editor" || p.GivenName != "edit" {
		t.Errorf("pane identity not preserved: %+v", p)
	}
	if p.Output != "hello\nworld\n" {
		t.Errorf("output not preserved: %q", p.Output)
	}
	// Heavy non-portable fields must be cleared.
	if len(p.VTScreen) != 0 {
		t.Errorf("VTScreen should be dropped, got %v", p.VTScreen)
	}
	if p.RawMode {
		t.Errorf("RawMode should be dropped")
	}
	// Command history IS carried (capped) so the subscriber can recall commands.
	if len(p.ShellHistory) != 2 || p.ShellHistory[0] != "a" {
		t.Errorf("ShellHistory should be carried, got %v", p.ShellHistory)
	}
}

func TestTailString(t *testing.T) {
	if got := tailString("short", 100); got != "short" {
		t.Errorf("short string changed: %q", got)
	}
	// Larger than cap: trims to next newline boundary.
	in := "line1\nline2\nlongtail"
	got := tailString(in, 10) // last 10 bytes = "2\nlongtail" -> after newline -> "longtail"
	if strings.Contains(got, "line1") {
		t.Errorf("tail should not contain head: %q", got)
	}
	if !strings.HasSuffix(in, got) {
		t.Errorf("tail %q is not a suffix of input", got)
	}
}

func TestFindMirrorEntityTab(t *testing.T) {
	snap := domain.WorkspaceSnapshot{Tabs: []domain.TabSnapshot{sampleTab()}}

	// tab
	if tab := findMirrorEntityTab(snap, "tab", "tab-1"); tab == nil || tab.ID != "tab-1" || len(tab.Lanes) != 2 {
		t.Errorf("tab lookup failed: %+v", tab)
	}
	// lane -> single-lane synthetic tab
	if tab := findMirrorEntityTab(snap, "lane", "lane-2"); tab == nil || len(tab.Lanes) != 1 || tab.Lanes[0].ID != "lane-2" {
		t.Errorf("lane lookup failed: %+v", tab)
	}
	// pane_group -> single-group synthetic tab
	tab := findMirrorEntityTab(snap, "pane_group", "grp-1")
	if tab == nil || len(tab.Lanes) != 1 || len(tab.Lanes[0].PaneGroups) != 1 || tab.Lanes[0].PaneGroups[0].ID != "grp-1" {
		t.Errorf("pane_group lookup failed: %+v", tab)
	}
	// missing
	if tab := findMirrorEntityTab(snap, "tab", "nope"); tab != nil {
		t.Errorf("expected nil for missing entity, got %+v", tab)
	}
}

func TestIsMirrorEntityType(t *testing.T) {
	for _, et := range []string{"tab", "lane", "pane_group"} {
		if !isMirrorEntityType(et) {
			t.Errorf("%q should be a mirror entity type", et)
		}
	}
	if isMirrorEntityType("pane") {
		t.Errorf("pane should not be a mirror entity type")
	}
}

func TestLastNStrings(t *testing.T) {
	if got := lastNStrings([]string{"a", "b"}, 5); len(got) != 2 {
		t.Errorf("under cap: got %v", got)
	}
	got := lastNStrings([]string{"a", "b", "c", "d"}, 2)
	if len(got) != 2 || got[0] != "c" || got[1] != "d" {
		t.Errorf("over cap: got %v, want [c d]", got)
	}
	if got := lastNStrings(nil, 3); got != nil {
		t.Errorf("nil input: got %v", got)
	}
}

func TestMirrorCarriesHistoryToDisplay(t *testing.T) {
	src := sampleTab() // p1 has ShellHistory ["a","b"]
	mt := &mirrorTab{shareID: "s-h", mode: "control"}
	mt.snap = trimTabForMirror(src)
	mt.hasData = true
	disp := mt.displayTab(false)
	p := disp.Lanes[0].PaneGroups[0].Panes[0]
	if len(p.ShellHistory) != 2 || p.ShellHistory[0] != "a" {
		t.Errorf("mirror display pane should carry shell history, got %v", p.ShellHistory)
	}
}

func TestMirrorTabIDRoundTrip(t *testing.T) {
	id := mirrorTabID("abc123")
	if !isMirrorTabID(id) {
		t.Errorf("%q should be recognized as a mirror tab id", id)
	}
	if isMirrorTabID("tab-1") {
		t.Errorf("normal tab id misclassified as mirror")
	}
}

func TestWorkspaceMirrorTabNavAndSnapshot(t *testing.T) {
	w := &WorkspaceActor{} // no real tabs, no NATS needed for mirror-only paths

	if w.tabCount() != 0 {
		t.Fatalf("empty workspace tabCount = %d", w.tabCount())
	}

	mt := w.addMirrorTab("share-1", "remote-work")
	mt.snap = sampleTab()
	mt.hasData = true

	if w.tabCount() != 1 {
		t.Fatalf("tabCount after add = %d, want 1", w.tabCount())
	}

	// Selecting index 0 (== len(tabs)) selects the mirror tab.
	w.activeTabIdx = 0
	if got := w.activeMirrorTab(); got != mt {
		t.Fatalf("activeMirrorTab() did not return the mirror tab")
	}

	snap := w.collectSnapshot(false, false)
	if len(snap.Tabs) != 1 {
		t.Fatalf("snapshot tabs = %d, want 1", len(snap.Tabs))
	}
	wantID := mirrorTabID("share-1")
	if snap.Tabs[0].ID != wantID {
		t.Errorf("mirror tab id = %q, want %q", snap.Tabs[0].ID, wantID)
	}
	if snap.ActiveTabID != wantID {
		t.Errorf("ActiveTabID = %q, want %q", snap.ActiveTabID, wantID)
	}
	if want := mirrorPaneID("share-1", "p1"); snap.ActivePaneID != want {
		t.Errorf("ActivePaneID = %q, want %q", snap.ActivePaneID, want)
	}
	// Rendered panes use stable mirror ids (subscriber pane ⇄ source pane).
	if got := snap.Tabs[0].Lanes[0].PaneGroups[0].Panes[0].ID; got != mirrorPaneID("share-1", "p1") {
		t.Errorf("mirror pane id = %q, want %q", got, mirrorPaneID("share-1", "p1"))
	}
	if !strings.Contains(snap.Tabs[0].Title, "remote-work") {
		t.Errorf("mirror tab title = %q, want it to contain alias", snap.Tabs[0].Title)
	}

	// Removing the mirror tab clamps the active index and empties the list.
	if !w.removeMirrorTab("share-1") {
		t.Fatalf("removeMirrorTab returned false")
	}
	if w.tabCount() != 0 {
		t.Errorf("tabCount after remove = %d, want 0", w.tabCount())
	}
	if w.activeTabIdx != 0 {
		t.Errorf("activeTabIdx after remove = %d, want 0", w.activeTabIdx)
	}
}

func TestWorkspaceMirrorPlaceholderWhenNoData(t *testing.T) {
	w := &WorkspaceActor{}
	w.addMirrorTab("share-2", "pending")
	w.activeTabIdx = 0

	snap := w.collectSnapshot(false, false)
	if len(snap.Tabs) != 1 {
		t.Fatalf("want 1 tab, got %d", len(snap.Tabs))
	}
	// Placeholder must have a renderable lane/group/pane.
	tab := snap.Tabs[0]
	if len(tab.Lanes) == 0 || len(tab.Lanes[0].PaneGroups) == 0 || len(tab.Lanes[0].PaneGroups[0].Panes) == 0 {
		t.Fatalf("placeholder tab not renderable: %+v", tab)
	}
	if !strings.Contains(tab.Lanes[0].PaneGroups[0].Panes[0].Output, "connecting") {
		t.Errorf("placeholder pane missing connecting message")
	}
}

func TestApplyMirrorTabUpdateUnknownShareIgnored(t *testing.T) {
	w := &WorkspaceActor{}
	// No panic / no effect for an unknown share.
	w.applyMirrorTabUpdate(&MsgMirrorTabUpdate{ShareID: "ghost", Tab: sampleTab()})
	if len(w.mirrorTabs) != 0 {
		t.Errorf("unknown update should not create a mirror tab")
	}
}

func TestLayoutDocAlias(t *testing.T) {
	cases := []struct {
		name        string
		entityType  string
		entityAlias string
		tabTitle    string
		want        string
	}{
		// A tab share follows the tab's current title so a post-share rename
		// (##tab name) reaches subscribers.
		{"tab tracks current title", "tab", "old-name", "abc", "abc"},
		{"tab empty title falls back to alias", "tab", "old-name", "", "old-name"},
		// Lane / pane_group keep their synthesized share-time alias.
		{"lane keeps alias", "lane", "work · lane", "work", "work · lane"},
		{"pane_group keeps alias", "pane_group", "group abcd", "work", "group abcd"},
	}
	for _, c := range cases {
		if got := layoutDocAlias(c.entityType, c.entityAlias, c.tabTitle); got != c.want {
			t.Errorf("%s: layoutDocAlias(%q,%q,%q) = %q, want %q",
				c.name, c.entityType, c.entityAlias, c.tabTitle, got, c.want)
		}
	}
}

// TestMirrorTabAdoptsRenamedAlias verifies the user-reported flow: a tab is
// shared first, then renamed (##tab name abc). The source now stamps the
// current title on each layout doc (layoutDocAlias), and the subscriber adopts
// it via applyMirrorTabUpdate, so the mirror tab's displayed name updates live.
func TestMirrorTabAdoptsRenamedAlias(t *testing.T) {
	w := &WorkspaceActor{}
	mt := w.addMirrorTab("share-1", "old-name")
	mt.mode = "view"

	// First layout doc carries the share-time name.
	w.applyMirrorTabUpdate(&MsgMirrorTabUpdate{ShareID: "share-1", Alias: "old-name", Tab: sampleTab()})
	if got := mt.displayName(); got != "old-name" {
		t.Fatalf("initial displayName = %q, want old-name", got)
	}

	// Source renamed the tab -> next doc carries the new title.
	w.applyMirrorTabUpdate(&MsgMirrorTabUpdate{ShareID: "share-1", Alias: "abc", Tab: sampleTab()})
	if got := mt.displayName(); got != "abc" {
		t.Errorf("after source rename, displayName = %q, want abc", got)
	}

	// A subscriber-local rename still wins over the source name and is never
	// clobbered by later layout docs.
	mt.localName = "my-local"
	w.applyMirrorTabUpdate(&MsgMirrorTabUpdate{ShareID: "share-1", Alias: "abc2", Tab: sampleTab()})
	if got := mt.displayName(); got != "my-local" {
		t.Errorf("local rename should win, displayName = %q, want my-local", got)
	}
	if mt.alias != "abc2" {
		t.Errorf("alias should still track source = %q, want abc2", mt.alias)
	}
}

// --- control mode (subscriber drives source panes) ---

func TestMirrorCommandFor(t *testing.T) {
	cases := []struct {
		text, mode  string
		wantType    string
		wantPayload string
	}{
		{"ls -la", "shell", "exec_shell", "ls -la"},
		{"explain this", "prompt", "exec_prompt", "explain this"},
		{"hi team", "chat", "exec_chat", "hi team"},
		{"new grid 2x2", "rysh", "exec_rysh", "new grid 2x2"},
		{"##new grid 2x2", "shell", "exec_rysh", "new grid 2x2"},        // ## overrides mode
		{"##>event:print:x", "shell", "exec_shell", "##>event:print:x"}, // pipeline, not rysh
	}
	for _, c := range cases {
		gotType, gotPayload := mirrorCommandFor(c.text, c.mode)
		if gotType != c.wantType || gotPayload != c.wantPayload {
			t.Errorf("mirrorCommandFor(%q,%q) = (%q,%q), want (%q,%q)",
				c.text, c.mode, gotType, gotPayload, c.wantType, c.wantPayload)
		}
	}
}

func TestMirrorTabFocusModel(t *testing.T) {
	mt := &mirrorTab{shareID: "s", mode: "control"}
	mt.snap = sampleTab() // panes p1, p2
	mt.hasData = true

	// Defaults to source's active pane (p1).
	if got := mt.effectiveFocusedPane(); got != "p1" {
		t.Fatalf("default focus = %q, want p1 (source active)", got)
	}

	ids := mt.orderedPaneIDs()
	if len(ids) != 2 || ids[0] != "p1" || ids[1] != "p2" {
		t.Fatalf("orderedPaneIDs = %v, want [p1 p2]", ids)
	}

	w := &WorkspaceActor{}
	// Cycle forward p1 -> p2 -> wrap p1.
	w.cycleMirrorFocus(mt, msg.DirNext)
	if mt.focusedPaneID != "p2" {
		t.Errorf("after next, focus = %q, want p2", mt.focusedPaneID)
	}
	w.cycleMirrorFocus(mt, msg.DirNext)
	if mt.focusedPaneID != "p1" {
		t.Errorf("after wrap, focus = %q, want p1", mt.focusedPaneID)
	}
	// Cycle back p1 -> p2 (wrap).
	w.cycleMirrorFocus(mt, msg.DirPrev)
	if mt.focusedPaneID != "p2" {
		t.Errorf("after prev wrap, focus = %q, want p2", mt.focusedPaneID)
	}

	// Spatial left/right move between lanes (columns), landing on the lane's
	// active pane. sampleTab has lane-1[p1] and lane-2[p2].
	mt.focusedPaneID = "p1"
	w.cycleMirrorFocus(mt, msg.DirRight)
	if mt.focusedPaneID != "p2" {
		t.Errorf("right from lane-1, focus = %q, want p2", mt.focusedPaneID)
	}
	w.cycleMirrorFocus(mt, msg.DirLeft)
	if mt.focusedPaneID != "p1" {
		t.Errorf("left from lane-2, focus = %q, want p1", mt.focusedPaneID)
	}

	// Edges do not wrap: left at the first lane / right at the last lane are
	// no-ops.
	mt.focusedPaneID = "p1"
	w.cycleMirrorFocus(mt, msg.DirLeft)
	if mt.focusedPaneID != "p1" {
		t.Errorf("left at first lane, focus = %q, want p1 (no-op)", mt.focusedPaneID)
	}
	mt.focusedPaneID = "p2"
	w.cycleMirrorFocus(mt, msg.DirRight)
	if mt.focusedPaneID != "p2" {
		t.Errorf("right at last lane, focus = %q, want p2 (no-op)", mt.focusedPaneID)
	}

	// up/down are a flat pane cycle (one stop per group), wrapping at the ends:
	// Down advances, Up goes back. sampleTab has groups [p1, p2].
	mt.focusedPaneID = "p1"
	w.cycleMirrorFocus(mt, msg.DirDown)
	if mt.focusedPaneID != "p2" {
		t.Errorf("down cycle from p1, focus = %q, want p2", mt.focusedPaneID)
	}
	w.cycleMirrorFocus(mt, msg.DirDown)
	if mt.focusedPaneID != "p1" {
		t.Errorf("down cycle wrap, focus = %q, want p1", mt.focusedPaneID)
	}
	w.cycleMirrorFocus(mt, msg.DirUp)
	if mt.focusedPaneID != "p2" {
		t.Errorf("up cycle wrap from p1, focus = %q, want p2", mt.focusedPaneID)
	}
}

// twoDTab builds a layout with one multi-group lane and one single-group lane:
//
//	lane-A: group g-a1[pa1], group g-a2[pa2]   (vertical split)
//	lane-B: group g-b1[pb1]
func twoDTab() domain.TabSnapshot {
	return domain.TabSnapshot{
		ID:           "tab-2d",
		ActivePaneID: "pa1",
		Lanes: []domain.LaneSnapshot{
			{
				ID:           "lane-A",
				Flex:         5,
				ActivePaneID: "pa1",
				PaneGroups: []domain.PaneGroupSnapshot{
					{ID: "g-a1", RowFlex: 5, ActivePaneID: "pa1", Panes: []domain.PaneSnapshot{{ID: "pa1", Mode: "shell"}}},
					{ID: "g-a2", RowFlex: 5, ActivePaneID: "pa2", Panes: []domain.PaneSnapshot{{ID: "pa2", Mode: "shell"}}},
				},
			},
			{
				ID:           "lane-B",
				Flex:         5,
				ActivePaneID: "pb1",
				PaneGroups: []domain.PaneGroupSnapshot{
					{ID: "g-b1", RowFlex: 10, ActivePaneID: "pb1", Panes: []domain.PaneSnapshot{{ID: "pb1", Mode: "shell"}}},
				},
			},
		},
	}
}

func TestMirrorTabFocus2D(t *testing.T) {
	mt := &mirrorTab{shareID: "s", mode: "control"}
	mt.snap = twoDTab()
	mt.hasData = true
	mt.focusedPaneID = "pa1"

	w := &WorkspaceActor{}

	// Down/up are a flat cycle through the groups in render order [pa1, pa2, pb1].
	w.cycleMirrorFocus(mt, msg.DirDown)
	if mt.focusedPaneID != "pa2" {
		t.Fatalf("down from pa1, focus = %q, want pa2", mt.focusedPaneID)
	}
	w.cycleMirrorFocus(mt, msg.DirDown)
	if mt.focusedPaneID != "pb1" {
		t.Fatalf("down from pa2, focus = %q, want pb1 (crosses lane)", mt.focusedPaneID)
	}
	w.cycleMirrorFocus(mt, msg.DirDown)
	if mt.focusedPaneID != "pa1" {
		t.Fatalf("down from pb1, focus = %q, want pa1 (wrap)", mt.focusedPaneID)
	}
	// Up wraps backwards: pa1 -> pb1 (no spatial edge no-op for the cycle axis).
	w.cycleMirrorFocus(mt, msg.DirUp)
	if mt.focusedPaneID != "pb1" {
		t.Errorf("up from pa1, focus = %q, want pb1 (wrap)", mt.focusedPaneID)
	}

	// Right/Left move spatially between lanes, landing on the lane's active pane.
	mt.focusedPaneID = "pa1"
	w.cycleMirrorFocus(mt, msg.DirRight)
	if mt.focusedPaneID != "pb1" {
		t.Fatalf("right to lane-B, focus = %q, want pb1", mt.focusedPaneID)
	}
	// Right at the last lane is a no-op (no wrap).
	w.cycleMirrorFocus(mt, msg.DirRight)
	if mt.focusedPaneID != "pb1" {
		t.Errorf("right at last lane, focus = %q, want pb1 (no-op)", mt.focusedPaneID)
	}

	// Left from lane-B returns to lane-A's active pane (pa1).
	w.cycleMirrorFocus(mt, msg.DirLeft)
	if mt.focusedPaneID != "pa1" {
		t.Fatalf("left to lane-A, focus = %q, want pa1 (lane active)", mt.focusedPaneID)
	}

	// Right from the lower group of lane-A still crosses to lane-B (lane move
	// ignores the current group row).
	mt.focusedPaneID = "pa2"
	w.cycleMirrorFocus(mt, msg.DirRight)
	if mt.focusedPaneID != "pb1" {
		t.Errorf("right from lane-A lower group, focus = %q, want pb1", mt.focusedPaneID)
	}

	// Flat next/prev traverses one pane per group in render order, with wrap
	// (here every group holds a single pane).
	mt.focusedPaneID = "pa1"
	for _, want := range []string{"pa2", "pb1", "pa1"} {
		w.cycleMirrorFocus(mt, msg.DirNext)
		if mt.focusedPaneID != want {
			t.Fatalf("next cycle, focus = %q, want %q", mt.focusedPaneID, want)
		}
	}
}

// stackedLaneTab builds a layout with three columns, the middle one a stack of
// three panes (the visible/active one is s1):
//
//	lane-1: group g1[p1]
//	lane-2: group g2[s1, s2, s3]   (a stack; s1 visible)
//	lane-3: group g3[p3]
func stackedLaneTab() domain.TabSnapshot {
	return domain.TabSnapshot{
		ID:           "tab-stk",
		ActivePaneID: "p1",
		Lanes: []domain.LaneSnapshot{
			{ID: "lane-1", Flex: 10, ActivePaneID: "p1", PaneGroups: []domain.PaneGroupSnapshot{
				{ID: "g1", RowFlex: 10, ActivePaneID: "p1", Panes: []domain.PaneSnapshot{{ID: "p1"}}}}},
			{ID: "lane-2", Flex: 10, ActivePaneID: "s1", PaneGroups: []domain.PaneGroupSnapshot{
				{ID: "g2", RowFlex: 10, ActivePaneID: "s1", Panes: []domain.PaneSnapshot{{ID: "s1"}, {ID: "s2"}, {ID: "s3"}}}}},
			{ID: "lane-3", Flex: 10, ActivePaneID: "p3", PaneGroups: []domain.PaneGroupSnapshot{
				{ID: "g3", RowFlex: 10, ActivePaneID: "p3", Panes: []domain.PaneSnapshot{{ID: "p3"}}}}},
		},
	}
}

// TestMirrorTabFocusStackTreatedAsOne verifies the user-reported behaviour:
// Ctrl+Space ↑/↓ on a subscribed tab cycles between panes treating a whole
// stack as a single pane (landing on its visible pane), and never steps onto a
// background stacked pane. Rotating inside the stack is Ctrl+S, not Ctrl+Space.
func TestMirrorTabFocusStackTreatedAsOne(t *testing.T) {
	mt := &mirrorTab{shareID: "s", mode: "control"}
	mt.snap = stackedLaneTab()
	mt.hasData = true
	w := &WorkspaceActor{}

	background := map[string]bool{"s2": true, "s3": true}

	// orderedGroupPaneIDs collapses the stack to its visible pane.
	reps := mt.orderedGroupPaneIDs()
	if len(reps) != 3 || reps[0] != "p1" || reps[1] != "s1" || reps[2] != "p3" {
		t.Fatalf("orderedGroupPaneIDs = %v, want [p1 s1 p3]", reps)
	}

	// Down cycles p1 -> s1 -> p3 -> p1, skipping s2/s3.
	mt.focusedPaneID = "p1"
	for _, want := range []string{"s1", "p3", "p1"} {
		w.cycleMirrorFocus(mt, msg.DirDown)
		if mt.focusedPaneID != want {
			t.Fatalf("down cycle, focus = %q, want %q", mt.focusedPaneID, want)
		}
		if background[mt.focusedPaneID] {
			t.Fatalf("down cycle landed on background stacked pane %q", mt.focusedPaneID)
		}
	}

	// Up cycles the other way: p1 -> p3 -> s1 -> p1.
	mt.focusedPaneID = "p1"
	for _, want := range []string{"p3", "s1", "p1"} {
		w.cycleMirrorFocus(mt, msg.DirUp)
		if mt.focusedPaneID != want {
			t.Fatalf("up cycle, focus = %q, want %q", mt.focusedPaneID, want)
		}
		if background[mt.focusedPaneID] {
			t.Fatalf("up cycle landed on background stacked pane %q", mt.focusedPaneID)
		}
	}

	// Next/Prev (web "next/prev pane") behave the same: stack counts once.
	mt.focusedPaneID = "p1"
	w.cycleMirrorFocus(mt, msg.DirNext)
	if mt.focusedPaneID != "s1" {
		t.Errorf("next from p1, focus = %q, want s1", mt.focusedPaneID)
	}
	w.cycleMirrorFocus(mt, msg.DirNext)
	if mt.focusedPaneID != "p3" {
		t.Errorf("next from stack, focus = %q, want p3 (skips s2/s3)", mt.focusedPaneID)
	}

	// Left/Right land on the stack's visible pane, never a background pane.
	mt.focusedPaneID = "p1"
	w.cycleMirrorFocus(mt, msg.DirRight)
	if mt.focusedPaneID != "s1" {
		t.Errorf("right into stack lane, focus = %q, want s1 (visible)", mt.focusedPaneID)
	}
	w.cycleMirrorFocus(mt, msg.DirRight)
	if mt.focusedPaneID != "p3" {
		t.Errorf("right past stack lane, focus = %q, want p3", mt.focusedPaneID)
	}

	// Defensive: if focus is somehow on a background stacked pane, the cycle
	// still advances by a whole group (maps to the group's visible pane first).
	mt.focusedPaneID = "s3"
	w.cycleMirrorFocus(mt, msg.DirDown)
	if mt.focusedPaneID != "p3" {
		t.Errorf("down from background stacked pane, focus = %q, want p3", mt.focusedPaneID)
	}
}

func TestEffectiveFocusedPaneFallback(t *testing.T) {
	// Focus pinned to a pane that no longer exists -> falls back to source active.
	mt := &mirrorTab{shareID: "s", focusedPaneID: "gone"}
	mt.snap = sampleTab()
	mt.hasData = true
	if got := mt.effectiveFocusedPane(); got != "p1" {
		t.Errorf("stale focus fallback = %q, want p1", got)
	}

	// No layout yet -> pending sentinel.
	empty := &mirrorTab{shareID: "s2"}
	if got := empty.effectiveFocusedPane(); got != "mirror-pending:s2" {
		t.Errorf("empty focus = %q, want pending sentinel", got)
	}
}

func TestMirrorListenerModeCoercion(t *testing.T) {
	// The mirror listener constructor coerces an invalid mode to "view".
	l := NewMirrorTabListenerActor("s", "alias", "bogus", "me", config.UpstreamConfig{}, nil, nil)
	if l.mode != "view" {
		t.Errorf("listener mode = %q, want view (coerced)", l.mode)
	}
	l2 := NewMirrorTabListenerActor("s", "alias", "control", "me", config.UpstreamConfig{}, nil, nil)
	if l2.mode != "control" {
		t.Errorf("listener mode = %q, want control", l2.mode)
	}
}

// --- interactive (alternate-screen) mirroring ---

func interactiveTab() domain.TabSnapshot {
	return domain.TabSnapshot{
		ID:           "tab-i",
		Title:        "work",
		ActivePaneID: "pi",
		Lanes: []domain.LaneSnapshot{{
			ID:           "lane-i",
			Flex:         10,
			ActivePaneID: "pi",
			PaneGroups: []domain.PaneGroupSnapshot{{
				ID:           "grp-i",
				RowFlex:      10,
				ActivePaneID: "pi",
				Panes: []domain.PaneSnapshot{{
					ID:          "pi",
					Title:       "claude",
					Mode:        "shell",
					RawMode:     true,
					VTScreen:    []string{"line A", "line B"},
					VTCursorRow: 1,
					VTCursorCol: 3,
				}},
			}},
		}},
	}
}

func TestTrimTabCarriesInteractiveVT(t *testing.T) {
	// The layout doc carries the VT screen + cursor for interactive panes as a
	// fallback/seed, so the subscriber can mirror an alternate-screen program even
	// when the fast per-pane raw VT stream is not flowing.
	out := trimTabForMirror(interactiveTab())
	p := out.Lanes[0].PaneGroups[0].Panes[0]
	if !p.RawMode {
		t.Errorf("interactive pane should keep RawMode marker")
	}
	if len(p.VTScreen) != 2 || p.VTScreen[0] != "line A" {
		t.Errorf("VTScreen should be carried as fallback, got %v", p.VTScreen)
	}
	if p.VTCursorRow != 1 || p.VTCursorCol != 3 {
		t.Errorf("cursor should be carried as fallback, got %d,%d", p.VTCursorRow, p.VTCursorCol)
	}
}

func TestDisplayTabMapsInteractiveToRemote(t *testing.T) {
	mt := &mirrorTab{shareID: "share-i", mode: "control"}
	mt.snap = trimTabForMirror(interactiveTab())
	mt.hasData = true
	// The pane is live-interactive: its screen streams to the TUI via the vtframe
	// plane, so the snapshot only flags RemoteInteractive (no embedded screen).
	mt.liveInteractive = map[string]bool{"pi": true}

	disp := mt.displayTab(false)
	p := disp.Lanes[0].PaneGroups[0].Panes[0]
	if p.RawMode {
		t.Errorf("display pane should NOT use local RawMode path")
	}
	if !p.RemoteInteractive {
		t.Errorf("interactive pane not mapped to RemoteInteractive: %+v", p)
	}
	if len(p.RemoteVTScreen) != 0 {
		t.Errorf("live pane screen must come from vtframe, not the snapshot: %v", p.RemoteVTScreen)
	}
	if p.ControllingShareID != "share-i" {
		t.Errorf("control mode should set ControllingShareID, got %q", p.ControllingShareID)
	}
	// The stored snapshot must NOT be mutated by displayTab.
	if !mt.snap.Lanes[0].PaneGroups[0].Panes[0].RawMode {
		t.Errorf("displayTab mutated the stored snapshot")
	}

	// View mode: rendered but no keystroke forwarding.
	mt.mode = "view"
	pv := mt.displayTab(false).Lanes[0].PaneGroups[0].Panes[0]
	if !pv.RemoteInteractive {
		t.Errorf("view mode should still render interactive screen")
	}
	if pv.ControllingShareID != "" {
		t.Errorf("view mode must not set ControllingShareID")
	}
}

func TestDisplayTabInteractiveFallsBackToDocVT(t *testing.T) {
	// An interactive pane whose live VT stream has not seeded remoteVT yet renders
	// via the layout doc's VT screen fallback (RemoteInteractive from p.VTScreen),
	// never via the local raw-pane path (there is no local PTY behind a mirror).
	mt := &mirrorTab{shareID: "share-i", mode: "control"}
	mt.snap = trimTabForMirror(interactiveTab())
	mt.hasData = true

	p := mt.displayTab(false).Lanes[0].PaneGroups[0].Panes[0]
	if p.RawMode {
		t.Errorf("mirror pane must never use the local RawMode render path")
	}
	if !p.RemoteInteractive {
		t.Errorf("interactive mirror pane should fall back to doc VT screen (RemoteInteractive)")
	}
	if len(p.RemoteVTScreen) != 2 || p.RemoteVTScreen[0] != "line A" {
		t.Errorf("fallback should use the doc VT screen, got %v", p.RemoteVTScreen)
	}
	if len(p.VTScreen) != 0 {
		t.Errorf("mirror pane must not expose local VTScreen, got %v", p.VTScreen)
	}
}

func TestDisplayTabLiveInteractiveOmitsDocScreen(t *testing.T) {
	// A live-interactive pane renders from the vtframe stream, so displayTab must
	// NOT embed the layout doc's VT screen even when the doc still carries one.
	mt := &mirrorTab{shareID: "share-i", mode: "view"}
	mt.snap = trimTabForMirror(interactiveTab()) // doc VTScreen = ["line A","line B"]
	mt.hasData = true
	mt.liveInteractive = map[string]bool{"pi": true}

	p := mt.displayTab(false).Lanes[0].PaneGroups[0].Panes[0]
	if !p.RemoteInteractive {
		t.Errorf("live pane should be RemoteInteractive")
	}
	if len(p.RemoteVTScreen) != 0 {
		t.Errorf("live pane must omit the doc VT screen (it streams via vtframe), got %v", p.RemoteVTScreen)
	}
}

func TestTabHasInteractive(t *testing.T) {
	if !tabHasInteractive(interactiveTab()) {
		t.Errorf("interactiveTab should be detected as interactive")
	}
	if tabHasInteractive(sampleTab()) {
		t.Errorf("sampleTab has no interactive pane")
	}
}

func TestMirrorTabLocalRename(t *testing.T) {
	w := &WorkspaceActor{}
	mt := w.addMirrorTab("share-r", "remote-name")
	mt.snap = sampleTab()
	mt.hasData = true
	w.activeTabIdx = 0 // select the mirror tab

	if mt.displayName() != "remote-name" {
		t.Fatalf("default displayName = %q, want remote-name", mt.displayName())
	}

	// Rename locally — applies to the mirror tab, not a real tab.
	if !w.renameActiveTab("my-local") {
		t.Fatalf("renameActiveTab returned false for active mirror tab")
	}
	if mt.localName != "my-local" {
		t.Errorf("localName = %q, want my-local", mt.localName)
	}
	if mt.displayName() != "my-local" {
		t.Errorf("displayName = %q, want my-local", mt.displayName())
	}

	// A later layout update changes the source alias but must NOT clobber the
	// subscriber's local rename.
	w.applyMirrorTabUpdate(&MsgMirrorTabUpdate{
		ShareID: "share-r",
		Alias:   "remote-name-2",
		Tab:     sampleTab(),
	})
	if mt.alias != "remote-name-2" {
		t.Errorf("alias = %q, want remote-name-2 (source tracked)", mt.alias)
	}
	if mt.localName != "my-local" {
		t.Errorf("local rename clobbered by layout update: %q", mt.localName)
	}
	if mt.displayName() != "my-local" {
		t.Errorf("displayName after update = %q, want my-local", mt.displayName())
	}

	// The rendered tab title reflects the local name only.
	snap := w.collectSnapshot(false, false)
	if !strings.Contains(snap.Tabs[0].Title, "my-local") || strings.Contains(snap.Tabs[0].Title, "remote-name") {
		t.Errorf("rendered title = %q, want the local name only", snap.Tabs[0].Title)
	}
}

// TestRenameActiveMirrorPaneLocalOverride verifies that renaming a pane on a
// mirror tab records a subscriber-local override that is visible in the rendered
// snapshot (as the pane's given-name), survives layout updates, and can be
// cleared — all in view mode, with no upstream/actor-system dependency.
func TestRenameActiveMirrorPaneLocalOverride(t *testing.T) {
	// No active mirror tab → not handled here (falls through to local handling).
	w := &WorkspaceActor{}
	if w.renameActiveMirrorPane("x") {
		t.Errorf("no active mirror tab: renameActiveMirrorPane = true, want false")
	}

	// View-mode mirror tab: rename the focused pane (defaults to source active p1).
	w = &WorkspaceActor{}
	mt := w.addMirrorTab("share-v", "remote")
	mt.mode = "view"
	mt.snap = sampleTab()
	mt.hasData = true
	w.activeTabIdx = 0

	if !w.renameActiveMirrorPane("my-pane") {
		t.Fatalf("view-mode mirror: renameActiveMirrorPane = false, want true")
	}
	if got := mt.paneName("p1"); got != "my-pane" {
		t.Errorf("paneName(p1) = %q, want my-pane", got)
	}
	if gn := givenNameOf(mt.displayTab(false), mirrorPaneID("share-v", "p1")); gn != "my-pane" {
		t.Errorf("rendered given-name = %q, want my-pane", gn)
	}

	// A later layout update must not clobber the local override (it lives on the
	// mirror tab, not in the snapshot).
	mt.snap = sampleTab()
	if gn := givenNameOf(mt.displayTab(false), mirrorPaneID("share-v", "p1")); gn != "my-pane" {
		t.Errorf("override clobbered by layout update: rendered given-name = %q", gn)
	}

	// Clearing the name removes the override → the source given-name shows again.
	if !w.renameActiveMirrorPane("") {
		t.Fatalf("clear rename: renameActiveMirrorPane = false, want true")
	}
	if got := mt.paneName("p1"); got != "" {
		t.Errorf("paneName(p1) after clear = %q, want empty", got)
	}
	if gn := givenNameOf(mt.displayTab(false), mirrorPaneID("share-v", "p1")); gn != "edit" {
		t.Errorf("rendered given-name after clear = %q, want source \"edit\"", gn)
	}
}

// givenNameOf returns the GivenName of the pane with id paneID in tab, or "".
func givenNameOf(tab domain.TabSnapshot, paneID string) string {
	for _, lane := range tab.Lanes {
		for _, g := range lane.PaneGroups {
			for _, p := range g.Panes {
				if p.ID == paneID {
					return p.GivenName
				}
			}
		}
	}
	return ""
}

// TestTabOpPayloadRenameRoundTrip locks the wire format of a rename_pane tab_op:
// the new given-name travels in Name, and empty Dir/Delta stay omitted.
func TestTabOpPayloadRenameRoundTrip(t *testing.T) {
	data, err := json.Marshal(tabOpPayload{Op: "rename_pane", Name: "builder"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out tabOpPayload
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Op != "rename_pane" || out.Name != "builder" {
		t.Errorf("round-trip = %+v, want op=rename_pane name=builder", out)
	}
	if strings.Contains(string(data), "dir") || strings.Contains(string(data), "delta") {
		t.Errorf("payload %s should omit empty dir/delta", data)
	}
}

func TestMirrorPaneIDRoundTrip(t *testing.T) {
	share := "abc-123"
	src := "pane-uuid-9"
	mid := mirrorPaneID(share, src)
	if mid != "mirror:abc-123:pane-uuid-9" {
		t.Fatalf("mirrorPaneID = %q", mid)
	}
	if got := mirrorPaneSourceID(mid); got != src {
		t.Errorf("mirrorPaneSourceID(%q) = %q, want %q", mid, got, src)
	}
	if mirrorPaneID(share, "") != "" {
		t.Errorf("empty source should yield empty mirror id")
	}
	if mirrorPaneSourceID("not-a-mirror-id") != "" {
		t.Errorf("non-mirror id should yield empty source")
	}
}

func TestOutputDelta(t *testing.T) {
	cases := []struct{ old, new, want string }{
		{"", "hello", "hello"},                          // first output
		{"abc", "abcdef", "def"},                        // pure append
		{"abc", "abc", ""},                              // unchanged
		{"line1\nline2\n", "line2\nline3\n", "line3\n"}, // rolled tail (overlap)
		{"xyz", "abc", "abc"},                           // no overlap -> whole
	}
	for _, c := range cases {
		if got := outputDelta(c.old, c.new); got != c.want {
			t.Errorf("outputDelta(%q,%q) = %q, want %q", c.old, c.new, got, c.want)
		}
	}
}

func TestExtractShareFlag(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		flag     string
		wantVal  string
		wantRest []string
	}{
		{"absent", []string{"tab", "control"}, "--tab", "", []string{"tab", "control"}},
		{"flag then mode", []string{"tab", "--tab", "2", "control"}, "--tab", "2", []string{"tab", "control"}},
		{"mode then flag", []string{"tab", "control", "--tab", "web"}, "--tab", "web", []string{"tab", "control"}},
		{"flag at end no value", []string{"tab", "--tab"}, "--tab", "", []string{"tab"}},
		{"pane flag", []string{"pane", "--pane", "editor", "view"}, "--pane", "editor", []string{"pane", "view"}},
	}
	for _, c := range cases {
		val, rest := extractShareFlag(c.args, c.flag)
		if val != c.wantVal {
			t.Errorf("%s: value = %q, want %q", c.name, val, c.wantVal)
		}
		if len(rest) != len(c.wantRest) {
			t.Errorf("%s: rest = %v, want %v", c.name, rest, c.wantRest)
			continue
		}
		for i := range rest {
			if rest[i] != c.wantRest[i] {
				t.Errorf("%s: rest = %v, want %v", c.name, rest, c.wantRest)
				break
			}
		}
	}
}

// TestMirrorScrollbackAccumulateAndFetch verifies the per-pane VTerm scrollback
// mechanism: ongoing appends accumulate, a one-time seed prepends the pre-join
// backlog (and is guarded against re-broadcast), reset clears (and re-arms the
// seed), and the WorkspaceActor serves history + current screen for copy mode.
func TestMirrorScrollbackAccumulateAndFetch(t *testing.T) {
	w := &WorkspaceActor{}
	mt := w.addMirrorTab("share-x", "remote")
	if mt == nil {
		t.Fatal("addMirrorTab returned nil")
	}
	// Set the layout so mirrorScrollbackRows can find p1's current screen.
	w.applyMirrorTabUpdate(&MsgMirrorTabUpdate{ShareID: "share-x", Tab: sampleTab()}) // p1 VTScreen ["raw"]

	scrollback := func(srcPaneID string, rows []string, reset, seed bool) {
		w.applyMirrorPaneScrollback(&MsgMirrorPaneScrollback{
			ShareID: "share-x", SourcePaneID: srcPaneID, Rows: rows, Reset: reset, Seed: seed,
		})
	}

	// Ongoing appends (lines evicted from the live VTerm) accumulate.
	scrollback("p1", []string{"live1"}, false, false)
	scrollback("p1", []string{"live2"}, false, false)
	if got := strings.Join(mt.scrollbackFor("p1"), ","); got != "live1,live2" {
		t.Fatalf("after appends, scrollback = %q, want live1,live2", got)
	}

	// A seed prepends the pre-join backlog before the already-appended live rows.
	scrollback("p1", []string{"old1", "old2"}, false, true)
	if got := strings.Join(mt.scrollbackFor("p1"), ","); got != "old1,old2,live1,live2" {
		t.Fatalf("after seed, scrollback = %q, want old1,old2,live1,live2", got)
	}

	// A re-broadcast seed (another subscriber joins) is ignored once seeded.
	scrollback("p1", []string{"X"}, false, true)
	if got := strings.Join(mt.scrollbackFor("p1"), ","); got != "old1,old2,live1,live2" {
		t.Fatalf("re-seed should be ignored, scrollback = %q", got)
	}

	// More ongoing appends keep accumulating after the seed.
	scrollback("p1", []string{"live3"}, false, false)

	// The WorkspaceActor serves scrollback + current screen for the mirror pane.
	mirrorID := mirrorPaneID("share-x", "p1")
	rows := w.mirrorScrollbackRows(mirrorID)
	if got := strings.Join(rows, ","); got != "old1,old2,live1,live2,live3,raw" {
		t.Fatalf("mirrorScrollbackRows = %q, want old1,old2,live1,live2,live3,raw (history + screen)", got)
	}

	// Reset clears the history and re-arms the seed (new interactive session).
	scrollback("p1", nil, true, false)
	if got := mt.scrollbackFor("p1"); len(got) != 0 {
		t.Fatalf("reset should clear scrollback, got %v", got)
	}
	scrollback("p1", []string{"new1"}, false, true)
	if got := strings.Join(mt.scrollbackFor("p1"), ","); got != "new1" {
		t.Fatalf("seed after reset should apply, got %q", got)
	}

	// Non-mirror ids and unknown shares return nil.
	if r := w.mirrorScrollbackRows("p1"); r != nil {
		t.Errorf("non-mirror id should return nil, got %v", r)
	}
	if r := w.mirrorScrollbackRows(mirrorPaneID("ghost", "p1")); r != nil {
		t.Errorf("unknown share should return nil, got %v", r)
	}
}

// TestMirrorLayoutDocScrollbackJSONRoundTrip verifies the ScrollbackDeltas field
// survives JSON marshal/unmarshal, since the layout doc crosses the upstream
// link as plain JSON.
func TestMirrorLayoutDocScrollbackJSONRoundTrip(t *testing.T) {
	doc := mirrorLayoutDoc{
		Type:             "layout",
		ShareID:          "s1",
		EntityType:       "tab",
		ScrollbackDeltas: map[string][]string{"p1": {"line A", "line B"}},
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got mirrorLayoutDoc
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	rows := got.ScrollbackDeltas["p1"]
	if len(rows) != 2 || rows[0] != "line A" || rows[1] != "line B" {
		t.Fatalf("ScrollbackDeltas round-trip = %v, want [line A, line B]", rows)
	}
	// Empty deltas are omitted from the wire form.
	empty, _ := json.Marshal(mirrorLayoutDoc{Type: "layout"})
	if strings.Contains(string(empty), "scrollback_deltas") {
		t.Errorf("empty ScrollbackDeltas should be omitted, got %s", empty)
	}
}
