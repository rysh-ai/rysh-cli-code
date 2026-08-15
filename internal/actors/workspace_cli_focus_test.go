// SPDX-License-Identifier: Apache-2.0

package actors

// Focus theft on the AGENT DISPATCH path — `rysh exec --pane <id> '##…'`, which
// is what ryshctl, ansa and every fleet tool use to drive another pane.
//
// The human's focus belongs to the human. It moves when they click a pane or
// press a navigation key, and at no other time. A command ARRIVING for a pane
// is not the human asking to go there — with several agents running, each
// dispatch yanks the cursor (and, across tabs, the whole visible tab) to
// whichever agent was addressed last.
//
// Design 025 §4.1 named this hazard and `TestBoardPostDoesNotStealFocus` closed
// it for board posts only, by routing them around the CLI path. The CLI path
// itself still called focusPaneByID on every named pane, so everything else that
// addresses a pane kept stealing focus. These tests pin the general case.

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// A ## command dispatched TO a pane must run in that pane and leave the human's
// focus exactly where it was — including when the target is in another tab,
// which is what makes the theft visible in the desktop app: its Body renders
// whatever tab the daemon calls active.
func TestCLIRyshCommandDoesNotStealFocus(t *testing.T) {
	w, _ := newBoardTestWorkspace(t)

	resp := w.handleCLIRyshCommand(nil, &msg.MsgCLIRyshCommand{
		PaneID:  "pB",
		Command: "##pane info",
	})
	if resp == nil {
		t.Fatal("no response")
	}

	if w.activePaneID != "pA" {
		t.Errorf("activePaneID = %q, want pA — dispatching to a pane moved the human's focus to it; "+
			"focus is the human's, moved by a click or a navigation key and nothing else", w.activePaneID)
	}
	if w.activeTabIdx != 0 {
		t.Errorf("activeTabIdx = %d, want 0 — dispatching to a pane in another tab switched the "+
			"visible tab out from under the human", w.activeTabIdx)
	}
}

// The same for a command addressed to a TAB rather than a pane.
func TestCLIRyshCommandToTabDoesNotStealFocus(t *testing.T) {
	w, _ := newBoardTestWorkspace(t)

	w.handleCLIRyshCommand(nil, &msg.MsgCLIRyshCommand{
		TabID:   "tab-B",
		Command: "##pane info",
	})

	if w.activePaneID != "pA" || w.activeTabIdx != 0 {
		t.Errorf("focus moved to tab-B (activePaneID=%q activeTabIdx=%d) — addressing a tab is not "+
			"the human asking to be taken there", w.activePaneID, w.activeTabIdx)
	}
}

// The routing itself must be unaffected: the command still runs in the NAMED
// pane, not in the focused one. Without this, "don't steal focus" could be
// satisfied by a handler that quietly targets the wrong pane — which is the
// bug this whole area exists to prevent (ambient attribution, design 025 §4.1
// hazard 1).
func TestCLIRyshCommandStillTargetsTheNamedPane(t *testing.T) {
	w, _ := newBoardTestWorkspace(t)

	resp := w.handleCLIRyshCommand(nil, &msg.MsgCLIRyshCommand{
		PaneID:  "pB",
		Command: "##pane info",
	})
	if resp == nil {
		t.Fatal("no response")
	}
	if !resp.OK {
		t.Fatalf("dispatch to pB failed: %+v", resp)
	}
	// `##pane info` reports the pane it ran in, so the output names the target.
	if !strings.Contains(resp.Output, "brave-otter") && !strings.Contains(resp.Output, "pB") {
		t.Errorf("output does not name pB as the target pane:\n%s", resp.Output)
	}
}

// An agent creating a pane must not drag the human to it either.
//
// `##new pane` restores focus after the create — deliberately, so a create does
// not leave the cursor on the new pane. It used to restore to the pane that
// ISSUED the command, which is right for a person typing it and wrong for every
// fleet tool, all of which issue it as `rysh exec --pane <agent> '##new pane'`.
// The restore then walked the human's focus over to the agent.
func TestAgentCreatingAPaneDoesNotStealFocus(t *testing.T) {
	w, _ := newBoardTestWorkspace(t)
	w.usedAliases = map[string]struct{}{}

	w.handleCLIRyshCommand(nil, &msg.MsgCLIRyshCommand{
		PaneID:  "pB",
		Command: "##new pane",
	})

	if w.activePaneID != "pA" {
		t.Errorf("activePaneID = %q, want pA — an agent's create moved the human's focus", w.activePaneID)
	}
	if w.activeTabIdx != 0 {
		t.Errorf("activeTabIdx = %d, want 0 — an agent's create switched the human's tab", w.activeTabIdx)
	}
}

// The human's own create still behaves: focus stays on the pane they typed in,
// not on the pane that was just created. This is the behaviour
// restoreFocusAfterCreate exists for, and it must survive the fix above.
func TestHumanCreatingAPaneKeepsTheirOwnFocus(t *testing.T) {
	w, _ := newBoardTestWorkspace(t)
	w.usedAliases = map[string]struct{}{}

	// Typed in pA, the pane the human is already on.
	_, _ = w.runRyshCommand(nil, "pA", "rysh", "##new pane")

	if w.activePaneID != "pA" {
		t.Errorf("activePaneID = %q, want pA — the creator's own focus was not preserved", w.activePaneID)
	}
	if w.activeTabIdx != 0 {
		t.Errorf("activeTabIdx = %d, want 0", w.activeTabIdx)
	}
}

// The agent-facing rysh TOOL submits input addressed to a pane
// (internal/tools/rysh_command.go). It routes there, and moves nobody's focus:
// an agent running a ## command is not a person typing in that pane.
func TestProgrammaticInputDoesNotStealFocus(t *testing.T) {
	w, _ := newBoardTestWorkspace(t)

	w.handleSubmitInput(nil, &msg.MsgSubmitInput{
		Text:         "echo hello",
		Mode:         "shell",
		PaneID:       "pB",
		Programmatic: true,
	})

	if w.activePaneID != "pA" {
		t.Errorf("activePaneID = %q, want pA — an agent's tool call moved the human's focus", w.activePaneID)
	}
	if w.activeTabIdx != 0 {
		t.Errorf("activeTabIdx = %d, want 0 — an agent's tool call switched the human's tab", w.activeTabIdx)
	}
}

// A person typing into a pane box still aligns focus to it. That is the whole
// point of the PaneID field (a starved focus command otherwise executes their
// Enter in the previously-active pane), and it must survive the fix above.
func TestTypedInputStillAlignsFocus(t *testing.T) {
	w, _ := newBoardTestWorkspace(t)

	w.handleSubmitInput(nil, &msg.MsgSubmitInput{
		Text:   "echo hello",
		Mode:   "shell",
		PaneID: "pB",
	})

	if w.activePaneID != "pB" {
		t.Errorf("activePaneID = %q, want pB — typing into a pane's box must focus it", w.activePaneID)
	}
}
