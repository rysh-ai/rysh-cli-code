// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package actors

import (
	"os/exec"
	"testing"
	"time"

	"github.com/creack/pty"
)

// TestTTYLastInputReadsControllingTerminal runs a process on a fresh pseudo-
// terminal (its controlling terminal, as an attached terminal UI's is) and
// checks the daemon can read that terminal's input clock. The process has had
// no input, so the time is the terminal's creation, give or take the clock's
// granularity.
func TestTTYLastInputReadsControllingTerminal(t *testing.T) {
	before := time.Now()
	cmd := exec.Command("sleep", "30")
	ptmx, err := pty.Start(cmd)
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = ptmx.Close()
	})

	var at time.Time
	var ok bool
	for range 50 { // the child needs a moment to take the terminal
		if at, ok = ttyLastInput(cmd.Process.Pid); ok {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ok {
		t.Fatal("ttyLastInput found no terminal for a process on a pty")
	}
	if at.Before(before.Add(-time.Minute)) || at.After(time.Now().Add(time.Minute)) {
		t.Errorf("input time %v is not near the terminal's creation (%v)", at, before)
	}
}

// TestTTYLastInputNoTerminal: a process without a terminal reports nothing,
// which the arbitration reads as "in use".
func TestTTYLastInputNoTerminal(t *testing.T) {
	if _, ok := ttyLastInput(0); ok {
		t.Error("pid 0 reported a terminal")
	}
	if _, ok := ttyLastInput(2147483646); ok {
		t.Error("a dead pid reported a terminal")
	}
}
