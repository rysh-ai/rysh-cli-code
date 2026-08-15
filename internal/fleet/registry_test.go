// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"strings"
	"testing"
)

// The registry model (design 028 §6.5, `E-40`). Pure: no NATS, no KV, no clock,
// so every rule below is asserted directly rather than inferred from a live
// session.

func mustRegister(t *testing.T, r *Registry, f Fleet) *Fleet {
	t.Helper()
	out, err := r.Register(f)
	if err != nil {
		t.Fatalf("Register(%q): %v", f.Name, err)
	}
	return out
}

// TestAFleetNameMustAlsoBeALegalBoardID.
//
// ONE NAMESPACE, and this is where it is enforced. A fleet's board id defaults
// to its name and a board id becomes a NATS subject token, so a fleet called
// "epic.07" would address a board nobody subscribes to. Registration is the
// only moment this can be refused; afterwards the symptom is a board that is
// silently empty, which reads as a quiet fleet.
func TestAFleetNameMustAlsoBeALegalBoardID(t *testing.T) {
	r := New()
	for _, bad := range []string{"", "epic.07", "epic 07", "*", ">", "-lead"} {
		if _, err := r.Register(Fleet{Name: bad}); err == nil {
			t.Errorf("Register(%q) was accepted; it cannot be a board id", bad)
		}
	}
	f := mustRegister(t, r, Fleet{Name: "EPIC-07"})
	if f.Name != "epic-07" || f.BoardID != "epic-07" {
		t.Fatalf("registered as %+v; a name normalises and the board defaults to it", f)
	}
}

// TestRegisteringTwiceKeepsTheMembers.
//
// Registration and membership arrive from different places at different times:
// the driver registers the fleet, then adds each agent as its pane is created,
// and it re-registers whenever it restarts. A Register that emptied the roster
// would silently forget every agent already running — the fleet would look
// half-built while its panes carried on working.
func TestRegisteringTwiceKeepsTheMembers(t *testing.T) {
	r := New()
	mustRegister(t, r, Fleet{Name: "epic-07", Source: "first"})
	if err := r.UpsertMember("epic-07", Member{PaneID: "p1", Role: "worker"}); err != nil {
		t.Fatalf("UpsertMember: %v", err)
	}

	again := mustRegister(t, r, Fleet{Name: "epic-07", Source: "second"})
	if len(again.Members) != 1 || again.Members[0].PaneID != "p1" {
		t.Fatalf("re-registering dropped the roster: %+v", again.Members)
	}
	if again.Source != "second" {
		t.Errorf("identity did not update: source = %q, want the new one", again.Source)
	}
}

// TestMembersAreKeyedByPaneIDNotLabel.
//
// A label is a pane's given-name and given-names are unique per LANE, not per
// session — a fleet legitimately holds two agents called `wkr-01` in different
// lanes. Keying on the label would merge them, and the fleet would report half
// its real size with nothing to indicate a loss.
func TestMembersAreKeyedByPaneIDNotLabel(t *testing.T) {
	r := New()
	mustRegister(t, r, Fleet{Name: "epic-07"})
	_ = r.UpsertMember("epic-07", Member{PaneID: "p1", Label: "wkr-01", Role: "worker"})
	_ = r.UpsertMember("epic-07", Member{PaneID: "p2", Label: "wkr-01", Role: "worker"})

	if got := r.Get("epic-07"); len(got.Members) != 2 {
		t.Fatalf("two panes sharing a label collapsed to %d member(s)", len(got.Members))
	}

	// The same pane again is an UPDATE, not a duplicate.
	_ = r.UpsertMember("epic-07", Member{PaneID: "p1", Label: "wkr-01", Role: "manager"})
	got := r.Get("epic-07")
	if len(got.Members) != 2 {
		t.Fatalf("re-adding one pane produced %d members", len(got.Members))
	}
	for _, m := range got.Members {
		if m.PaneID == "p1" && m.Role != "manager" {
			t.Errorf("upsert did not update the member: %+v", m)
		}
	}
}

// TestReconcileTreatsAnEmptySetAsIDoNotKnow.
//
// THIS IS THE MOST IMPORTANT RULE IN THE FILE. The live-pane lookup can fail —
// a busy workspace, a timed-out snapshot — and if a failure were reported as
// "no panes exist", reconciliation would empty every fleet's roster at once. A
// registry that overcounts is bad; one that silently empties itself takes the
// session's whole org chart with it and cannot be recovered from memory.
func TestReconcileTreatsAnEmptySetAsIDoNotKnow(t *testing.T) {
	r := New()
	mustRegister(t, r, Fleet{Name: "epic-07"})
	_ = r.UpsertMember("epic-07", Member{PaneID: "p1"})
	_ = r.UpsertMember("epic-07", Member{PaneID: "p2"})

	if dropped := r.Reconcile(nil); dropped != 0 {
		t.Fatalf("a nil live-set dropped %d members", dropped)
	}
	if dropped := r.Reconcile(map[string]bool{}); dropped != 0 {
		t.Fatalf("an empty live-set dropped %d members", dropped)
	}
	if got := r.Get("epic-07"); len(got.Members) != 2 {
		t.Fatalf("roster is %d after two non-answers, want 2", len(got.Members))
	}

	// A real answer DOES drop what is gone.
	if dropped := r.Reconcile(map[string]bool{"p1": true}); dropped != 1 {
		t.Fatalf("dropped %d, want the one pane that no longer exists", dropped)
	}
	if got := r.Get("epic-07"); len(got.Members) != 1 || got.Members[0].PaneID != "p1" {
		t.Fatalf("roster after reconcile = %+v, want only p1", got.Members)
	}
}

// TestReconcileNeverRemovesAFleet. A fleet whose panes have all closed is a
// fleet that is DOWN — a state somebody decides, not something to infer from a
// snapshot. Inferring it would delete the record that says where the work was
// and how to resume it.
func TestReconcileNeverRemovesAFleet(t *testing.T) {
	r := New()
	mustRegister(t, r, Fleet{Name: "epic-07", RoadmapDir: "new_roadmap/tracks/x"})
	_ = r.UpsertMember("epic-07", Member{PaneID: "p1"})

	r.Reconcile(map[string]bool{"someone-else": true})

	f := r.Get("epic-07")
	if f == nil {
		t.Fatal("the fleet was deleted by a reconcile; only its members may be")
	}
	if len(f.Members) != 0 {
		t.Fatalf("members = %+v, want empty", f.Members)
	}
	if f.RoadmapDir == "" {
		t.Error("the fleet's identity was lost with its members")
	}
}

// TestForgetLeavesThePanesAlone is a statement about SCOPE, pinned so a later
// change cannot quietly turn a registry operation into a teardown. This is the
// live failure that motivated the registry: a sibling's teardown deleted a
// manifest and the agents carried on running, unnamed.
func TestForgetLeavesThePanesAlone(t *testing.T) {
	r := New()
	mustRegister(t, r, Fleet{Name: "epic-07"})
	_ = r.UpsertMember("epic-07", Member{PaneID: "p1"})

	if !r.Forget("epic-07") {
		t.Fatal("Forget reported the fleet was not there")
	}
	if r.Get("epic-07") != nil {
		t.Fatal("the fleet survived Forget")
	}
	if r.Forget("epic-07") {
		t.Error("forgetting an absent fleet reported success; that is usually a typo " +
			"and reporting it as done sends the caller away believing it is gone")
	}
}

// TestFleetOfPaneAnswersMembership — what makes a scoped interrupt confirmable
// rather than a guess from pane meta (`E-41` matches meta; this is the
// authority it will consult).
func TestFleetOfPaneAnswersMembership(t *testing.T) {
	r := New()
	mustRegister(t, r, Fleet{Name: "epic-07"})
	mustRegister(t, r, Fleet{Name: "epic-08"})
	_ = r.UpsertMember("epic-07", Member{PaneID: "p1"})
	_ = r.UpsertMember("epic-08", Member{PaneID: "p2"})

	if got := r.FleetOfPane("p1"); got != "epic-07" {
		t.Errorf("FleetOfPane(p1) = %q, want epic-07", got)
	}
	if got := r.FleetOfPane("p2"); got != "epic-08" {
		t.Errorf("FleetOfPane(p2) = %q, want epic-08", got)
	}
	if got := r.FleetOfPane("stranger"); got != "" {
		t.Errorf("FleetOfPane(stranger) = %q, want empty", got)
	}
}

// TestListIsOrdered: Go randomises map iteration, and a fleet list that
// reorders itself between two calls looks like the session changed.
func TestListIsOrdered(t *testing.T) {
	r := New()
	for _, n := range []string{"epic-08", "epic-07", "board"} {
		mustRegister(t, r, Fleet{Name: n})
	}
	var names []string
	for _, f := range r.List() {
		names = append(names, f.Name)
	}
	if strings.Join(names, ",") != "board,epic-07,epic-08" {
		t.Fatalf("List() = %v, want a stable alphabetical order", names)
	}
}

// TestGetReturnsACopy: a reader must not be able to mutate the registry through
// a pointer it was handed for reading.
func TestGetReturnsACopy(t *testing.T) {
	r := New()
	mustRegister(t, r, Fleet{Name: "epic-07"})
	_ = r.UpsertMember("epic-07", Member{PaneID: "p1", Role: "worker"})

	got := r.Get("epic-07")
	got.State = "tampered"
	got.Members[0].Role = "tampered"

	fresh := r.Get("epic-07")
	if fresh.State == "tampered" || fresh.Members[0].Role == "tampered" {
		t.Fatal("Get handed out a live pointer: a reader can rewrite the registry")
	}
}

// TestStateRejectsAnUnknownValue — the lifecycle is three values, and a typo
// must not invent a fourth that nothing else understands.
func TestStateRejectsAnUnknownValue(t *testing.T) {
	r := New()
	mustRegister(t, r, Fleet{Name: "epic-07"})
	if err := r.SetState("epic-07", "runnning"); err == nil {
		t.Fatal("a misspelled state was accepted")
	}
	if err := r.SetState("nope", StateUp); err == nil {
		t.Fatal("a state change to an unknown fleet was accepted")
	}
	if err := r.SetState("epic-07", StateUp); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if got := r.Get("epic-07").State; got != StateUp {
		t.Fatalf("state = %q, want %q", got, StateUp)
	}
}

// TestTheCapRefusesTheFourthFleetUp is founder gate `D-14`, ruled at 3.
//
// ENFORCED IN THE REGISTRY, NOT IN THE DRIVER, and this test is what makes that
// meaningful: a cap the driver respects holds until somebody runs `fleetctl up`
// twice by hand, or a second driver starts, or a script loops.
func TestTheCapRefusesTheFourthFleetUp(t *testing.T) {
	r := New()
	for _, n := range []string{"epic-01", "epic-02", "epic-03", "epic-04"} {
		mustRegister(t, r, Fleet{Name: n})
	}

	for _, n := range []string{"epic-01", "epic-02", "epic-03"} {
		if err := r.SetState(n, StateUp); err != nil {
			t.Fatalf("SetState(%s, up): %v", n, err)
		}
	}

	err := r.SetState("epic-04", StateUp)
	if err == nil {
		t.Fatal("a fourth fleet came up; the cap is not enforced")
	}
	// The refusal must NAME what is already up, or the operator cannot tell
	// which fleet to take down first.
	for _, want := range []string{"epic-01", "epic-02", "epic-03"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
	if got := r.Get("epic-04").State; got != StateRegistered {
		t.Fatalf("the refused fleet is now %q; a refusal must change nothing", got)
	}
}

// TestRegisteringIsFreeUnderTheCap — registered fleets cost a row and nothing
// else, which is what lets a driver register twenty-five and promote three.
func TestRegisteringIsFreeUnderTheCap(t *testing.T) {
	r := New()
	for i := 0; i < 25; i++ {
		name := "epic-" + string(rune('a'+i%26))
		if _, err := r.Register(Fleet{Name: name}); err != nil {
			t.Fatalf("Register(%s): %v", name, err)
		}
	}
	if r.Len() < 20 {
		t.Fatalf("only %d fleets registered; registration must not be capped", r.Len())
	}
}

// TestASlotFreesWhenAFleetGoesDown — the promote-the-next loop the driver runs.
func TestASlotFreesWhenAFleetGoesDown(t *testing.T) {
	r := New()
	for _, n := range []string{"a", "b", "c", "d"} {
		mustRegister(t, r, Fleet{Name: n})
		if n != "d" {
			if err := r.SetState(n, StateUp); err != nil {
				t.Fatalf("SetState(%s, up): %v", n, err)
			}
		}
	}
	if err := r.SetState("d", StateUp); err == nil {
		t.Fatal("the cap did not hold")
	}
	if err := r.SetState("a", StateDown); err != nil {
		t.Fatalf("SetState(a, down): %v", err)
	}
	if err := r.SetState("d", StateUp); err != nil {
		t.Fatalf("a slot freed but the next fleet was still refused: %v", err)
	}
}

// TestAFleetAlreadyUpCanBeReAffirmed: a driver that re-registers and re-marks
// its own fleet up must not be refused by the cap for a slot it already holds.
func TestAFleetAlreadyUpCanBeReAffirmed(t *testing.T) {
	r := New()
	for _, n := range []string{"a", "b", "c"} {
		mustRegister(t, r, Fleet{Name: n})
		if err := r.SetState(n, StateUp); err != nil {
			t.Fatalf("SetState(%s): %v", n, err)
		}
	}
	if err := r.SetState("b", StateUp); err != nil {
		t.Fatalf("re-affirming a fleet that is already up was refused: %v", err)
	}
}
