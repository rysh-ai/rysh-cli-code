// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

// TestParseCmdArgsBooleanCapture is the parser bug this flag could have
// introduced: every other ##cmd flag takes a value, so a value-taking
// --capture would have eaten the command and left the broadcast with nothing
// to run.
func TestParseCmdArgsBooleanCapture(t *testing.T) {
	scope, sel, command, err := parseCmdArgs([]string{"stack", "--capture", "git", "status"})
	if err != nil {
		t.Fatalf("parseCmdArgs: %v", err)
	}
	if scope != "panegroup" || !sel.capture || command != "git status" {
		t.Errorf("scope=%q capture=%v command=%q; want panegroup/true/\"git status\"",
			scope, sel.capture, command)
	}
}

func TestParseCmdArgsRunningFilter(t *testing.T) {
	_, sel, command, err := parseCmdArgs([]string{"ws", "--running", "claude", "--capture", "pwd"})
	if err != nil {
		t.Fatalf("parseCmdArgs: %v", err)
	}
	if sel.running != "claude" || !sel.capture || command != "pwd" {
		t.Errorf("running=%q capture=%v command=%q", sel.running, sel.capture, command)
	}
}

// TestFilterPanesByProgram checks the filter that decides where a broadcast
// actually lands. Getting "shell" wrong would type shell commands into whatever
// full-screen program happens to be running.
func TestFilterPanesByProgram(t *testing.T) {
	snap := &domain.WorkspaceSnapshot{Tabs: []domain.TabSnapshot{{
		Lanes: []domain.LaneSnapshot{{PaneGroups: []domain.PaneGroupSnapshot{{Panes: []domain.PaneSnapshot{
			{ID: "a", Program: "claude"},
			{ID: "b"},
			{ID: "c", Program: "vim"},
		}}}}},
	}}}
	all := []string{"a", "b", "c"}

	if got := filterPanesByProgram(snap, all, ""); len(got) != 3 {
		t.Errorf("no filter dropped panes: %v", got)
	}
	if got := filterPanesByProgram(snap, all, "claude"); len(got) != 1 || got[0] != "a" {
		t.Errorf("--running claude = %v, want [a]", got)
	}
	// "shell" is the pane at its prompt, which reports an EMPTY program — the
	// spelling users will reach for, and the one case a naive equality check
	// gets backwards.
	if got := filterPanesByProgram(snap, all, "shell"); len(got) != 1 || got[0] != "b" {
		t.Errorf("--running shell = %v, want [b]", got)
	}
	if got := filterPanesByProgram(snap, all, "emacs"); len(got) != 0 {
		t.Errorf("unmatched filter = %v, want nothing", got)
	}
}

// TestCaptureCommand pins the contract a reader depends on: output in a known
// file, and a final line that distinguishes "finished with no output" from
// "still running".
func TestCaptureCommand(t *testing.T) {
	got := captureCommand("/ws", "pane-1", "git status")
	want := "/ws/.rysh/captures/pane-1.out"
	if !strings.Contains(got, want) {
		t.Errorf("capture path %q missing from %q", want, got)
	}
	if !strings.Contains(got, captureSentinel+"$?") {
		t.Errorf("sentinel does not carry the exit status: %q", got)
	}
	// Braces, not a subshell: `cd` and exports inside a captured command must
	// affect the pane's shell exactly as they would without capture.
	if !strings.Contains(got, "{ git status ; }") {
		t.Errorf("command not run in a brace group: %q", got)
	}
}
