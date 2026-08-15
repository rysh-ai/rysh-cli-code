// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// TestVerifyResizeLive drives the REAL resize messages (the exact ones the TUI
// sends in Ctrl+L layout mode) against a running rysh daemon and reads back
// real workspace snapshots to confirm the split boundary follows the arrow.
//
// It is gated on RYSH_VERIFY_PORT so it never runs during a normal `go test`.
// Start a daemon first:  rysh create verify-resize -d   (NATS port 24242)
// then:  RYSH_VERIFY_PORT=24242 go test ./internal/cli/ -run TestVerifyResizeLive -v
func TestVerifyResizeLive(t *testing.T) {
	portStr := os.Getenv("RYSH_VERIFY_PORT")
	if portStr == "" {
		t.Skip("RYSH_VERIFY_PORT not set; skipping live-session verification")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("bad RYSH_VERIFY_PORT %q: %v", portStr, err)
	}

	sess := os.Getenv("RYSH_VERIFY_SESSION")
	if sess == "" {
		sess = "verify-resize"
	}
	// Subjects are namespaced per session; match the running daemon's prefix.
	msg.SetSessionPrefix(sess)

	c, err := NewClient(port, sess)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()

	snap := func() domain.WorkspaceSnapshot {
		t.Helper()
		reply, err := c.RequestSnapshot(3 * time.Second)
		if err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		r, ok := reply.(*msg.MsgWorkspaceSnapshotReply)
		if !ok {
			t.Fatalf("unexpected reply %T", reply)
		}
		return r.Snapshot
	}
	activeTab := func(s domain.WorkspaceSnapshot) domain.TabSnapshot {
		for _, tb := range s.Tabs {
			if tb.ID == s.ActiveTabID {
				return tb
			}
		}
		return s.Tabs[0]
	}
	laneFlexes := func(tb domain.TabSnapshot) []int {
		out := make([]int, len(tb.Lanes))
		for i, ln := range tb.Lanes {
			out[i] = ln.Flex
		}
		return out
	}
	// activeLaneIdx returns the index of the lane that owns the active pane.
	activeLaneIdx := func(tb domain.TabSnapshot) int {
		for i, ln := range tb.Lanes {
			for _, g := range ln.PaneGroups {
				for _, p := range g.Panes {
					if p.ID == tb.ActivePaneID {
						return i
					}
				}
			}
		}
		return -1
	}
	send := func(m interface{}) {
		if err := c.Send(m); err != nil {
			t.Fatalf("send %T: %v", m, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	focusLane := func(target int) {
		for i := 0; i < 6; i++ {
			tb := activeTab(snap())
			cur := activeLaneIdx(tb)
			if cur == target {
				return
			}
			if cur < target {
				send(&msg.MsgFocusPane{Direction: msg.DirRight})
			} else {
				send(&msg.MsgFocusPane{Direction: msg.DirLeft})
			}
		}
	}

	// --- Ensure a 2-lane horizontal layout ---
	for i := 0; i < 4; i++ {
		tb := activeTab(snap())
		if len(tb.Lanes) >= 2 {
			break
		}
		t.Logf("creating a 2nd lane (split right)...")
		send(&msg.MsgCreatePane{})
	}
	tb := activeTab(snap())
	if len(tb.Lanes) < 2 {
		t.Fatalf("need >=2 lanes, got %d", len(tb.Lanes))
	}
	t.Logf("layout: %d lanes, flexes=%v", len(tb.Lanes), laneFlexes(tb))

	// ================= HORIZONTAL =================
	// The width arrows widen (→) / narrow (←) the FOCUSED lane, taking the width
	// from the lane on its right so its own left edge does not move. See
	// resizeActiveLane. (This replaced "the boundary follows the arrow": that
	// rule made ← grow a focused lane leftward, sliding its left edge out from
	// under it.)
	//
	// Case 1: focus the LEFT lane, press RIGHT. It widens; lane1 pays.
	focusLane(0)
	before := laneFlexes(activeTab(snap()))
	send(&msg.MsgResizePaneWidth{Delta: 1}) // RIGHT arrow
	after := laneFlexes(activeTab(snap()))
	t.Logf("[focus=LEFT]  RIGHT arrow: lane flex %v -> %v", before, after)
	if !(after[0] > before[0] && after[1] < before[1]) {
		t.Errorf("RIGHT (focus left): focused lane should widen (lane0 grow, lane1 shrink); %v -> %v", before, after)
	}

	// LEFT arrow narrows it again, handing the width back to lane1.
	before = laneFlexes(activeTab(snap()))
	send(&msg.MsgResizePaneWidth{Delta: -1}) // LEFT arrow
	after = laneFlexes(activeTab(snap()))
	t.Logf("[focus=LEFT]  LEFT arrow:  lane flex %v -> %v", before, after)
	if !(after[0] < before[0] && after[1] > before[1]) {
		t.Errorf("LEFT (focus left): focused lane should narrow (lane0 shrink, lane1 grow); %v -> %v", before, after)
	}

	// Case 2, the rightmost lane — the documented exception. It has no lane to
	// its right, so it borrows from lane0, and → must STILL mean wider (this is
	// the case that reverses c3eb7a8's position-independent rule).
	focusLane(1)
	before = laneFlexes(activeTab(snap()))
	send(&msg.MsgResizePaneWidth{Delta: 1}) // RIGHT arrow
	after = laneFlexes(activeTab(snap()))
	t.Logf("[focus=RIGHT] RIGHT arrow: lane flex %v -> %v (rightmost borrows leftward)", before, after)
	if !(after[1] > before[1] && after[0] < before[0]) {
		t.Errorf("RIGHT (focus right): the rightmost lane should WIDEN; %v -> %v", before, after)
	}

	// Case 3, the anchor itself — it needs a lane on BOTH sides of the focused
	// one to be observable, so two lanes cannot show it. With three, focus the
	// middle lane: whichever arrow is pressed, lane0 must not move (the focused
	// lane's left edge holds) and lane2 must absorb the difference.
	focusLane(0)
	send(&msg.MsgCreatePane{}) // split right off lane0 -> a third lane
	if n := len(activeTab(snap()).Lanes); n < 3 {
		t.Logf("only %d lanes; skipping the middle-lane anchor check", n)
	} else {
		send(&msg.MsgEqualizeAll{})
		for _, tc := range []struct {
			name  string
			delta int
			wider bool
		}{
			{"RIGHT", 1, true},
			{"LEFT", -1, false},
		} {
			focusLane(1)
			before = laneFlexes(activeTab(snap()))
			send(&msg.MsgResizePaneWidth{Delta: tc.delta})
			after = laneFlexes(activeTab(snap()))
			t.Logf("[focus=MIDDLE of 3] %s arrow: lane flex %v -> %v", tc.name, before, after)

			if after[0] != before[0] {
				t.Errorf("%s (focus middle): lane0 moved %d -> %d; the focused lane's LEFT edge must hold",
					tc.name, before[0], after[0])
			}
			if tc.wider && !(after[1] > before[1] && after[2] < before[2]) {
				t.Errorf("%s (focus middle): expected wider off lane2; %v -> %v", tc.name, before, after)
			}
			if !tc.wider && !(after[1] < before[1] && after[2] > before[2]) {
				t.Errorf("%s (focus middle): expected narrower back to lane2; %v -> %v", tc.name, before, after)
			}
		}
		focusLane(1) // the vertical section below splits inside the active lane
	}

	// ================= VERTICAL =================
	// Same rule with "below" in place of "right": ↓ makes the focused group
	// taller, ↑ shorter, and the height comes from the group underneath, so the
	// focused group's TOP edge holds. Three groups, for the same reason three
	// lanes were needed above — with two, the focused group is always an edge
	// group and the anchor is unobservable.
	send(&msg.MsgCreatePaneDown{})
	send(&msg.MsgCreatePaneDown{})
	grpRowFlex := func() []int {
		tb := activeTab(snap())
		i := activeLaneIdx(tb)
		if i < 0 {
			i = 0
		}
		gs := tb.Lanes[i].PaneGroups
		out := make([]int, len(gs))
		for j, g := range gs {
			out[j] = g.RowFlex
		}
		return out
	}
	// activeGroupIdx locates the group holding the active pane within its lane.
	activeGroupIdx := func() int {
		tb := activeTab(snap())
		i := activeLaneIdx(tb)
		if i < 0 {
			return -1
		}
		for j, g := range tb.Lanes[i].PaneGroups {
			for _, p := range g.Panes {
				if p.ID == tb.ActivePaneID {
					return j
				}
			}
		}
		return -1
	}
	focusGroup := func(target int) {
		for i := 0; i < 6; i++ {
			cur := activeGroupIdx()
			if cur == target {
				return
			}
			if cur < target {
				send(&msg.MsgFocusPane{Direction: msg.DirDown})
			} else {
				send(&msg.MsgFocusPane{Direction: msg.DirUp})
			}
		}
	}

	if n := len(grpRowFlex()); n < 3 {
		t.Logf("only %d group(s) in the active lane; skipping the vertical anchor check", n)
		return
	}
	send(&msg.MsgEqualizeVertical{})

	// Middle group: the top edge must hold for BOTH arrows.
	for _, tc := range []struct {
		name   string
		delta  int
		taller bool
	}{
		{"DOWN", 1, true},
		{"UP", -1, false},
	} {
		focusGroup(1)
		gb := grpRowFlex()
		send(&msg.MsgResizePaneHeight{Delta: tc.delta})
		ga := grpRowFlex()
		t.Logf("[focus=MIDDLE of 3] %s arrow: group rowflex %v -> %v", tc.name, gb, ga)

		if ga[0] != gb[0] {
			t.Errorf("%s (focus middle): group0 moved %d -> %d; the focused group's TOP edge must hold",
				tc.name, gb[0], ga[0])
		}
		if tc.taller && !(ga[1] > gb[1] && ga[2] < gb[2]) {
			t.Errorf("%s (focus middle): expected taller off group2; %v -> %v", tc.name, gb, ga)
		}
		if !tc.taller && !(ga[1] < gb[1] && ga[2] > gb[2]) {
			t.Errorf("%s (focus middle): expected shorter back to group2; %v -> %v", tc.name, gb, ga)
		}
	}

	// Bottommost group: no group below, so it borrows upward — and ↓ must STILL
	// mean taller.
	focusGroup(2)
	gb := grpRowFlex()
	send(&msg.MsgResizePaneHeight{Delta: 1}) // DOWN arrow
	ga := grpRowFlex()
	t.Logf("[focus=BOTTOM of 3] DOWN arrow: group rowflex %v -> %v (borrows upward)", gb, ga)
	if !(ga[2] > gb[2] && ga[1] < gb[1]) {
		t.Errorf("DOWN (focus bottom): the bottommost group should GROW; %v -> %v", gb, ga)
	}
}
