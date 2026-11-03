// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package hooks

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A timed-out hook's children die with it: the hook runs in its own process
// group and the whole group is killed, not just bash (lead review (4)).
func TestRun_TimeoutKillsTheWholeProcessGroup(t *testing.T) {
	needBash(t)
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	dir := writeConfig(t, `
hooks:
  pre-tool:
    - run: 'sleep 60 & echo $! > `+pidFile+`; wait'
      timeout: 1s
`)
	start := time.Now()
	blocked, _ := Load(dir).Gate(context.Background(), PreTool, Payload{Tool: "Bash"}, Env{Dir: dir})
	if !blocked {
		t.Fatal("a timed-out pre-tool hook must block")
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		t.Fatalf("child pid %q", b)
	}
	// Only this test's own child is ever signalled here.
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("the hook's child %d outlived the timeout", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Errorf("Gate took %s after a 1s timeout", d)
	}
}
