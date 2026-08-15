// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// A board claude per fleet (design 028, founder gate `D-13`, ruled 2026-08-11).
//
// 027 ruling 2 put the board claude in the path for everything, knowingly
// paying a turn per message for the judgement it buys. That was ruled for one
// board; one mind in the path of twenty-five is a queue, and its failure is
// silent — messages get slower, not lost.

func TestEachBoardGetsItsOwnClaudeName(t *testing.T) {
	if got := boardAgentNameFor(""); got != "board" {
		t.Errorf("the session board's claude is %q, want the unchanged %q", got, "board")
	}
	if got := boardAgentNameFor(msg.DefaultBoardID); got != "board" {
		t.Errorf("the default board's claude is %q, want %q", got, "board")
	}
	if got := boardAgentNameFor("epic-07"); got != "board-epic-07" {
		t.Errorf("a fleet's board claude is %q, want board-epic-07", got)
	}
	if boardAgentNameFor("epic-07") == boardAgentNameFor("epic-08") {
		t.Fatal("two fleets' board claudes share a name: one prompt would reach both, " +
			"and resolveBoardAgent would refuse to address either")
	}
}

// TestResolvingABoardClaudeIgnoresAnotherFleets is the property the naming
// exists for: with several running, each board resolves ITS OWN mind.
func TestResolvingABoardClaudeIgnoresAnotherFleets(t *testing.T) {
	panes := []domain.PaneSnapshot{
		{ID: "p-07", GivenName: "board-epic-07"},
		{ID: "p-08", GivenName: "board-epic-08"},
		{ID: "p-sess", GivenName: "board"},
	}
	pick := func(boardID string) []domain.PaneSnapshot {
		want := boardAgentNameFor(boardID)
		var out []domain.PaneSnapshot
		for _, p := range panes {
			if p.GivenName == want {
				out = append(out, p)
			}
		}
		return out
	}

	for _, c := range []struct{ board, wantPane string }{
		{"epic-07", "p-07"},
		{"epic-08", "p-08"},
		{msg.DefaultBoardID, "p-sess"},
	} {
		got, err := resolveBoardAgentNamed(pick(c.board), boardAgentNameFor(c.board))
		if err != nil {
			t.Fatalf("board %s: %v", c.board, err)
		}
		if got.ID != c.wantPane {
			t.Errorf("board %s resolved to %s, want %s", c.board, got.ID, c.wantPane)
		}
	}
}

// TestAMissingBoardClaudeSaysWhichNameItLookedFor. With several fleets running,
// "no pane named board" is true and useless — the operator needs to know which
// fleet's mind is missing.
func TestAMissingBoardClaudeSaysWhichNameItLookedFor(t *testing.T) {
	_, err := resolveBoardAgentNamed(nil, boardAgentNameFor("epic-07"))
	if err == nil {
		t.Fatal("a missing board claude resolved")
	}
	if !strings.Contains(err.Error(), "board-epic-07") {
		t.Fatalf("the refusal does not name the pane it wanted: %v", err)
	}
}

// TestAFleetsBoardClaudeIsBriefedForItsOwnFleet.
//
// THE BRIEF IS THE ONLY THING STOPPING IT acting across fleets. The verb is
// scoped (`E-41`), but the mind chooses which verb to run — a board claude
// handed the unscoped commands would read every fleet's stream and could stop
// agents belonging to work nobody asked it about.
func TestAFleetsBoardClaudeIsBriefedForItsOwnFleet(t *testing.T) {
	brief := boardAgentBriefFor("epic-07")

	if !strings.Contains(brief, "FLEET `epic-07`") {
		t.Error("the brief does not tell the claude which fleet it serves")
	}
	if !strings.Contains(brief, "rysh board tail --board epic-07") {
		t.Error("the brief does not scope the READ path to its own board")
	}
	if !strings.Contains(brief, "##ansa interrupt --fleet epic-07") {
		t.Error("the brief does not scope the STOP to its own fleet")
	}
	if strings.Contains(brief, "--all-fleets") {
		t.Error("a fleet's board claude was handed the session-wide stop; it would " +
			"stop fleets nobody asked it about")
	}
	if !strings.Contains(brief, "refuse and say which fleet it belongs to") {
		t.Error("the brief does not tell it what to do with a request about another fleet")
	}
}

// TestTheSessionBoardClaudeKeepsItsBriefVerbatim — the ruling adds fleets, it
// does not change what the session's own mind was told.
func TestTheSessionBoardClaudeKeepsItsBriefVerbatim(t *testing.T) {
	if boardAgentBriefFor("") != boardAgentBrief {
		t.Error("the session board claude's brief changed")
	}
	if boardAgentBriefFor(msg.DefaultBoardID) != boardAgentBrief {
		t.Error("the default board's brief changed")
	}
}
