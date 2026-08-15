// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package actors

import (
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// Strict mode's block (design 022 §8.2) is only as real as the signal it sends.
// This exercises the mechanism against a genuine process group rather than
// asserting that a function was called.

func TestTerminateProcessGroup_StopsTheGroupNotJustTheLeader(t *testing.T) {
	// A shell that spawns a child and waits: the leader plus one descendant, the
	// shape an agent CLI has (a runtime with workers under it).
	cmd := exec.Command("/bin/sh", "-c", "sleep 30 & sleep 30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pgid := processPgid(cmd.Process.Pid)
	t.Cleanup(func() {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		_ = cmd.Wait()
	})

	// Give the shell a moment to fork its child, so the assertion is about a
	// group and not a single process.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !processGroupAlive(pgid) {
		time.Sleep(10 * time.Millisecond)
	}
	if !processGroupAlive(pgid) {
		t.Fatal("the test process group never came up")
	}

	if err := terminateProcessGroup(pgid); err != nil {
		t.Fatalf("terminateProcessGroup: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the leader survived SIGTERM to its process group")
	}

	// And the whole group is gone, not only the leader — a surviving child is a
	// process still holding a provider connection.
	gone := false
	for i := 0; i < 200; i++ {
		if !processGroupAlive(pgid) {
			gone = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !gone {
		t.Fatal("a child of the ungoverned CLI survived — signalling the leader " +
			"alone leaves processes on the wire")
	}
}

// TestTerminateProcessGroup_RefusesDangerousTargets: pgid 0 means "my own
// group" and 1 is init. Either would turn a governance rule into an outage.
func TestTerminateProcessGroup_RefusesDangerousTargets(t *testing.T) {
	for _, pgid := range []int{0, 1, -1} {
		if err := terminateProcessGroup(pgid); err == nil {
			t.Errorf("terminateProcessGroup(%d) returned nil — it must refuse", pgid)
		}
	}
}
