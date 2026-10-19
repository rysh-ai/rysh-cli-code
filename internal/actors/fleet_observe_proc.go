// SPDX-License-Identifier: Apache-2.0

package actors

// Tool-aware activity (design 056 §2): an agent waiting on a script it ran is
// WORKING, however still its screen is.
//
// The screen signals (footer marker, two-frame diff) miss the commonest busy
// state in a fleet: claude runs `go test ./...` or a build and waits. On a
// narrow pane the footer marker is clipped, the tool's output is buffered, and
// two frames 1.2 s apart are identical — so the agent was observed idle,
// judged stalled after 5 minutes, and checked in with every interval: a full
// turn on the agent's (expensive) model to ask a busy agent if it is busy.
//
// The process tree does not have that blind spot. Claude Code runs every Bash
// tool command as a child shell that sources its own shell snapshot
// (`…/.claude/shell-snapshots/snapshot-…`); codex runs its commands as
// `<shell> -lc <cmd>`. A live one under the pane's foreground program means a
// tool is running, which is what "waiting on a script" is.
//
// One `ps` per observation sweep, for every pane at once.

import (
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// afaToolMaxAge bounds how long a running tool counts as work. A backgrounded
// dev server or watcher is a tool process too, and one that ran forever would
// hide a real stall forever; no build or test an agent waits on runs for two
// hours.
var afaToolMaxAge = 2 * time.Hour

// procEntry is one row of the process table.
type procEntry struct {
	PID, PPID int
	Elapsed   time.Duration
	Args      string
}

// procTable is the process table, children indexed by parent.
type procTable struct {
	byPID    map[int]procEntry
	children map[int][]int
}

// readProcTable runs ps once. nil on any failure (Windows, a sandbox without
// ps): the caller falls back to the screen signals alone, exactly as before.
func readProcTable() *procTable {
	out, err := exec.Command("ps", "-Ao", "pid=,ppid=,etime=,args=").Output()
	if err != nil {
		return nil
	}
	return parseProcTable(string(out))
}

// parseProcTable parses `ps -Ao pid=,ppid=,etime=,args=` output.
func parseProcTable(out string) *procTable {
	t := &procTable{byPID: map[int]procEntry{}, children: map[int][]int{}}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		e := procEntry{PID: pid, PPID: ppid, Elapsed: parseEtime(f[2]), Args: strings.Join(f[3:], " ")}
		t.byPID[pid] = e
		t.children[ppid] = append(t.children[ppid], pid)
	}
	return t
}

// parseEtime parses ps's elapsed time: [[dd-]hh:]mm:ss.
func parseEtime(s string) time.Duration {
	days := 0
	if i := strings.IndexByte(s, '-'); i >= 0 {
		days, _ = strconv.Atoi(s[:i])
		s = s[i+1:]
	}
	parts := strings.Split(s, ":")
	secs := 0
	for _, p := range parts {
		n, _ := strconv.Atoi(p)
		secs = secs*60 + n
	}
	return time.Duration(days)*24*time.Hour + time.Duration(secs)*time.Second
}

// isToolProcess reports whether a process is an agent's tool command.
func isToolProcess(args string) bool {
	if strings.Contains(args, "/.claude/shell-snapshots/snapshot-") {
		return true // Claude Code's Bash tool (foreground or backgrounded)
	}
	// codex: `<shell> -lc <command>`.
	f := strings.Fields(args)
	if len(f) >= 3 && f[1] == "-lc" {
		base := f[0][strings.LastIndexByte(f[0], '/')+1:]
		switch base {
		case "bash", "zsh", "sh", "fish":
			return true
		}
	}
	return false
}

// toolRunning reports whether any live descendant of root is a tool command
// younger than afaToolMaxAge.
func (t *procTable) toolRunning(root int) bool {
	if t == nil || root <= 0 {
		return false
	}
	seen := map[int]bool{root: true}
	stack := append([]int(nil), t.children[root]...)
	for len(stack) > 0 {
		pid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		if e, ok := t.byPID[pid]; ok && isToolProcess(e.Args) && e.Elapsed < afaToolMaxAge {
			return true
		}
		stack = append(stack, t.children[pid]...)
	}
	return false
}
