// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/fleet"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// The `##fleet` verb (design 028 §6.5, `E-40`).
//
// The workspace holds NO fleet state of its own; every one of these goes over
// the bus to the registry actor. That is what the tests assert by standing up a
// real registry and a real workspace and letting them talk.

func newFleetVerbWorkspace(t *testing.T) *WorkspaceActor {
	t.Helper()
	nc := newABLATestNATS(t)
	codecs := msg.DefaultCodecRegistry()
	startFleetActor(t, nc, newFleetTestKV(t, nc))
	return &WorkspaceActor{
		nc:  nc,
		pub: msg.NewNATSPublisher(nc, codecs),
	}
}

// TestFleetVerbRegistersAndLists — the round trip a human gets.
func TestFleetVerbRegistersAndLists(t *testing.T) {
	w := newFleetVerbWorkspace(t)

	var out strings.Builder
	if err := w.handleFleetCommand(&out, "pA",
		[]string{"register", "epic-07", "--source", "tracks/fleet/epic07.md"}); err != nil {
		t.Fatalf("##fleet register: %v (%s)", err, out.String())
	}
	if !strings.Contains(out.String(), "no panes were opened") {
		// Registering is the CHEAP half and the message has to say so, or an
		// operator reasonably assumes twenty-five registrations just opened a
		// hundred and fifty panes.
		t.Errorf("register did not say it opened nothing: %q", out.String())
	}

	out.Reset()
	if err := w.handleFleetCommand(&out, "pA", []string{"list"}); err != nil {
		t.Fatalf("##fleet list: %v", err)
	}
	if !strings.Contains(out.String(), "epic-07") || !strings.Contains(out.String(), "registered") {
		t.Fatalf("list did not show the fleet and its state: %q", out.String())
	}
}

// TestFleetVerbShowsMembersAndState.
func TestFleetVerbShowsMembersAndState(t *testing.T) {
	w := newFleetVerbWorkspace(t)

	var out strings.Builder
	if err := w.handleFleetCommand(&out, "pA", []string{"register", "epic-07"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := fleet.Send(w.nc, fleet.Update{Op: fleet.OpMemberUpsert, Name: "epic-07",
		Member: fleet.Member{PaneID: "pane-1", Role: "worker", Label: "wkr-07"}},
		fleetQueryTimeout); err != nil {
		t.Fatalf("member upsert: %v", err)
	}

	out.Reset()
	if err := w.handleFleetCommand(&out, "pA", []string{"state", "epic-07", "up"}); err != nil {
		t.Fatalf("state: %v", err)
	}

	out.Reset()
	if err := w.handleFleetCommand(&out, "pA", []string{"show", "epic-07"}); err != nil {
		t.Fatalf("show: %v", err)
	}
	got := out.String()
	for _, want := range []string{"epic-07", "up", "wkr-07", "worker"} {
		if !strings.Contains(got, want) {
			t.Errorf("##fleet show does not mention %q:\n%s", want, got)
		}
	}
}

// TestFleetVerbSaysNoFleetsRatherThanNothing.
//
// An empty registry that ANSWERED must print a sentence, not blank output. A
// command that prints nothing reads as a command that failed, and the operator's
// next move (stand a fleet up, or debug the daemon) depends on which it was.
func TestFleetVerbSaysNoFleetsRatherThanNothing(t *testing.T) {
	w := newFleetVerbWorkspace(t)

	var out strings.Builder
	if err := w.handleFleetCommand(&out, "pA", []string{"list"}); err != nil {
		t.Fatalf("##fleet list: %v", err)
	}
	if !strings.Contains(out.String(), "no fleets registered") {
		t.Fatalf("an empty registry printed %q", out.String())
	}
}

// TestFleetVerbRefusesWhenTheRegistryIsSilent.
//
// THE FAILURE THIS SURFACE MUST NEVER PRODUCE is "no fleets registered" when
// the truth is "nothing answered". They lead to opposite actions — one says
// create a fleet, the other says find out why the daemon is not listening — and
// with no registry running the command must fail loudly rather than print the
// reassuring one.
func TestFleetVerbRefusesWhenTheRegistryIsSilent(t *testing.T) {
	nc := newABLATestNATS(t) // no registry actor
	w := &WorkspaceActor{nc: nc, pub: msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())}

	var out strings.Builder
	err := w.handleFleetCommand(&out, "pA", []string{"list"})
	if err == nil {
		t.Fatal("a silent registry produced a successful list")
	}
	if strings.Contains(out.String(), "no fleets registered") {
		t.Fatalf("a silent registry was reported as an empty one: %q", out.String())
	}
	if w.ryshFail == nil {
		t.Error("the failure was not recorded, so ##fleet would report success " +
			"through the status-aware path")
	}
}

// TestFleetVerbRefusesAnIllegalName — a fleet name is also a board id, and a
// name that cannot be a subject token would address a board nobody subscribes
// to. Caught at registration, because afterwards the symptom is an empty board.
func TestFleetVerbRefusesAnIllegalName(t *testing.T) {
	w := newFleetVerbWorkspace(t)

	var out strings.Builder
	if err := w.handleFleetCommand(&out, "pA", []string{"register", "epic.07"}); err == nil {
		t.Fatalf("a fleet name that is not a legal board id was accepted: %q", out.String())
	}
}

// TestFleetForgetSaysThePanesKeepRunning.
//
// Forgetting is a REGISTRY operation. The live failure that motivated this
// whole actor was a manifest deleted by a sibling while its agents carried on
// working, unnamed — so the message says out loud that this is not a teardown.
func TestFleetForgetSaysThePanesKeepRunning(t *testing.T) {
	w := newFleetVerbWorkspace(t)

	var out strings.Builder
	if err := w.handleFleetCommand(&out, "pA", []string{"register", "epic-07"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	out.Reset()
	if err := w.handleFleetCommand(&out, "pA", []string{"forget", "epic-07"}); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if !strings.Contains(out.String(), "still running") {
		t.Errorf("forget did not say the panes survive it: %q", out.String())
	}

	// And forgetting what is not there is a refusal, not a silent success.
	out.Reset()
	if err := w.handleFleetCommand(&out, "pA", []string{"forget", "epic-07"}); err == nil {
		t.Error("forgetting an absent fleet reported success")
	}
}
