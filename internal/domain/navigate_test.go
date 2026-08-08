package domain

import (
	"reflect"
	"testing"
)

// navTree builds a two-tab workspace with enough structure to exercise every
// resolution rule:
//
//	tab-1 "alpha"  (active tab, active pane p3)
//	  lane-a "left"   (active p3)
//	    grp-a1 (active p1)  p1 "one"   p2 "two"(shared, given name "deuce")
//	    grp-a2 (active p3)  p3 "three"
//	  lane-b "right"  (active "")
//	    grp-b1 (active "")  p4 "four"
//	tab-2 "beta"
//	  lane-c ""       (active "")
//	    grp-c1 (active p5)  p5 "five"
func navTree() *WorkspaceSnapshot {
	return &WorkspaceSnapshot{
		ActiveTabID:  "tab-1",
		ActivePaneID: "p3",
		Tabs: []TabSnapshot{
			{
				ID: "tab-1", Title: "alpha", ActivePaneID: "p3",
				Lanes: []LaneSnapshot{
					{
						ID: "lane-a", Name: "left", Flex: 3, ActivePaneID: "p3",
						PaneGroups: []PaneGroupSnapshot{
							{ID: "grp-a1", ActivePaneID: "p1", Panes: []PaneSnapshot{
								{ID: "p1", Title: "one"},
								{ID: "p2", Title: "two", GivenName: "deuce", Sharing: true},
							}},
							{ID: "grp-a2", ActivePaneID: "p3", Panes: []PaneSnapshot{
								{ID: "p3", Title: "three"},
							}},
						},
					},
					{
						ID: "lane-b", Name: "right", Flex: 1,
						PaneGroups: []PaneGroupSnapshot{
							{ID: "grp-b1", Panes: []PaneSnapshot{{ID: "p4", Title: "four"}}},
						},
					},
				},
			},
			{
				ID: "tab-2", Title: "beta",
				Lanes: []LaneSnapshot{
					{
						ID: "lane-c",
						PaneGroups: []PaneGroupSnapshot{
							{ID: "grp-c1", ActivePaneID: "p5", Panes: []PaneSnapshot{{ID: "p5", Title: "five"}}},
						},
					},
				},
			},
		},
	}
}

func paneIDs(t *testing.T, got []string, want ...string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	if got == nil {
		got = []string{}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ids = %v, want %v", got, want)
	}
}

// ---------------------------------------------------------------------------
// Iteration
// ---------------------------------------------------------------------------

func TestPanesInTabOrderAndPointers(t *testing.T) {
	snap := navTree()
	tab := &snap.Tabs[0]

	var got []string
	for p := range PanesInTab(tab) {
		got = append(got, p.ID)
	}
	paneIDs(t, got, "p1", "p2", "p3", "p4")

	// The yielded pointers must address the real tree, not a copy — that is the
	// whole reason these take a *TabSnapshot rather than a value. A value
	// parameter would silently hand back pointers into a temporary.
	for p := range PanesInTab(tab) {
		if p.ID == "p3" {
			p.Title = "rewritten"
		}
	}
	if got := tab.Lanes[0].PaneGroups[1].Panes[0].Title; got != "rewritten" {
		t.Errorf("write through yielded pointer did not stick: Title = %q", got)
	}
}

func TestPanesIteratorsStopEarly(t *testing.T) {
	snap := navTree()

	// break must stop the walk, not just the innermost loop.
	n := 0
	for range PanesInTab(&snap.Tabs[0]) {
		n++
		break
	}
	if n != 1 {
		t.Errorf("PanesInTab visited %d panes after break, want 1", n)
	}

	n = 0
	for range PanesInWorkspace(snap) {
		n++
		if n == 2 {
			break
		}
	}
	if n != 2 {
		t.Errorf("PanesInWorkspace visited %d panes after break, want 2", n)
	}

	n = 0
	for range PanesInLane(&snap.Tabs[0].Lanes[0]) {
		n++
		break
	}
	if n != 1 {
		t.Errorf("PanesInLane visited %d panes after break, want 1", n)
	}

	n = 0
	for range PaneSitesInTab(&snap.Tabs[0]) {
		n++
		break
	}
	if n != 1 {
		t.Errorf("PaneSitesInTab visited %d sites after break, want 1", n)
	}

	n = 0
	for range GroupsInTab(&snap.Tabs[0]) {
		n++
		break
	}
	if n != 1 {
		t.Errorf("GroupsInTab visited %d groups after break, want 1", n)
	}
}

func TestIteratorsTolerateNil(t *testing.T) {
	for range PanesInGroup(nil) {
		t.Error("PanesInGroup(nil) yielded")
	}
	for range PanesInLane(nil) {
		t.Error("PanesInLane(nil) yielded")
	}
	for range PanesInTab(nil) {
		t.Error("PanesInTab(nil) yielded")
	}
	for range PanesInWorkspace(nil) {
		t.Error("PanesInWorkspace(nil) yielded")
	}
	for range PaneSitesInTab(nil) {
		t.Error("PaneSitesInTab(nil) yielded")
	}
	for range GroupsInTab(nil) {
		t.Error("GroupsInTab(nil) yielded")
	}
}

func TestPaneSitesInTab(t *testing.T) {
	snap := navTree()
	tab := &snap.Tabs[0]

	type row struct {
		pane        string
		lane, group string
		li, gi, pi  int
	}
	var got []row
	for s := range PaneSitesInTab(tab) {
		got = append(got, row{s.Pane.ID, s.Lane.ID, s.Group.ID, s.LaneIndex, s.GroupIndex, s.PaneIndex})
	}
	want := []row{
		{"p1", "lane-a", "grp-a1", 0, 0, 0},
		{"p2", "lane-a", "grp-a1", 0, 0, 1},
		{"p3", "lane-a", "grp-a2", 0, 1, 0},
		{"p4", "lane-b", "grp-b1", 1, 0, 0},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sites =\n  %v\nwant\n  %v", got, want)
	}
}

func TestGroupsInTab(t *testing.T) {
	snap := navTree()
	var got []string
	for lane, g := range GroupsInTab(&snap.Tabs[0]) {
		got = append(got, lane.ID+"/"+g.ID)
	}
	paneIDs(t, got, "lane-a/grp-a1", "lane-a/grp-a2", "lane-b/grp-b1")
}

// ---------------------------------------------------------------------------
// Containment
// ---------------------------------------------------------------------------

func TestContainsPane(t *testing.T) {
	snap := navTree()
	tab := &snap.Tabs[0]
	lane := &tab.Lanes[0]
	group := &lane.PaneGroups[0]

	cases := []struct {
		name string
		got  bool
		want bool
	}{
		{"group has p1", GroupContainsPane(group, "p1"), true},
		{"group lacks p3", GroupContainsPane(group, "p3"), false},
		{"group lacks missing", GroupContainsPane(group, "nope"), false},
		{"lane has p3", LaneContainsPane(lane, "p3"), true},
		{"lane lacks p4", LaneContainsPane(lane, "p4"), false},
		{"tab has p4", TabContainsPane(tab, "p4"), true},
		{"tab lacks p5", TabContainsPane(tab, "p5"), false},
		{"nil group", GroupContainsPane(nil, "p1"), false},
		{"nil lane", LaneContainsPane(nil, "p1"), false},
		{"nil tab", TabContainsPane(nil, "p1"), false},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, c.got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Lookup
// ---------------------------------------------------------------------------

func TestFindPane(t *testing.T) {
	snap := navTree()
	tab := &snap.Tabs[0]

	p := FindPaneInTab(tab, "p2")
	if p == nil || p.ID != "p2" {
		t.Fatalf("FindPaneInTab(p2) = %v", p)
	}
	if p != &tab.Lanes[0].PaneGroups[0].Panes[1] {
		t.Error("FindPaneInTab returned a pointer that is not into the tab")
	}
	if got := FindPaneInTab(tab, "p5"); got != nil {
		t.Errorf("FindPaneInTab(p5) = %v, want nil (p5 is in tab-2)", got)
	}
	if got := FindPaneInTab(nil, "p1"); got != nil {
		t.Errorf("FindPaneInTab(nil) = %v, want nil", got)
	}

	if got := FindPaneInWorkspace(snap, "p5"); got == nil || got.ID != "p5" {
		t.Errorf("FindPaneInWorkspace(p5) = %v, want p5", got)
	}
	if got := FindPaneInWorkspace(snap, "nope"); got != nil {
		t.Errorf("FindPaneInWorkspace(nope) = %v, want nil", got)
	}
	if got := FindPaneInWorkspace(nil, "p1"); got != nil {
		t.Errorf("FindPaneInWorkspace(nil) = %v, want nil", got)
	}
}

func TestLocatePane(t *testing.T) {
	snap := navTree()
	tab := &snap.Tabs[0]

	site, ok := LocatePaneInTab(tab, "p3")
	if !ok {
		t.Fatal("LocatePaneInTab(p3) not found")
	}
	if site.Lane.ID != "lane-a" || site.Group.ID != "grp-a2" || site.Pane.ID != "p3" {
		t.Errorf("site = %s/%s/%s, want lane-a/grp-a2/p3", site.Lane.ID, site.Group.ID, site.Pane.ID)
	}
	if site.LaneIndex != 0 || site.GroupIndex != 1 || site.PaneIndex != 0 {
		t.Errorf("indices = %d/%d/%d, want 0/1/0", site.LaneIndex, site.GroupIndex, site.PaneIndex)
	}

	if _, ok := LocatePaneInTab(tab, "p5"); ok {
		t.Error("LocatePaneInTab(p5) reported found")
	}
	if _, ok := LocatePaneInTab(nil, "p1"); ok {
		t.Error("LocatePaneInTab(nil) reported found")
	}

	wsTab, wsSite, ok := LocatePaneInWorkspace(snap, "p5")
	if !ok || wsTab.ID != "tab-2" || wsSite.Group.ID != "grp-c1" {
		t.Errorf("LocatePaneInWorkspace(p5) = %v %v %v", wsTab, wsSite, ok)
	}
	if _, _, ok := LocatePaneInWorkspace(snap, "nope"); ok {
		t.Error("LocatePaneInWorkspace(nope) reported found")
	}
	if _, _, ok := LocatePaneInWorkspace(nil, "p1"); ok {
		t.Error("LocatePaneInWorkspace(nil) reported found")
	}
}

func TestLaneAndGroupOfPane(t *testing.T) {
	snap := navTree()
	tab := &snap.Tabs[0]

	if l := LaneOfPane(tab, "p4"); l == nil || l.ID != "lane-b" {
		t.Errorf("LaneOfPane(p4) = %v, want lane-b", l)
	}
	if g := GroupOfPane(tab, "p2"); g == nil || g.ID != "grp-a1" {
		t.Errorf("GroupOfPane(p2) = %v, want grp-a1", g)
	}
	if l := LaneOfPane(tab, "nope"); l != nil {
		t.Errorf("LaneOfPane(nope) = %v, want nil", l)
	}
	if g := GroupOfPane(tab, "nope"); g != nil {
		t.Errorf("GroupOfPane(nope) = %v, want nil", g)
	}
}

func TestFindByID(t *testing.T) {
	snap := navTree()
	tab := &snap.Tabs[0]

	if got := FindTabByID(snap, "tab-2"); got == nil || got.Title != "beta" {
		t.Errorf("FindTabByID(tab-2) = %v", got)
	}
	if got := FindTabByID(snap, "nope"); got != nil {
		t.Errorf("FindTabByID(nope) = %v, want nil", got)
	}
	if got := FindTabByID(nil, "tab-1"); got != nil {
		t.Errorf("FindTabByID(nil) = %v, want nil", got)
	}

	if got := FindLaneByID(tab, "lane-b"); got == nil || got.Name != "right" {
		t.Errorf("FindLaneByID(lane-b) = %v", got)
	}
	if got := FindLaneByID(tab, "lane-c"); got != nil {
		t.Errorf("FindLaneByID(lane-c) = %v, want nil (it is in tab-2)", got)
	}
	if got := FindLaneByID(nil, "lane-a"); got != nil {
		t.Errorf("FindLaneByID(nil) = %v, want nil", got)
	}

	if got := FindGroupByID(tab, "grp-b1"); got == nil || got.ID != "grp-b1" {
		t.Errorf("FindGroupByID(grp-b1) = %v", got)
	}
	if got := FindGroupByID(tab, "grp-c1"); got != nil {
		t.Errorf("FindGroupByID(grp-c1) = %v, want nil", got)
	}
	if got := FindGroupByID(nil, "grp-a1"); got != nil {
		t.Errorf("FindGroupByID(nil) = %v, want nil", got)
	}
}

// ---------------------------------------------------------------------------
// Counting
// ---------------------------------------------------------------------------

func TestCounting(t *testing.T) {
	snap := navTree()
	tab := &snap.Tabs[0]

	cases := []struct {
		name string
		got  int
		want int
	}{
		{"group grp-a1", CountPanesInGroup(&tab.Lanes[0].PaneGroups[0]), 2},
		{"group grp-a2", CountPanesInGroup(&tab.Lanes[0].PaneGroups[1]), 1},
		{"lane-a", CountPanesInLane(&tab.Lanes[0]), 3},
		{"lane-b", CountPanesInLane(&tab.Lanes[1]), 1},
		{"tab-1", CountPanesInTab(tab), 4},
		{"tab-2", CountPanesInTab(&snap.Tabs[1]), 1},
		{"workspace", CountPanesInWorkspace(snap), 5},
		{"groups in tab-1", CountGroupsInTab(tab), 3},
		{"nil group", CountPanesInGroup(nil), 0},
		{"nil lane", CountPanesInLane(nil), 0},
		{"nil tab", CountPanesInTab(nil), 0},
		{"nil workspace", CountPanesInWorkspace(nil), 0},
		{"nil tab groups", CountGroupsInTab(nil), 0},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, c.got, c.want)
		}
	}

	// Counting includes shared panes; only the Where-collectors filter.
	if CountPanesInGroup(&tab.Lanes[0].PaneGroups[0]) != 2 {
		t.Error("CountPanesInGroup must not exclude the shared pane p2")
	}
}

// ---------------------------------------------------------------------------
// Collecting
// ---------------------------------------------------------------------------

func TestPaneIDCollectors(t *testing.T) {
	snap := navTree()
	tab := &snap.Tabs[0]

	paneIDs(t, PaneIDsInGroup(&tab.Lanes[0].PaneGroups[0]), "p1", "p2")
	paneIDs(t, PaneIDsInLane(&tab.Lanes[0]), "p1", "p2", "p3")
	paneIDs(t, PaneIDsInTab(tab), "p1", "p2", "p3", "p4")
	paneIDs(t, PaneIDsInWorkspace(snap), "p1", "p2", "p3", "p4", "p5")

	paneIDs(t, PaneIDsInGroup(nil))
	paneIDs(t, PaneIDsInLane(nil))
	paneIDs(t, PaneIDsInTab(nil))
	paneIDs(t, PaneIDsInWorkspace(nil))
}

// TestPaneIDCollectorsWhere pins the predicate contract that internal/actors'
// ##cmd broadcast depends on: rejected panes are counted, not silently dropped,
// because the command reports "skipped N shared panes" to the user.
func TestPaneIDCollectorsWhere(t *testing.T) {
	snap := navTree()
	tab := &snap.Tabs[0]
	notShared := func(p *PaneSnapshot) bool { return !p.Sharing }

	ids, skipped := PaneIDsInGroupWhere(&tab.Lanes[0].PaneGroups[0], notShared)
	paneIDs(t, ids, "p1")
	if skipped != 1 {
		t.Errorf("group skipped = %d, want 1", skipped)
	}

	ids, skipped = PaneIDsInLaneWhere(&tab.Lanes[0], notShared)
	paneIDs(t, ids, "p1", "p3")
	if skipped != 1 {
		t.Errorf("lane skipped = %d, want 1", skipped)
	}

	ids, skipped = PaneIDsInTabWhere(tab, notShared)
	paneIDs(t, ids, "p1", "p3", "p4")
	if skipped != 1 {
		t.Errorf("tab skipped = %d, want 1", skipped)
	}

	ids, skipped = PaneIDsInWorkspaceWhere(snap, notShared)
	paneIDs(t, ids, "p1", "p3", "p4", "p5")
	if skipped != 1 {
		t.Errorf("workspace skipped = %d, want 1", skipped)
	}

	// A nil predicate accepts everything and skips nothing — this is what makes
	// the plain PaneIDsIn* wrappers exact.
	ids, skipped = PaneIDsInTabWhere(tab, nil)
	paneIDs(t, ids, "p1", "p2", "p3", "p4")
	if skipped != 0 {
		t.Errorf("nil predicate skipped = %d, want 0", skipped)
	}

	// A predicate that rejects everything yields no ids and skips all.
	ids, skipped = PaneIDsInTabWhere(tab, func(*PaneSnapshot) bool { return false })
	paneIDs(t, ids)
	if skipped != 4 {
		t.Errorf("reject-all skipped = %d, want 4", skipped)
	}
}

// ---------------------------------------------------------------------------
// Selector resolution
// ---------------------------------------------------------------------------

func TestResolveTab(t *testing.T) {
	snap := navTree()

	cases := []struct {
		arg  string
		want string // tab ID, "" = nil
		why  string
	}{
		{"", "tab-1", "empty selector picks the active tab"},
		{"tab-2", "tab-2", "exact id"},
		{"1", "tab-1", "1-based index"},
		{"2", "tab-2", "1-based index"},
		{"0", "", "index is 1-based, so 0 is out of range"},
		{"3", "", "index past the end"},
		{"beta", "tab-2", "title match"},
		{"nope", "", "no match"},
	}
	for _, c := range cases {
		got := ResolveTab(snap, c.arg)
		if c.want == "" {
			if got != nil {
				t.Errorf("ResolveTab(%q) = %q, want nil (%s)", c.arg, got.ID, c.why)
			}
			continue
		}
		if got == nil || got.ID != c.want {
			t.Errorf("ResolveTab(%q) = %v, want %q (%s)", c.arg, got, c.want, c.why)
		}
	}

	if got := ResolveTab(nil, ""); got != nil {
		t.Errorf("ResolveTab(nil) = %v, want nil", got)
	}
	if got := ResolveTab(&WorkspaceSnapshot{}, ""); got != nil {
		t.Errorf("ResolveTab(no tabs) = %v, want nil", got)
	}

	// Empty selector falls back to the first tab when ActiveTabID names nothing.
	orphan := navTree()
	orphan.ActiveTabID = "gone"
	if got := ResolveTab(orphan, ""); got == nil || got.ID != "tab-1" {
		t.Errorf("ResolveTab with stale ActiveTabID = %v, want tab-1", got)
	}
}

// TestResolveTabIDBeatsIndex pins the resolution ORDER. A workspace whose second
// tab is literally named "1" must still resolve "1" to that tab by id, not to
// the first tab by index — otherwise an id that happens to look like a number
// becomes unreachable.
func TestResolveTabIDBeatsIndex(t *testing.T) {
	snap := &WorkspaceSnapshot{Tabs: []TabSnapshot{
		{ID: "tab-x", Title: "first"},
		{ID: "1", Title: "second"},
	}}
	if got := ResolveTab(snap, "1"); got == nil || got.Title != "second" {
		t.Errorf("ResolveTab(\"1\") = %v, want the tab whose ID is \"1\"", got)
	}

	// Same for title-vs-index: index wins over title, since listings number rows.
	titled := &WorkspaceSnapshot{Tabs: []TabSnapshot{
		{ID: "tab-x", Title: "2"},
		{ID: "tab-y", Title: "other"},
	}}
	if got := ResolveTab(titled, "2"); got == nil || got.ID != "tab-y" {
		t.Errorf("ResolveTab(\"2\") = %v, want tab-y (index beats title)", got)
	}
}

func TestResolveLane(t *testing.T) {
	snap := navTree()
	tab := &snap.Tabs[0]

	cases := []struct {
		arg  string
		want string
		why  string
	}{
		{"", "lane-a", "empty selector picks the lane holding the tab's active pane"},
		{"lane-b", "lane-b", "exact id"},
		{"2", "lane-b", "1-based index"},
		{"0", "", "index is 1-based"},
		{"9", "", "index past the end"},
		{"right", "lane-b", "name match"},
		{"nope", "", "no match"},
	}
	for _, c := range cases {
		got := ResolveLane(tab, c.arg)
		if c.want == "" {
			if got != nil {
				t.Errorf("ResolveLane(%q) = %q, want nil (%s)", c.arg, got.ID, c.why)
			}
			continue
		}
		if got == nil || got.ID != c.want {
			t.Errorf("ResolveLane(%q) = %v, want %q (%s)", c.arg, got, c.want, c.why)
		}
	}

	if got := ResolveLane(nil, ""); got != nil {
		t.Errorf("ResolveLane(nil) = %v, want nil", got)
	}
	if got := ResolveLane(&TabSnapshot{}, ""); got != nil {
		t.Errorf("ResolveLane(no lanes) = %v, want nil", got)
	}

	// No active pane at all: fall back to the first lane.
	noActive := navTree()
	noActive.Tabs[0].ActivePaneID = ""
	if got := ResolveLane(&noActive.Tabs[0], ""); got == nil || got.ID != "lane-a" {
		t.Errorf("ResolveLane with no active pane = %v, want lane-a", got)
	}

	// Active pane that lives nowhere: also fall back to the first lane.
	orphan := navTree()
	orphan.Tabs[0].ActivePaneID = "ghost"
	if got := ResolveLane(&orphan.Tabs[0], ""); got == nil || got.ID != "lane-a" {
		t.Errorf("ResolveLane with orphan active pane = %v, want lane-a", got)
	}

	// The active-pane fallback really does pick by containment, not by position:
	// point the tab at p4, which lives in the SECOND lane.
	shifted := navTree()
	shifted.Tabs[0].ActivePaneID = "p4"
	if got := ResolveLane(&shifted.Tabs[0], ""); got == nil || got.ID != "lane-b" {
		t.Errorf("ResolveLane for active pane p4 = %v, want lane-b", got)
	}
}

func TestResolveGroup(t *testing.T) {
	snap := navTree()
	lane := &snap.Tabs[0].Lanes[0]

	cases := []struct {
		arg  string
		want string
		why  string
	}{
		{"", "grp-a2", "empty selector picks the group holding the lane's active pane (p3)"},
		{"grp-a1", "grp-a1", "exact id"},
		{"1", "grp-a1", "1-based index"},
		{"2", "grp-a2", "1-based index"},
		{"0", "", "index is 1-based"},
		{"5", "", "index past the end"},
		{"nope", "", "groups have no name, so nothing else matches"},
	}
	for _, c := range cases {
		got := ResolveGroup(lane, c.arg)
		if c.want == "" {
			if got != nil {
				t.Errorf("ResolveGroup(%q) = %q, want nil (%s)", c.arg, got.ID, c.why)
			}
			continue
		}
		if got == nil || got.ID != c.want {
			t.Errorf("ResolveGroup(%q) = %v, want %q (%s)", c.arg, got, c.want, c.why)
		}
	}

	if got := ResolveGroup(nil, ""); got != nil {
		t.Errorf("ResolveGroup(nil) = %v, want nil", got)
	}
	if got := ResolveGroup(&LaneSnapshot{}, ""); got != nil {
		t.Errorf("ResolveGroup(no groups) = %v, want nil", got)
	}

	// Lane with no active pane falls back to the first group.
	if got := ResolveGroup(&snap.Tabs[0].Lanes[1], ""); got == nil || got.ID != "grp-b1" {
		t.Errorf("ResolveGroup(lane-b, \"\") = %v, want grp-b1", got)
	}
}

func TestResolvePane(t *testing.T) {
	snap := navTree()
	group := &snap.Tabs[0].Lanes[0].PaneGroups[0]

	cases := []struct {
		arg  string
		want string
		why  string
	}{
		{"", "p1", "empty selector picks the group's active pane"},
		{"p2", "p2", "exact id"},
		{"two", "p2", "title match"},
		{"deuce", "p2", "given-name match"},
		{"1", "", "panes are NOT resolved by index"},
		{"nope", "", "no match"},
	}
	for _, c := range cases {
		got := ResolvePane(group, c.arg)
		if c.want == "" {
			if got != nil {
				t.Errorf("ResolvePane(%q) = %q, want nil (%s)", c.arg, got.ID, c.why)
			}
			continue
		}
		if got == nil || got.ID != c.want {
			t.Errorf("ResolvePane(%q) = %v, want %q (%s)", c.arg, got, c.want, c.why)
		}
	}

	if got := ResolvePane(nil, ""); got != nil {
		t.Errorf("ResolvePane(nil) = %v, want nil", got)
	}
	if got := ResolvePane(&PaneGroupSnapshot{}, ""); got != nil {
		t.Errorf("ResolvePane(no panes) = %v, want nil", got)
	}

	// No active pane, and a stale active pane, both fall back to the first.
	if got := ResolvePane(&snap.Tabs[0].Lanes[1].PaneGroups[0], ""); got == nil || got.ID != "p4" {
		t.Errorf("ResolvePane(grp-b1, \"\") = %v, want p4", got)
	}
	stale := navTree()
	stale.Tabs[0].Lanes[0].PaneGroups[0].ActivePaneID = "ghost"
	if got := ResolvePane(&stale.Tabs[0].Lanes[0].PaneGroups[0], ""); got == nil || got.ID != "p1" {
		t.Errorf("ResolvePane with stale ActivePaneID = %v, want p1", got)
	}

	// The returned pointer addresses the group, so callers may write through it.
	p := ResolvePane(group, "p1")
	p.Title = "renamed"
	if group.Panes[0].Title != "renamed" {
		t.Error("ResolvePane returned a pointer into a copy, not into the group")
	}
}

// TestResolveChainMatchesBroadcast walks the whole selector chain the way
// internal/actors' ##cmd does, to confirm the promoted helpers compose the same
// way they did when they lived there.
func TestResolveChainMatchesBroadcast(t *testing.T) {
	snap := navTree()

	tab := ResolveTab(snap, "")
	if tab == nil || tab.ID != "tab-1" {
		t.Fatalf("tab = %v", tab)
	}
	lane := ResolveLane(tab, "")
	if lane == nil || lane.ID != "lane-a" {
		t.Fatalf("lane = %v", lane)
	}
	group := ResolveGroup(lane, "")
	if group == nil || group.ID != "grp-a2" {
		t.Fatalf("group = %v", group)
	}
	pane := ResolvePane(group, "")
	if pane == nil || pane.ID != "p3" {
		t.Fatalf("pane = %v", pane)
	}

	// Explicit selectors at each level override the active-entity default.
	tab = ResolveTab(snap, "beta")
	lane = ResolveLane(tab, "1")
	group = ResolveGroup(lane, "grp-c1")
	pane = ResolvePane(group, "five")
	if pane == nil || pane.ID != "p5" {
		t.Fatalf("explicit chain resolved to %v, want p5", pane)
	}
}
