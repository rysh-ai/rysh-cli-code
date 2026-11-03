// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"strings"
	"testing"
)

// Design 030 §9.6 Q1 (D-25), founder 2026-11-01: `fleet` (the session-wide
// fleet board) and `session` (the session board) are reserved. Wave W1.
//
// Today a fleet named `fleet` or `session` is accepted and silently shares
// that board with every other poster, and so is a fleet whose board id
// normalises to one of them. Both must be refused at registration, the only
// moment the mistake can be caught.

func TestW1AReservedNameCannotBeAFleet(t *testing.T) {
	r := New()
	for _, name := range []string{"fleet", "FLEET", " Fleet ", "session", "Session"} {
		_, err := r.Register(Fleet{Name: name})
		if err == nil {
			t.Errorf("Register(%q) was accepted; the name is reserved", name)
			continue
		}
		if !strings.Contains(err.Error(), "reserved") {
			t.Errorf("Register(%q): %v; the refusal must say the name is reserved", name, err)
		}
	}
	if len(r.List()) != 0 {
		t.Fatalf("a refused fleet was registered: %+v", r.List())
	}
}

func TestW1AFleetCannotUseAReservedBoard(t *testing.T) {
	r := New()
	for _, board := range []string{"fleet", "FLEET", "session"} {
		if _, err := r.Register(Fleet{Name: "epic-07", BoardID: board}); err == nil ||
			!strings.Contains(err.Error(), "reserved") {
			t.Errorf("Register(epic-07, board %q) = %v; want a refusal naming the reserved board", board, err)
		}
	}
}

// Names that only look like the reserved ones stay legal.
func TestW1NearbyNamesStayLegal(t *testing.T) {
	r := New()
	for _, name := range []string{"fleets", "fleet-a", "my-fleet", "sessions", "root"} {
		mustRegister(t, r, Fleet{Name: name})
	}
}

// The refusal reaches whoever asked over the bus (`rysh fleet register`,
// fleetctl), not only the in-process caller.
func TestW1TheWireRefusesAReservedName(t *testing.T) {
	r := New()
	reply := apply(r, Update{Op: OpRegister, Fleet: Fleet{Name: "fleet"}})
	if reply.OK || !strings.Contains(reply.Err, "reserved") {
		t.Fatalf("apply(register fleet) = %+v; want a refusal naming the reserved name", reply)
	}
}
