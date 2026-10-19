// SPDX-License-Identifier: Apache-2.0

//go:build linux

package session

import (
	"os"
	"runtime/debug"
	"testing"
)

// ProcessAlive runs every couple of seconds per multi-viewport pane in the
// daemon. Since Go 1.23, os.FindProcess holds a pidfd that only Release or a
// GC finalizer closes — so without Release the pidfds piled up between GCs
// (a 45-day daemon held 11 open on one attached TUI's pid).
func TestProcessAliveDoesNotHoldPidfds(t *testing.T) {
	defer debug.SetGCPercent(debug.SetGCPercent(-1)) // no finalizer may hide a leak

	countFDs := func() int {
		ents, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Skipf("no /proc/self/fd: %v", err)
		}
		return len(ents)
	}
	before := countFDs()
	for i := 0; i < 200; i++ {
		if !ProcessAlive(os.Getpid()) {
			t.Fatal("ProcessAlive(self) = false")
		}
	}
	if grew := countFDs() - before; grew > 5 {
		t.Fatalf("200 ProcessAlive calls left %d more fds open — each call leaks a pidfd", grew)
	}
}
