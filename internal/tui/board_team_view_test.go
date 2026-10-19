// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
)

// t shows the team view: the fleet status table — who is progressing, idle or
// blocked — and the dependency tree under it, from the same golden fixture
// the flow view is tested against.
func TestBoardTeamViewRenders(t *testing.T) {
	m := boardFlowModel(t)
	pressBoardKey(t, &m, 't')
	if v := m.boardViewFor(boardPaneID).boardView(); v != board.ViewTeam {
		t.Fatalf("t set view %q, want team", v)
	}
	got := boardViewTextWidth(m, 200)
	if testing.Verbose() {
		fmt.Println(got)
	}
	for _, want := range []string{
		"showing team · 5 members · 1 blocked · keys s p a f t",
		"MEMBER       WORK      ACTIVITY  HEALTH       LAST POST  TASK / BLOCKING",
		// chain order, indented under the parent
		"└─ mgr-a     progress  working   ok",
		"plan the parser",
		// a blocked member names its blockers before its task
		"└─ wk-a1  blocked   idle      ok",
		"⧗ mgr-a, human · parser",
		// unhealthy is shouted, as in the flow badges
		"STALLED",
		"health: 2 no status · 2 ok · 1 stalled",
		// the dependency tree under the table
		"dependencies · 1 blocked · bottleneck: mgr-a (1 waiting)",
		"▣ mgr-a {progress·working}  ← 1 waiting",
		"└─ wk-a1 {blocked·idle}  · also blocked by human",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("team view missing %q:\n%s", want, got)
		}
	}
	// The table is a status screen: it opens at the top, not its last row.
	if !strings.Contains(strings.SplitN(got, "\n", 3)[1], "MEMBER") {
		t.Errorf("team view does not open at the table's header row:\n%s", got)
	}
	// Every other view's header advertises the new key.
	pressBoardKey(t, &m, 's')
	if h := boardViewTextWidth(m, 200); !strings.Contains(h, "keys s p a f t") {
		t.Errorf("status header does not list t:\n%s", h)
	}
}
