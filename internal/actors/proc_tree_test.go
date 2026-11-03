// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"reflect"
	"sort"
	"testing"
)

// Windows has no process groups or TIOCGPGRP. The pane's "foreground program"
// there is the newest live child of the shell, read from a process snapshot;
// these are the pure halves of that logic.
func TestForegroundChild(t *testing.T) {
	procs := []snapProc{
		{PID: 100, PPID: 4, Created: 10},   // the shell
		{PID: 200, PPID: 100, Created: 20}, // older child
		{PID: 300, PPID: 100, Created: 30}, // newest child: the foreground
		{PID: 310, PPID: 300, Created: 31}, // grandchild: not a candidate
		{PID: 400, PPID: 100, Created: 5},  // created before the shell: a recycled PPID
		{PID: 500, PPID: 7, Created: 40},   // unrelated
	}
	if got := foregroundChild(100, procs); got != 300 {
		t.Errorf("foregroundChild = %d, want 300 (newest direct child)", got)
	}
	if got := foregroundChild(200, procs); got != 200 {
		t.Errorf("no children: got %d, want the shell itself (200)", got)
	}
	if got := foregroundChild(999, procs); got != -1 {
		t.Errorf("shell not in the snapshot: got %d, want -1", got)
	}
}

func TestProcessTree(t *testing.T) {
	procs := []snapProc{
		{PID: 300, PPID: 100, Created: 30},
		{PID: 310, PPID: 300, Created: 31},
		{PID: 320, PPID: 310, Created: 32},
		{PID: 330, PPID: 300, Created: 29}, // older than its "parent": recycled, excluded
		{PID: 400, PPID: 100, Created: 40}, // sibling, not in 300's tree
	}
	got := processTree(300, procs)
	sort.Ints(got)
	if want := []int{300, 310, 320}; !reflect.DeepEqual(got, want) {
		t.Errorf("processTree(300) = %v, want %v", got, want)
	}
	if got := processTree(999, procs); len(got) != 0 {
		t.Errorf("unknown root: %v, want empty", got)
	}
}

func TestExeBaseName(t *testing.T) {
	for in, want := range map[string]string{
		`C:\Users\me\AppData\codex.exe`: "codex",
		"Claude.EXE":                    "claude",
		"node.exe":                      "node",
		"pwsh":                          "pwsh",
		"":                              "",
	} {
		if got := exeBaseName(in); got != want {
			t.Errorf("exeBaseName(%q) = %q, want %q", in, got, want)
		}
	}
}
