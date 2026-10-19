// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

// Tool-aware activity (design 056 §2).

const psFixture = `
  100     1  2-03:04:05 /bin/zsh -l
  200   100       12:00 claude --dangerously-skip-permissions --session-id x
  210   200       11:59 node /opt/mcp/server.js
  220   200       00:42 /bin/bash -c source /Users/h/.claude/shell-snapshots/snapshot-bash-1.sh 2>/dev/null || true && eval 'go test ./...'
  221   220       00:41 go test ./...
  300     1     1:00:00 claude --session-id y
  310   300       59:00 node /opt/mcp/server.js
  400     1     3:00:00 claude --session-id z
  410   400  1-00:00:00 /bin/bash -c source /Users/h/.claude/shell-snapshots/snapshot-bash-2.sh && eval 'npm run dev'
  500     1       05:00 codex --model gpt-5
  510   500       00:10 /bin/zsh -lc cargo build
`

func TestToolRunningUnderTheAgent(t *testing.T) {
	pt := parseProcTable(psFixture)
	for _, c := range []struct {
		name string
		root int
		want bool
	}{
		{"claude waiting on go test is working", 200, true},
		{"claude with only its MCP server is not", 300, false},
		{"a day-old backgrounded dev server does not count", 400, false},
		{"codex running a shell command is working", 500, true},
		{"an unknown pid", 999, false},
	} {
		if got := pt.toolRunning(c.root); got != c.want {
			t.Errorf("%s: toolRunning(%d) = %v", c.name, c.root, got)
		}
	}
	if d := parseEtime("2-03:04:05"); d != 2*24*time.Hour+3*time.Hour+4*time.Minute+5*time.Second {
		t.Errorf("parseEtime = %v", d)
	}
	var nilTable *procTable
	if nilTable.toolRunning(1) {
		t.Error("a missing process table claimed a running tool")
	}
}

// The real ps: a child of this test whose command line carries Claude Code's
// tool signature is found as a running tool of this process.
func TestToolRunningWithTheRealProcessTable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no ps")
	}
	cmd := exec.Command("/bin/sh", "-c", "sleep 5; : /tmp/.claude/shell-snapshots/snapshot-test.sh")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if readProcTable().toolRunning(os.Getpid()) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("a running tool child of this process was not found in the real process table")
}
