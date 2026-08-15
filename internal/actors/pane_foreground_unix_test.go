// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package actors

import (
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

// TestProcessGroupAlive verifies the signal the interactive-lifetime detection
// relies on: a process group reports alive while a member runs and dead once
// it exits. This is what keeps the pane interactive for an inline TUI's whole
// lifetime even when it momentarily isn't the PTY foreground (e.g. codex
// switching from its splash to its inline UI).
func TestProcessGroupAlive(t *testing.T) {
	cmd := exec.Command("sleep", "1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // sleep leads its own group
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start sleep: %v", err)
	}
	pgid := processPgid(cmd.Process.Pid)
	if pgid <= 1 {
		t.Fatalf("processPgid = %d", pgid)
	}
	if !processGroupAlive(pgid) {
		t.Fatalf("group %d should be alive while the process runs", pgid)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && processGroupAlive(pgid) {
		time.Sleep(20 * time.Millisecond)
	}
	if processGroupAlive(pgid) {
		t.Fatalf("group %d should be dead after the process exited", pgid)
	}
}

// TestForegroundPgrpTracksRunningChild verifies the primitive the interactive
// detection relies on: the PTY master's foreground process group equals the
// shell's pgid at the prompt and changes while a foreground command runs. This
// is what lets the pane stay interactive while an inline TUI (e.g. codex) is
// still running even after it stops tripping the alt-screen/cursor heuristic.
func TestForegroundPgrpTracksRunningChild(t *testing.T) {
	cmd := exec.Command("/bin/bash", "--norc", "--noprofile", "-i")
	f, err := pty.Start(cmd)
	if err != nil {
		t.Skipf("pty.Start unavailable: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = f.Close() }()

	// Drain PTY output so the shell never blocks on a full buffer.
	go func() {
		b := make([]byte, 4096)
		for {
			if _, e := f.Read(b); e != nil {
				return
			}
		}
	}()

	shellPgid := processPgid(cmd.Process.Pid)
	if shellPgid <= 0 {
		t.Fatalf("processPgid(%d) = %d", cmd.Process.Pid, shellPgid)
	}

	waitFor := func(name string, want bool, pred func() bool) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if pred() == want {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s (fg=%d shellPgid=%d)", name, foregroundPgrp(f), shellPgid)
	}
	atShellPrompt := func() bool {
		fg := foregroundPgrp(f)
		return fg > 0 && fg == shellPgid
	}
	childRunning := func() bool {
		fg := foregroundPgrp(f)
		return fg > 0 && fg != shellPgid
	}

	// 1. At the shell prompt, the foreground pgrp is the shell itself.
	waitFor("shell prompt", true, atShellPrompt)

	// 2. While a foreground command runs, the foreground pgrp differs.
	if _, err := f.Write([]byte("sleep 1\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	waitFor("child running", true, childRunning)

	// 3. After the command exits, the foreground returns to the shell.
	waitFor("back at prompt", true, atShellPrompt)
}
