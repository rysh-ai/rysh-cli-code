// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"strings"
	"sync"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

// Tests for W1-2: ansaPane carries Meta.
//
// Why this is its own work item rather than a field added in passing: ANSA
// could not tell a fleet pane from the human's shell, because the snapshot's
// Meta was dropped on the floor at the flattening step. Everything `--fleet`
// does rests on this map arriving intact, and a map that arrives EMPTY looks
// exactly like a session with no fleet in it — the failure mode this track
// keeps paying for.

// ansaSnapshotWithMeta builds a workspace snapshot shaped like a real session:
// two fleet panes and one pane belonging to nobody.
func ansaSnapshotWithMeta() *domain.WorkspaceSnapshot {
	return &domain.WorkspaceSnapshot{
		Tabs: []domain.TabSnapshot{{
			ID: "tab-1",
			Lanes: []domain.LaneSnapshot{{
				ID: "lane-1",
				PaneGroups: []domain.PaneGroupSnapshot{{
					ID: "group-1",
					Panes: []domain.PaneSnapshot{
						{
							ID: "pane-mgr", GivenName: "mgr-01", Title: "eager-lynx",
							Meta: map[string]string{
								"fleet.name": "agent-fleet",
								"fleet.role": "manager",
								"fleet.unit": "01",
							},
						},
						{
							ID: "pane-wkr", GivenName: "wkr-01", Title: "brave-otter",
							Meta: map[string]string{
								"fleet.name": "agent-fleet",
								"fleet.role": "worker",
								"fleet.unit": "01",
							},
						},
						{
							// The human's shell. No fleet meta at all, which is
							// what makes it safe from --fleet.
							ID: "pane-human", GivenName: "shell", Title: "calm-heron",
						},
					},
				}},
			}},
		}},
	}
}

// TestAnsaPaneCarriesMetaThroughTheFlattening: the snapshot → router hop.
//
// This is the hop the field was being lost at, and losing it is invisible:
// every pane still routes, every name still resolves, and only a fleet-shaped
// question gets the wrong answer.
func TestAnsaPaneCarriesMetaThroughTheFlattening(t *testing.T) {
	panes := ansaPanesFromWorkspace(ansaSnapshotWithMeta())
	if len(panes) != 3 {
		t.Fatalf("want 3 panes, got %d", len(panes))
	}

	byID := map[string]ansaPane{}
	for _, p := range panes {
		byID[p.ID] = p
	}

	if got := byID["pane-mgr"].fleetRole(); got != "manager" {
		t.Errorf("pane-mgr fleet.role = %q, want %q — meta did not survive the hop", got, "manager")
	}
	if got := byID["pane-wkr"].fleetRole(); got != "worker" {
		t.Errorf("pane-wkr fleet.role = %q, want %q", got, "worker")
	}
	if got := byID["pane-mgr"].meta("fleet.unit"); got != "01" {
		t.Errorf("pane-mgr fleet.unit = %q, want %q — only some keys survived", got, "01")
	}
	if got := byID["pane-human"].fleetRole(); got != "" {
		t.Errorf("the human's shell reports fleet.role %q — it has no fleet meta and must "+
			"report none, because absence IS the exclusion --fleet relies on", got)
	}
}

// TestAnsaPaneMetaIsAlwaysANonNilMap: a pane with no metadata gets an empty
// map, never nil.
func TestAnsaPaneMetaIsAlwaysANonNilMap(t *testing.T) {
	panes := ansaPanesFromWorkspace(ansaSnapshotWithMeta())
	for _, p := range panes {
		if p.Meta == nil {
			t.Fatalf("pane %s carries a nil Meta map", p.ID)
		}
	}
	// And the zero value must be readable without a guard, because a refusal
	// path can hand one back.
	var zero ansaPane
	if got := zero.fleetRole(); got != "" {
		t.Fatalf("zero ansaPane fleetRole() = %q", got)
	}
	if got := zero.meta("anything"); got != "" {
		t.Fatalf("zero ansaPane meta() = %q", got)
	}
}

// TestAnsaPaneMetaIsCopiedNotAliased.
//
// A router holding a live reference into a snapshot is a data race waiting for
// -race to find it, and worse than a race: mutating the snapshot afterwards
// would silently change what the router believes about the session. This test
// mutates the source AFTER flattening and requires the router's view to be
// unmoved.
func TestAnsaPaneMetaIsCopiedNotAliased(t *testing.T) {
	snap := ansaSnapshotWithMeta()
	panes := ansaPanesFromWorkspace(snap)

	src := snap.Tabs[0].Lanes[0].PaneGroups[0].Panes[0].Meta
	src["fleet.role"] = "ceo"
	src["fleet.injected"] = "yes"

	var mgr ansaPane
	for _, p := range panes {
		if p.ID == "pane-mgr" {
			mgr = p
		}
	}
	if got := mgr.fleetRole(); got != "manager" {
		t.Fatalf("mutating the snapshot changed the router's view: fleet.role = %q, want %q — "+
			"Meta is aliased, not copied", got, "manager")
	}
	if _, present := mgr.Meta["fleet.injected"]; present {
		t.Fatal("a key added to the snapshot after flattening appeared in the router's copy")
	}
}

// TestAnsaPaneMetaCopyIsRaceFreeUnderConcurrentSnapshotWrites is the -race
// half of the test above: the copy must be safe while the source is written.
func TestAnsaPaneMetaCopyIsRaceFreeUnderConcurrentSnapshotWrites(t *testing.T) {
	snap := ansaSnapshotWithMeta()
	var wg sync.WaitGroup
	stop := make(chan struct{})

	// One writer, mutating its OWN copy of the map — the point is that the
	// flattened panes never touch the source after ansaCopyMeta returns.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				panes := ansaPanesFromWorkspace(snap)
				for _, p := range panes {
					p.Meta["scratch"] = "written by the reader's own copy"
				}
			}
		}
	}()

	for i := 0; i < 200; i++ {
		panes := ansaPanesFromWorkspace(snap)
		for _, p := range panes {
			_ = p.fleetRole()
		}
	}
	close(stop)
	wg.Wait()

	// The source is untouched by any of it.
	if got := snap.Tabs[0].Lanes[0].PaneGroups[0].Panes[0].Meta["scratch"]; got != "" {
		t.Fatalf("writing to a flattened pane's Meta reached the snapshot: %q", got)
	}
}

// ---------------------------------------------------------------------------
// the --meta filter
// ---------------------------------------------------------------------------

func TestAnsaMetaFilterSelectsAndExcludes(t *testing.T) {
	panes := ansaPanesFromWorkspace(ansaSnapshotWithMeta())

	cases := []struct {
		arg  string
		want []string
	}{
		{"fleet.role", []string{"pane-mgr", "pane-wkr"}},
		{"fleet.role=worker", []string{"pane-wkr"}},
		{"fleet.role=manager", []string{"pane-mgr"}},
		{"fleet.role=ceo", nil},
		{"fleet.name=agent-fleet", []string{"pane-mgr", "pane-wkr"}},
		{"nonexistent.key", nil},
	}
	for _, tc := range cases {
		f, err := parseAnsaMetaFilter(tc.arg)
		if err != nil {
			t.Fatalf("parseAnsaMetaFilter(%q): %v", tc.arg, err)
		}
		var got []string
		for _, p := range panes {
			if f.matches(p) {
				got = append(got, p.ID)
			}
		}
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("--meta %s selected %v, want %v", tc.arg, got, tc.want)
		}
	}
}

// TestAnsaMetaFilterNeverMatchesThePaneWithNoMeta is the safety half stated on
// its own, because it is the property `--fleet` is built on and it must not be
// able to regress quietly behind a passing table test.
func TestAnsaMetaFilterNeverMatchesThePaneWithNoMeta(t *testing.T) {
	human := ansaPane{ID: "pane-human", GivenName: "shell", Meta: map[string]string{}}
	for _, arg := range []string{"fleet.role", "fleet.role=worker", "fleet.name", "anything"} {
		f, err := parseAnsaMetaFilter(arg)
		if err != nil {
			t.Fatalf("parse %q: %v", arg, err)
		}
		if f.matches(human) {
			t.Errorf("--meta %s matched a pane with no metadata", arg)
		}
	}
}

// TestAnsaMetaFilterTreatsAnEmptyValueAsAbsent: fleetctl writes fleet.role on
// every pane it creates, so a blank value means "the writer had nothing to
// say", not "this pane holds an empty role".
func TestAnsaMetaFilterTreatsAnEmptyValueAsAbsent(t *testing.T) {
	blank := ansaPane{ID: "pane-blank", Meta: map[string]string{"fleet.role": "   "}}
	f, _ := parseAnsaMetaFilter("fleet.role")
	if f.matches(blank) {
		t.Fatal("a blank fleet.role matched --meta fleet.role — a pane would join a fleet " +
			"operation on the strength of whitespace")
	}
	if blank.fleetRole() != "" {
		t.Fatalf("fleetRole() = %q, want empty for a blank value", blank.fleetRole())
	}
}

// TestAnsaWhoArgsRefuseWhatTheyDoNotUnderstand — the F-19 rule. A silently
// dropped `--met fleet.role` lists the whole session while the caller believes
// it asked for the fleet, and on this surface that answer feeds a decision
// about who to interrupt.
func TestAnsaWhoArgsRefuseWhatTheyDoNotUnderstand(t *testing.T) {
	for _, args := range [][]string{
		{"--met", "fleet.role"},
		{"fleet.role"},
		{"--meta"},
		{"--meta", "=worker"},
		{"--meta="},
	} {
		if _, err := parseAnsaWhoArgs(args); err == nil {
			t.Errorf("parseAnsaWhoArgs(%v) accepted an argument it does not implement", args)
		}
	}

	// Both spellings of the flag work, and no argument means no filter.
	for _, args := range [][]string{
		{"--meta", "fleet.role"},
		{"--meta=fleet.role"},
	} {
		f, err := parseAnsaWhoArgs(args)
		if err != nil {
			t.Fatalf("parseAnsaWhoArgs(%v): %v", args, err)
		}
		if f.key != "fleet.role" || f.hasValue {
			t.Errorf("parseAnsaWhoArgs(%v) = %+v", args, f)
		}
	}
	f, err := parseAnsaWhoArgs(nil)
	if err != nil || f.active() {
		t.Fatalf("no arguments should mean no filter, got %+v (%v)", f, err)
	}
}

// TestAnsaUsageDocumentsTheMetaFlag: a flag that works but is undocumented is
// invisible, which is precisely how F-19 survived a whole epic.
func TestAnsaUsageDocumentsTheMetaFlag(t *testing.T) {
	var out strings.Builder
	w := &WorkspaceActor{}
	_ = w.ansaUsage(&out, "test")
	if !strings.Contains(out.String(), "--meta") {
		t.Fatalf("##ansa usage does not document --meta:\n%s", out.String())
	}
}
