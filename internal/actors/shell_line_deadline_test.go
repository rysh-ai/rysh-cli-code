// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// runShellLine runs `sh -c line` with env in its own process group and waits
// at most timeout; past it the whole group is killed. Reports whether it timed
// out. The exit status is the caller's business.
func runShellLine(shPath, line string, env []string, timeout time.Duration) (timedOut bool) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, shPath, "-c", line)
	cmd.Env = env
	startInOwnGroup(cmd)
	cmd.WaitDelay = 2 * time.Second
	_ = cmd.Run()
	return ctx.Err() != nil
}

// QA-31: a shell line run by a test has a deadline, and when it passes the
// whole process group goes, children included, so a host stall fails the
// test instead of hanging it and leaving orphans behind.
func TestRunShellLineKillsTheGroupOnTimeout(t *testing.T) {
	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	start := time.Now()
	timedOut := runShellLine(shPath, "sleep 30 & echo $! > "+shellQuote(pidFile)+"; wait", os.Environ(), time.Second)
	if !timedOut {
		t.Fatal("a line that sleeps 30 s did not time out after 1 s")
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("runShellLine returned after %v; the deadline did not bound it", d)
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("the background child never started: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && processAlive(pid) {
		time.Sleep(50 * time.Millisecond)
	}
	if processAlive(pid) {
		t.Fatalf("the backgrounded child %d outlived the timeout: the group was not killed", pid)
	}
	if runShellLine(shPath, "exit 3", os.Environ(), 5*time.Second) {
		t.Fatal("a line that exits at once was reported as timed out")
	}
}
