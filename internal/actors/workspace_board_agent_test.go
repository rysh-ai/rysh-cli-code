// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

// The board claude's lifecycle and addressing (design 027 §5.5).

func TestResolveBoardAgentRefusesTwoPanesCalledBoard(t *testing.T) {
	// Given-names are unique per LANE, not per session, so two panes may
	// legally be called "board". Picking one would hide or message the wrong
	// agent, and the sender would never know.
	panes := []domain.PaneSnapshot{
		{ID: "aaaaaaaa-1111", GivenName: boardAgentName},
		{ID: "bbbbbbbb-2222", GivenName: boardAgentName},
	}

	err := resolveBoardAgentFromErr(panes)
	if err == nil {
		t.Fatal("two panes named board resolved to one — refusing is always correct, guessing never is")
	}
	if !strings.Contains(err.Error(), "aaaaaaaa") || !strings.Contains(err.Error(), "bbbbbbbb") {
		t.Fatalf("the refusal must name the candidates so the human can fix it, got: %v", err)
	}
}

func TestResolveBoardAgentReportsWhenItIsNotRunning(t *testing.T) {
	err := resolveBoardAgentFromErr(nil)
	if err == nil {
		t.Fatal("no board claude must be an error, not an empty success")
	}
	if !strings.Contains(err.Error(), "not running") {
		t.Fatalf("the error must say the board claude is absent, got: %v", err)
	}
}

func TestASingleBoardAgentResolves(t *testing.T) {
	panes := []domain.PaneSnapshot{{ID: "aaaaaaaa-1111", GivenName: boardAgentName}}
	if err := resolveBoardAgentFromErr(panes); err != nil {
		t.Fatalf("one board claude must resolve cleanly, got: %v", err)
	}
}

// The brief is what makes the board claude a mind rather than a relay. These
// are the instructions the founder's rulings turn into behaviour, and a brief
// that loses them ships an agent that forwards everything.
func TestTheBriefCarriesTheRulings(t *testing.T) {
	// CONTRACT CHANGED 2026-08-11 — the founder reversed 027 ruling 3 for the
	// stop verb. This list used to require "INTERRUPT ... never a kill"; it
	// now requires the kill, because ESC cannot cancel a pending
	// task-notification and an interrupted agent with a background task wakes
	// itself (F-41). A dead process cannot be woken, and the sessions stay
	// resumable by pinned id — the brief must carry BOTH halves, the stop and
	// the way back.
	required := map[string]string{
		"REFUSE":           "it must know it is allowed to refuse",
		"KILL":             "stop-all-fleet is now a verified kill (founder ruling 2026-08-11)",
		"##ansa kill":      "it must know the verb that stops the fleet",
		"resume":           "a kill without the way back is a loss, not a stop",
		"PANE ID":          "addressing is by id, never by name",
		"untouched":        "non-fleet panes are not signalled",
		"rysh board tail":  "it must know how to read what it is meant to accumulate",
		"##ansa interrupt": "the gentle pause still exists and still means ESC",
		"board post":       "its judgement has to be visible or a refusal looks like a drop",
	}
	for needle, why := range required {
		if !strings.Contains(boardAgentBrief, needle) {
			t.Errorf("the board claude's brief is missing %q — %s", needle, why)
		}
	}
}

// resolveBoardAgentFromErr is the error half of resolveBoardAgentFrom.
func resolveBoardAgentFromErr(panes []domain.PaneSnapshot) error {
	_, err := resolveBoardAgentFrom(panes)
	return err
}

// THE BOARD CLAUDE MUST BE AUTONOMOUS, and this test exists because it was not.
//
// Shipped in wave 4, `##board agent up` launched claude through the shared
// `##pane new --claude` command builder, which passes no permission flag. The
// board claude came up in MANUAL MODE — `⏸ manual mode on` on its own screen —
// so every prompt typed into the board reached an agent that would ask a human
// to approve each tool call. Its pane is hidden, so there is nobody to ask.
//
// It was invisible because everything reported success: the pane existed, was
// named, was hidden, and was running claude. `##board agent status` said
// `running 2.1.226`. Only asking the fleet to stop and watching nothing happen
// exposed it — design 025 §3's fail-closed stall with the approval pane missing.
func TestBoardClaudeIsLaunchedAutonomous(t *testing.T) {
	if boardAgentClaudeArgs == "" {
		t.Fatal("the board claude launches with no extra flags — it will come up in manual mode and can never act on the fleet")
	}
	cmd := claudeLaunchCommand("sid-1", "/tmp/p.txt", boardAgentClaudeArgs)
	if !strings.Contains(cmd, boardAgentClaudeArgs) {
		t.Fatalf("launch command drops the board claude's flags: %q", cmd)
	}
	if !strings.Contains(cmd, "--session-id sid-1") {
		t.Fatalf("launch command lost the pinned session id: %q", cmd)
	}
}

// The default stays manual for a pane a human opened and is watching.
func TestAPlainClaudePaneIsNotGivenTheBoardClaudesFlags(t *testing.T) {
	cmd := claudeLaunchCommand("sid-2", "", "")
	if strings.Contains(cmd, "--dangerously-skip-permissions") {
		t.Fatalf("`##pane new --claude` inherited the board claude's autonomy: %q", cmd)
	}
}
