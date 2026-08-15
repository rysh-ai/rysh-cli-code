// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/fleet"
)

// `rysh fleet` rendering (design 028 §6.5, `E-40`).
//
// Pure tests on the WORDING, because the wording is the safety argument on this
// surface — the same reason board_cmd.go's no-recorder message is tested
// without a daemon.

// TestAnEmptyRegistryReadsDifferentlyFromASilentOne.
//
// Two facts, two sentences, and they must never converge: a registry that
// answered with nothing tells the operator to stand a fleet up, while one that
// did not answer tells them the daemon is not listening. The no-answer path
// does not go through this renderer at all — it exits non-zero with nothing on
// stdout — which is what this test pins from the other side.
func TestAnEmptyRegistryReadsDifferentlyFromASilentOne(t *testing.T) {
	empty := renderFleetList(&fleet.Reply{Fleets: []fleet.Fleet{}}, "")
	if !strings.Contains(empty, "no fleets registered") {
		t.Fatalf("an empty registry rendered as %q", empty)
	}

	silent := fleetNoRegistryError(fleet.ErrNoRegistry).Error()
	if !strings.Contains(silent, "NOT AN EMPTY SESSION") {
		t.Errorf("the no-registry error does not distinguish itself: %q", silent)
	}
	if strings.Contains(silent, "no fleets registered") {
		t.Error("the no-registry error borrows the empty-registry wording; a reader " +
			"cannot then tell which happened")
	}
}

// TestANamedFleetThatIsAbsentSaysSo — `rysh fleet show nope` must not print the
// session-wide "no fleets registered", which would be false whenever other
// fleets exist.
func TestANamedFleetThatIsAbsentSaysSo(t *testing.T) {
	got := renderFleetList(&fleet.Reply{Fleets: []fleet.Fleet{}}, "epic-07")
	if !strings.Contains(got, `no fleet named "epic-07"`) {
		t.Fatalf("rendered %q", got)
	}
	if strings.Contains(got, "no fleets registered") {
		t.Error("a missing named fleet was reported as an empty session")
	}
}

// TestShowListsMembersAndListDoesNot: `ls` is a session overview and `show` is
// one fleet, so the roster belongs to exactly one of them.
func TestShowListsMembersAndListDoesNot(t *testing.T) {
	reply := &fleet.Reply{Fleets: []fleet.Fleet{{
		Name: "epic-07", State: fleet.StateUp, BoardID: "epic-07",
		Members: []fleet.Member{{PaneID: "pane-1", Role: "worker", Label: "wkr-07"}},
	}}, ReconciledAt: 1}

	if got := renderFleetList(reply, "epic-07"); !strings.Contains(got, "wkr-07") {
		t.Errorf("show did not list members: %q", got)
	}
	if got := renderFleetList(reply, ""); strings.Contains(got, "wkr-07") {
		t.Errorf("ls listed a member roster: %q", got)
	}
}

// TestAnUncheckedRosterSaysSo. Membership is reconciled on the registry's own
// clock, so a roster can be older than the answer. A reader who cannot tell is
// one who will eventually act on a member that closed ten minutes ago.
func TestAnUncheckedRosterSaysSo(t *testing.T) {
	never := renderFleetList(&fleet.Reply{
		Fleets: []fleet.Fleet{{Name: "epic-07", State: fleet.StateRegistered}},
	}, "")
	if !strings.Contains(never, "not been checked") {
		t.Fatalf("an unreconciled answer did not disclose it: %q", never)
	}

	checked := renderFleetList(&fleet.Reply{
		Fleets:       []fleet.Fleet{{Name: "epic-07", State: fleet.StateRegistered}},
		ReconciledAt: 1,
	}, "")
	if strings.Contains(checked, "not been checked") {
		t.Errorf("a reconciled answer carried the warning anyway: %q", checked)
	}
}

// TestTheFleetVerbIsDocumented — the E-44 lesson, applied to my own verb.
//
// `##tab delete` worked for months and was missing from `##tab`'s help, and that
// absence alone cost two deferrals of tab-per-fleet: every reading of the live
// surface concluded the verb did not exist. `rysh fleet` shipped with exactly
// the same gap — the switch case was added, the usage block was not — and it
// was caught by running `rysh help | grep fleet` before the first real fleet
// run, not by any test.
//
// A verb nobody can find is a verb nobody has.
func TestTheFleetVerbIsDocumented(t *testing.T) {
	// SOURCE-READ, the idiom this repo already uses for properties of CALL SITES
	// (TestEveryInterruptReceiptStatesTheQueueLimit, TestEveryBusNewPassesA
	// SessionName): usageLine prints straight to stdout, and what matters is
	// that the lines are IN the usage block at all.
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	out := string(src)

	for _, want := range []string{"rysh fleet ls", "rysh fleet register", "rysh fleet member"} {
		if !strings.Contains(out, want) {
			t.Errorf("`%s` is missing from rysh help", want)
		}
	}
	// The read path's board flag, added by the same design and equally invisible
	// without a line here.
	if !strings.Contains(out, "rysh board tail --board") {
		t.Error("`rysh board tail --board <id>` is missing from rysh help")
	}
}
