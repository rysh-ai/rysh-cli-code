// SPDX-License-Identifier: Apache-2.0

package actors

import "strings"

// snapProc is one row of a process snapshot. Windows has no process groups
// and no TIOCGPGRP, so the pane's foreground program and the tree a hard stop
// must reach are read from a snapshot instead (pane_foreground_windows.go);
// these helpers are the pure part of that, tested on any OS.
//
// Created orders processes in time. Windows recycles pids, and a snapshot's
// parent pid is only a number recorded at creation, so a "child" created before
// its "parent" is a stranger whose parent died and whose pid was reused. Both
// helpers drop such rows; a process whose creation time could not be read
// should carry a negative Created so it is dropped too.
type snapProc struct {
	PID, PPID int
	Created   int64
}

// foregroundChild is the pane's foreground program on Windows: the newest live
// direct child of the shell, or the shell itself while it is at its prompt.
// It returns -1 when the shell is not in the snapshot (it has exited).
func foregroundChild(shellPID int, procs []snapProc) int {
	var shell *snapProc
	for i := range procs {
		if procs[i].PID == shellPID {
			shell = &procs[i]
			break
		}
	}
	if shell == nil {
		return -1
	}
	best := -1
	var bestCreated int64
	for _, e := range procs {
		if e.PPID != shellPID || e.PID == shellPID || e.Created < shell.Created {
			continue
		}
		if best < 0 || e.Created > bestCreated || (e.Created == bestCreated && e.PID > best) {
			best, bestCreated = e.PID, e.Created
		}
	}
	if best < 0 {
		return shellPID
	}
	return best
}

// processTree is root and every live descendant of it: what a stop on Windows
// must end, since there is no process group to signal. Empty when root is not
// in the snapshot.
func processTree(root int, procs []snapProc) []int {
	byPID := make(map[int]snapProc, len(procs))
	children := make(map[int][]snapProc)
	for _, e := range procs {
		byPID[e.PID] = e
		if e.PID != e.PPID {
			children[e.PPID] = append(children[e.PPID], e)
		}
	}
	r, ok := byPID[root]
	if !ok {
		return nil
	}
	out := []int{root}
	seen := map[int]bool{root: true}
	queue := []snapProc{r}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		for _, c := range children[parent.PID] {
			if seen[c.PID] || c.Created < parent.Created {
				continue
			}
			seen[c.PID] = true
			out = append(out, c.PID)
			queue = append(queue, c)
		}
	}
	return out
}

// exeBaseName is a Windows image path's program name as strict mode and
// `##pane list` know it: the last path element, lower-cased, without ".exe".
func exeBaseName(path string) string {
	base := path[strings.LastIndexAny(path, `\/`)+1:]
	return strings.TrimSuffix(strings.ToLower(base), ".exe")
}
