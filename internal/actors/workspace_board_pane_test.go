package actors

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

// The agents-board pane is created with PaneType "agents-board" (design 025,
// founder ruling gate 1), which is what keeps it shell-less:
// domain.IsShelllessPaneType gates the shell start in pane.go.

func TestOpenAgentsBoardPaneFailsClosedWithNoTab(t *testing.T) {
	// A WorkspaceActor with no tabs is the state a headless daemon can be in
	// before any tab exists. The board must decline rather than mint a pane id
	// it then cannot place -- and crucially must NOT record boardPaneID, or a
	// later open would report "already open" for a pane that never existed.
	w := &WorkspaceActor{}
	var out strings.Builder
	w.openAgentsBoardPane(&out, "")

	if got := out.String(); !strings.Contains(got, "no active tab") {
		t.Errorf("expected a 'no active tab' message, got %q", got)
	}
	if w.boardPaneID != "" {
		t.Errorf("boardPaneID = %q, want empty: a failed open must not record a pane",
			w.boardPaneID)
	}
	if w.ryshFail == nil {
		t.Error("a failed open must record the failure (##board open is statusAware): " +
			"ryshFail is nil, so the command would report success")
	}
	if w.resCounts.panes != 0 {
		t.Errorf("resCounts.panes = %d, want 0: nothing was created",
			w.resCounts.panes)
	}
}

// TestAgentsBoardPaneTypeIsTheShelllessOne is the link between the pane this
// file creates and the guard that keeps it read-only. If someone changes the
// PaneType string here without updating IsShelllessPaneType, the board grows a
// PTY and becomes a terminal a stray keystroke can type into.
func TestAgentsBoardPaneTypeIsTheShelllessOne(t *testing.T) {
	if !domain.IsShelllessPaneType(domain.PaneTypeAgentsBoard) {
		t.Fatalf("the pane type this file creates (%q) must be shell-less",
			domain.PaneTypeAgentsBoard)
	}
	if domain.PaneTypeAgentsBoard == domain.PaneTypeReplay ||
		domain.PaneTypeAgentsBoard == domain.PaneTypeApproval {
		t.Errorf("agents-board must be its OWN pane type, not an alias of an "+
			"existing one; got %q", domain.PaneTypeAgentsBoard)
	}
}

// TestBoardOpenIsReachableFromTheDispatcher pins the user-facing door.
//
// The board is worth nothing if a human cannot open it. A handler that exists
// but is not wired to a verb is the "parse layer one step ahead of the
// enforcement layer" defect the 2026-07-21 audit found nine times; this asserts
// the verb reaches openAgentsBoardPane and that the subcommand is documented,
// since a rysh verb ships its own help.
func TestBoardOpenIsReachableFromTheDispatcher(t *testing.T) {
	cmd, ok := lookupRyshCommand("board")
	if !ok || cmd == nil {
		t.Fatal("##board is not registered in the dispatch table")
	}
	var help strings.Builder
	for _, h := range cmd.help {
		help.WriteString(h)
	}
	if !strings.Contains(help.String(), "##board open") {
		t.Errorf("##board open is undocumented; help was:\n%s", help.String())
	}

	// Route the verb the way the dispatcher does and assert it reached the
	// pane-opening path (which fails closed with no tab) rather than the
	// posting path (which would complain about having no pane to post as).
	w := &WorkspaceActor{}
	var out strings.Builder
	if err := w.handleBoardCommand(&out, "", []string{"open"}); err != nil {
		t.Fatalf("##board open returned %v", err)
	}
	if got := out.String(); !strings.Contains(got, "no active tab") {
		t.Errorf("##board open did not reach openAgentsBoardPane; got %q", got)
	}
}
