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
	// Case 1: focus the LEFT lane, press RIGHT. Divider must move RIGHT, i.e.
	// lane0 grows and lane1 shrinks.
	focusLane(0)
	before := laneFlexes(activeTab(snap()))
	send(&msg.MsgResizePaneWidth{Delta: 1}) // RIGHT arrow
	after := laneFlexes(activeTab(snap()))
	t.Logf("[focus=LEFT]  RIGHT arrow: lane flex %v -> %v", before, after)
	if !(after[0] > before[0] && after[1] < before[1]) {
		t.Errorf("RIGHT (focus left): divider should move right (lane0 grow, lane1 shrink); %v -> %v", before, after)
	}

	// LEFT arrow must move the divider back LEFT.
	before = laneFlexes(activeTab(snap()))
	send(&msg.MsgResizePaneWidth{Delta: -1}) // LEFT arrow
	after = laneFlexes(activeTab(snap()))
	t.Logf("[focus=LEFT]  LEFT arrow:  lane flex %v -> %v", before, after)
	if !(after[0] < before[0] && after[1] > before[1]) {
		t.Errorf("LEFT (focus left): divider should move left (lane0 shrink, lane1 grow); %v -> %v", before, after)
	}

	// Case 2 (the bug): focus the RIGHT lane, press RIGHT. The OLD code grew the
	// *active* (right) lane, sliding the divider LEFT. The boundary must still
	// move RIGHT regardless of which lane is focused.
	focusLane(1)
	before = laneFlexes(activeTab(snap()))
	send(&msg.MsgResizePaneWidth{Delta: 1}) // RIGHT arrow
	after = laneFlexes(activeTab(snap()))
	t.Logf("[focus=RIGHT] RIGHT arrow: lane flex %v -> %v (position-independent check)", before, after)
	if !(after[0] > before[0] && after[1] < before[1]) {
		t.Errorf("RIGHT (focus right): divider should STILL move right; %v -> %v", before, after)
	}

	// ================= VERTICAL =================
	// Add a split-down group in the active lane, then drive UP/DOWN.
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
	if len(grpRowFlex()) < 2 {
		t.Logf("only %d group(s) in active lane; skipping vertical check", len(grpRowFlex()))
		return
	}
	gb := grpRowFlex()
	send(&msg.MsgResizePaneHeight{Delta: 1}) // DOWN arrow
	ga := grpRowFlex()
	t.Logf("DOWN arrow: group rowflex %v -> %v (divider should move down: g0 grow)", gb, ga)
	if !(ga[0] > gb[0] && ga[1] < gb[1]) {
		t.Errorf("DOWN: divider should move down (g0 grow, g1 shrink); %v -> %v", gb, ga)
	}

	gb = grpRowFlex()
	send(&msg.MsgResizePaneHeight{Delta: -1}) // UP arrow
	ga = grpRowFlex()
	t.Logf("UP arrow:   group rowflex %v -> %v (divider should move up: g0 shrink)", gb, ga)
	if !(ga[0] < gb[0] && ga[1] > gb[1]) {
		t.Errorf("UP: divider should move up (g0 shrink, g1 grow); %v -> %v", gb, ga)
	}
}
