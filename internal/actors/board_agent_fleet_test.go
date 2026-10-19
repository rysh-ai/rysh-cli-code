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

// TestAMissingBoardClaudeNamesTheNearMiss. A pane the human called
// `etl-fix-board` is not the board claude of `etl-fix` and must not resolve —
// but the refusal must point at it, or "not running" next to a pane plainly
// called that board reads as a bug rather than a naming mismatch.
func TestAMissingBoardClaudeNamesTheNearMiss(t *testing.T) {
	want := boardAgentNameFor("etl-fix")
	near := []domain.PaneSnapshot{{ID: "77cbb01e-a445", GivenName: "etl-fix-board"}}

	_, err := resolveBoardAgentHinted(nil, near, want)
	if err == nil {
		t.Fatal("a near miss resolved — it must only be named in the refusal")
	}
	for _, needle := range []string{"did you mean", `"etl-fix-board"`, "77cbb01e", `"board-etl-fix"`} {
		if !strings.Contains(err.Error(), needle) {
			t.Errorf("refusal is missing %s: %v", needle, err)
		}
	}
}

func TestNearBoardAgentNameIsSameWordsOnly(t *testing.T) {
	want := boardAgentNameFor("etl-fix")
	for name, wantNear := range map[string]bool{
		"etl-fix-board":   true,
		"Board_ETL_fix":   true,
		"fix etl board":   true,
		"board-etl-fix":   false, // the exact name is a match, not a near miss
		"etl-fix":         false,
		"board":           false,
		"board-etl-fix-2": false,
		"":                false,
	} {
		if got := isNearBoardAgentName(name, want); got != wantNear {
			t.Errorf("isNearBoardAgentName(%q, %q) = %v, want %v", name, want, got, wantNear)
		}
	}
}

// A board claude that dies with the daemon must come back on its own. Its pane
// is hidden, so nobody is looking at it; without the resume, the pane restores
// as a bare shell under the board claude's name and every prompt typed into the
// board runs in bash. This holds the contract end to end: what the board
// claude's launch stamps is what the post-restore sweep reads back.
func TestABoardClaudeIsResumedAfterADaemonRestart(t *testing.T) {
	const sid = "46cd7c37-65ba-4d6f-b59e-08b1b69f014a"
	meta := map[string]string{}
	for _, kv := range nativeAgentMeta(nativeAgentClaude, "/work/datalake", boardAgentClaudeArgs, sid, false, "") {
		meta[kv[0]] = kv[1]
	}
	panes := []ansaPane{{ID: "6e2be3cb-802a", GivenName: boardAgentNameFor("etl-fix"), Meta: meta}}

	deliveries, _, _ := nativeResumePlan(panes, idleProbe, sessionAlwaysExists, nil)
	if len(deliveries) != 1 {
		t.Fatalf("the board claude was not resumed: %+v", deliveries)
	}
	cmd := deliveries[0].Command
	for _, needle := range []string{"--resume " + sid, boardAgentClaudeArgs, "cd '/work/datalake'"} {
		if !strings.Contains(cmd, needle) {
			t.Errorf("resume command is missing %q: %s", needle, cmd)
		}
	}
}

// The refusal and the status line tell the human the exact command that brings
// a fleet's board claude back — for the session board, without a --board that
// would name a board nobody created.
func TestBoardAgentUpCommandNamesTheBoard(t *testing.T) {
	if got := boardAgentUpCommand("etl-fix"); got != "##board agent up --board etl-fix" {
		t.Errorf("fleet board: got %q", got)
	}
	if got := boardAgentUpCommand(msg.DefaultBoardID); got != "##board agent up" {
		t.Errorf("session board: got %q", got)
	}
}
