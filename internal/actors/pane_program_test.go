// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"strings"
	"testing"
)

// TestForegroundEvents covers the transition a supervisor is actually waiting
// on. The case that matters is the middle one: a program replacing another
// without the shell ever coming back. Reporting only the start there would
// leave anything waiting for "claude exited" waiting forever.
func TestForegroundEvents(t *testing.T) {
	cases := []struct {
		name       string
		prev, next string
		want       []processEvent
	}{
		{"shell to program", "", "claude", []processEvent{{"start", "claude"}}},
		{"program to shell", "claude", "", []processEvent{{"exit", "claude"}}},
		{"program replaced without returning to the shell", "git", "less",
			[]processEvent{{"exit", "git"}, {"start", "less"}}},
		{"no change is not an event", "claude", "claude", nil},
		{"shell to shell is not an event", "", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := foregroundEvents(tc.prev, tc.next)
			if len(got) != len(tc.want) {
				t.Fatalf("foregroundEvents(%q,%q) = %v, want %v", tc.prev, tc.next, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("event %d = %v, want %v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestClaudeLaunchCommand pins the three properties that make `##pane new
// --claude` different from typing "claude" into a pane.
func TestClaudeLaunchCommand(t *testing.T) {
	cmd := claudeLaunchCommand("11111111-2222-3333-4444-555555555555", "/tmp/p.txt", "")

	// 1. The session id is pinned, so the caller knows it before claude runs
	//    rather than after it exits.
	if !strings.Contains(cmd, "--session-id 11111111-2222-3333-4444-555555555555") {
		t.Errorf("session id not pinned: %s", cmd)
	}
	// 2. The prompt arrives as argv read from a file — never typed, and never
	//    inlined through the ## tokeniser that would mangle its whitespace.
	if !strings.Contains(cmd, `"$(cat /tmp/p.txt)"`) {
		t.Errorf("prompt is not passed as argv from the file: %s", cmd)
	}
	// 3. The parent's markers are unset, so the child is a top-level session
	//    rather than a nested one.
	for _, v := range []string{"CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "CLAUDE_PID"} {
		if !strings.Contains(cmd, "-u "+v) {
			t.Errorf("%s not unset: %s", v, cmd)
		}
	}

	// Without a prompt there must be no trailing argv at all: an empty "" would
	// be an empty first message, not "no message".
	if bare := claudeLaunchCommand("id", "", ""); strings.Contains(bare, `"$(cat`) || strings.HasSuffix(bare, `""`) {
		t.Errorf("no-prompt launch should end at the flags: %s", bare)
	}
}

func TestStripClaudeFlag(t *testing.T) {
	rest, ok := stripClaudeFlag([]string{"--claude", "do", "the", "thing"})
	if !ok || strings.Join(rest, " ") != "do the thing" {
		t.Errorf("stripClaudeFlag = %v, %v; want the prompt words and true", rest, ok)
	}
	if rest, ok := stripClaudeFlag([]string{"--worktree"}); ok || len(rest) != 1 {
		t.Errorf("stripClaudeFlag on an unrelated flag = %v, %v; want it untouched", rest, ok)
	}
}

func TestSplitPaneTarget(t *testing.T) {
	rest, pane := splitPaneTarget([]string{"set", "--pane", "other", "k", "v"}, "mine")
	if pane != "other" || strings.Join(rest, " ") != "set k v" {
		t.Errorf("splitPaneTarget = %v, %q; want [set k v] and \"other\"", rest, pane)
	}
	rest, pane = splitPaneTarget([]string{"--pane=other", "list"}, "mine")
	if pane != "other" || strings.Join(rest, " ") != "list" {
		t.Errorf("--pane= form = %v, %q", rest, pane)
	}
	// No flag: the caller's own pane, which is what makes `##pane meta list`
	// mean "this pane" without anyone having to name it.
	if _, pane := splitPaneTarget([]string{"list"}, "mine"); pane != "mine" {
		t.Errorf("default target = %q, want the caller's pane", pane)
	}
}
