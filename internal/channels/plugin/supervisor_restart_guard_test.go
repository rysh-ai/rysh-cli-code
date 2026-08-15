// SPDX-License-Identifier: Apache-2.0

package plugin

import "testing"

// The restart-loop guard.
//
// TestSupervisorCircuitBreak was flaky (~1 run in 4) with "failures = 4, want
// 3". The cause was not the assertion: `spawn` registers the process exit
// watcher BEFORE the ready handshake, so a handshake that fails returns an
// error to the restart loop that is already running while the watcher for that
// same dead process fires and opens a SECOND loop. Both then spent the same
// `failures` budget, so the circuit tripped at an arbitrary count above
// MaxFailures.
//
// These tests pin the invariant directly, without timing: at most one restart
// loop may hold the budget, and a tripped circuit stays tripped until an
// explicit Start.

func TestBeginRestart_OnlyOneLoopAtATime(t *testing.T) {
	s := &PluginSupervisor{}

	if !s.beginRestart() {
		t.Fatal("the first caller must claim the restart slot")
	}
	if s.beginRestart() {
		t.Fatal("a second concurrent caller must be refused — two loops spend one budget")
	}

	s.endRestart()

	if !s.beginRestart() {
		t.Fatal("the slot must be reusable once the previous loop finishes")
	}
}

func TestBeginRestart_RefusedWhenCircuitIsOpen(t *testing.T) {
	s := &PluginSupervisor{broken: true}

	if s.beginRestart() {
		t.Fatal("a tripped circuit must not be reopened by a late process exit")
	}
}

func TestBeginRestart_RefusedWhileStopping(t *testing.T) {
	s := &PluginSupervisor{stopping: true}

	if s.beginRestart() {
		t.Fatal("a supervisor being stopped must not start a restart loop")
	}
}

// TestEndRestart_DoesNotClearTheBreak keeps the two flags independent: the loop
// finishing must not re-arm a circuit that tripped.
func TestEndRestart_DoesNotClearTheBreak(t *testing.T) {
	s := &PluginSupervisor{}
	if !s.beginRestart() {
		t.Fatal("expected to claim the slot")
	}
	s.broken = true // circuit-break inside the loop
	s.endRestart()

	if s.beginRestart() {
		t.Fatal("releasing the slot must not clear the circuit-break latch")
	}
}
