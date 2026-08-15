// SPDX-License-Identifier: Apache-2.0

package daemontest_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/daemontest"
)

// `##pane list` prints a lane header, a group header, then one line per pane.
// The pane line's `[N] <name> id=<uuid>` prefix is the shape every parser in
// this tree keys on.
var (
	listLaneHeader  = regexp.MustCompile(`(?m)^\s+lane-\d+ `)
	listGroupHeader = regexp.MustCompile(`(?m)^\s+group-\d+\s*$`)
)

// paneIDs returns every pane id in `##pane list` order.
func paneIDs(t *testing.T, s *daemontest.Session) []string {
	t.Helper()
	return paneIDsIn(t, s, "")
}

// paneIDsIn is paneIDs for a named tab.
func paneIDsIn(t *testing.T, s *daemontest.Session, tab string) []string {
	t.Helper()
	cmd := "##pane list"
	if tab != "" {
		cmd += " --tab " + tab
	}
	out := s.MustSucceed(t, cmd)
	var ids []string
	for _, m := range listPaneID.FindAllStringSubmatch(out, -1) {
		ids = append(ids, m[1])
	}
	if len(ids) == 0 {
		t.Fatalf("no pane ids in ##pane list:\n%s", out)
	}
	return ids
}

// layout returns the number of lanes and stacks `##pane list` reports.
func layout(t *testing.T, s *daemontest.Session) (lanes, stacks int) {
	t.Helper()
	out := s.MustSucceed(t, "##pane list")
	return len(listLaneHeader.FindAllString(out, -1)), len(listGroupHeader.FindAllString(out, -1))
}

// shellPID asks ONE pane's shell for its own pid, through the pane rather than
// around it. That is the claim a move has to preserve: not "a shell is running
// there" but "the SAME shell, with the same children, is still running there".
//
// Every pane in the workspace is asked, and each writes to a file named after
// its OWN $RYSH_PANE. Addressing a single pane would need the fully qualified
// --tab/--lane/--pg/--pane selector chain (a bare --pane resolves within the
// active pane's group), and the lane and stack halves of that chain are exactly
// what a move changes — so the probe would be reading the layout it is meant to
// be checking. A pane id never changes, which makes it the one safe key here.
//
// The command is re-sent rather than waited on once: it is typed into a pane's
// shell, and a shell still starting up drops what it never read (the same
// reason readPaneIdentity retries).
func shellPID(t *testing.T, s *daemontest.Session, paneID, dir string) string {
	t.Helper()
	dump := filepath.Join(dir, "pid-"+paneID)
	_ = os.Remove(dump)
	cmd := "##cmd ws echo $$ > " + filepath.Join(dir, "pid-$RYSH_PANE")

	deadline := time.Now().Add(30 * time.Second)
	for attempt := 0; time.Now().Before(deadline); attempt++ {
		if attempt%20 == 0 {
			s.MustSucceed(t, cmd)
		}
		if b, err := os.ReadFile(dump); err == nil {
			if pid := strings.TrimSpace(string(b)); pid != "" {
				return pid
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("pane %s never wrote its shell pid to %s — its shell is not answering", paneID, dump)
	return ""
}

// TestMovePaneKeepsItsShellAlive is the property the whole feature rests on.
//
// A move that re-created the pane at the destination would look identical in
// every listing and be wrong in the only way that matters: the shell, its
// children, and any agent mid-turn inside it would be gone. So this asserts the
// pane's shell pid is UNCHANGED across two moves — one that empties the pane's
// lane (which drops the lane, the case that would have killed a pane spawned as
// its group's child) and one that creates a fresh lane for it.
func TestMovePaneKeepsItsShellAlive(t *testing.T) {
	s := daemontest.Fresh(t)
	dir := t.TempDir()

	s.MustSucceed(t, "##new lane")
	ids := paneIDs(t, s)
	if len(ids) < 2 {
		t.Fatalf("expected at least two panes after ##new lane, got %d", len(ids))
	}
	host, mover := ids[0], ids[len(ids)-1]

	before := shellPID(t, s, mover, dir)

	// Into the host pane's stack. This empties the mover's own lane, so the
	// lane and its stack are both dropped behind it.
	s.MustSucceed(t, "##move pane "+mover+" to-stacked-pane "+host)

	lanes, stacks := layout(t, s)
	if lanes != 1 || stacks != 1 {
		t.Errorf("after stacking: %d lane(s), %d stack(s); want 1 and 1 — "+
			"the emptied lane should have closed up behind the move", lanes, stacks)
	}
	if got := paneIDs(t, s); len(got) != len(ids) {
		t.Errorf("pane count changed across the move: %d -> %d", len(ids), len(got))
	}

	if after := shellPID(t, s, mover, dir); after != before {
		t.Fatalf("the moved pane's shell pid changed %s -> %s — the pane was re-created, "+
			"not moved, and everything running in it was lost", before, after)
	}

	// Back out into a lane of its own.
	s.MustSucceed(t, "##move pane "+mover+" to-new-lane")
	if lanes, stacks = layout(t, s); lanes != 2 || stacks != 2 {
		t.Errorf("after to-new-lane: %d lane(s), %d stack(s); want 2 and 2", lanes, stacks)
	}
	if after := shellPID(t, s, mover, dir); after != before {
		t.Fatalf("the shell pid changed %s -> %s on the second move", before, after)
	}
}

// TestMoveOutUnstacksWithoutRestarting covers the reverse of stacking: a pane
// leaves the stack it shares and takes a stack of its own in the same lane.
func TestMoveOutUnstacksWithoutRestarting(t *testing.T) {
	s := daemontest.Fresh(t)
	dir := t.TempDir()

	s.MustSucceed(t, "##new stack 1") // second pane in the active stack
	ids := paneIDs(t, s)
	if len(ids) < 2 {
		t.Fatalf("expected two panes after ##new stack 1, got %d", len(ids))
	}
	mover := ids[len(ids)-1]
	before := shellPID(t, s, mover, dir)

	if _, stacks := layout(t, s); stacks != 1 {
		t.Fatalf("expected one stack holding both panes, got %d", stacks)
	}
	s.MustSucceed(t, "##move pane "+mover+" out")
	if lanes, stacks := layout(t, s); lanes != 1 || stacks != 2 {
		t.Errorf("after out: %d lane(s), %d stack(s); want 1 and 2", lanes, stacks)
	}
	if after := shellPID(t, s, mover, dir); after != before {
		t.Fatalf("the shell pid changed %s -> %s across ##move pane out", before, after)
	}

	// A pane that is alone in its stack has nothing to move out of, and says so
	// rather than quietly doing nothing.
	s.MustFail(t, "##move pane "+mover+" out")
}

// TestMoveStackMovesEveryPaneAsOne checks the composite subject. A stack move is
// not a primitive: it is one pane transfer per pane, and the thing that can go
// wrong is the panes arriving as separate stacks (each pane creating its own)
// rather than as the one stack they were.
func TestMoveStackMovesEveryPaneAsOne(t *testing.T) {
	s := daemontest.Fresh(t)
	dir := t.TempDir()

	s.MustSucceed(t, "##new stack 2") // three panes in one stack
	s.MustSucceed(t, "##new lane")    // a second lane to move it into
	ids := paneIDs(t, s)
	if len(ids) != 4 {
		t.Fatalf("expected 4 panes (3 stacked + 1 new lane), got %d:\n%v", len(ids), ids)
	}
	lanes, stacks := layout(t, s)
	if lanes != 2 || stacks != 2 {
		t.Fatalf("setup produced %d lane(s), %d stack(s); want 2 and 2", lanes, stacks)
	}
	// The stacked panes are the first three listed; the new lane's pane is last.
	stacked := ids[:3]
	before := shellPID(t, s, stacked[0], dir)

	// Address the stack by a pane in it, which is the handle a human has.
	s.MustSucceed(t, "##move stack "+stacked[0]+" to-lane 2")

	if lanes, stacks = layout(t, s); lanes != 1 || stacks != 2 {
		t.Errorf("after the stack move: %d lane(s), %d stack(s); want 1 and 2 — "+
			"the three panes must arrive as ONE stack beside the destination's own", lanes, stacks)
	}
	if got := paneIDs(t, s); len(got) != 4 {
		t.Errorf("pane count changed across the stack move: 4 -> %d", len(got))
	}
	if after := shellPID(t, s, stacked[0], dir); after != before {
		t.Fatalf("a stacked pane's shell pid changed %s -> %s across the move", before, after)
	}
}

// TestMoveLaneToNewTabCarriesItsPanes checks the outermost composite: a lane
// move is one stack move per stack, and every pane has to end up in ONE lane in
// the destination tab rather than one lane each.
func TestMoveLaneToNewTabCarriesItsPanes(t *testing.T) {
	s := daemontest.Fresh(t)
	dir := t.TempDir()

	s.MustSucceed(t, "##new lane")            // lane 2, so lane 1 can leave
	s.MustSucceed(t, "##lane name departing") // the name must travel with it
	s.MustSucceed(t, "##new pane")            // a second stack in the active lane

	if lanes, _ := layout(t, s); lanes != 2 {
		t.Fatalf("setup produced %d lanes, want 2", lanes)
	}
	ids := paneIDs(t, s)
	before := shellPID(t, s, ids[0], dir)

	s.MustSucceed(t, "##move lane 1 to-new-tab")

	// The source tab keeps what is left; the new tab holds the lane.
	if got := paneIDsIn(t, s, "2"); len(got) == 0 {
		t.Fatalf("the new tab has no panes")
	}
	moved := s.MustSucceed(t, "##pane list --tab 2")
	if n := len(listLaneHeader.FindAllString(moved, -1)); n != 1 {
		t.Errorf("the moved lane arrived as %d lanes; want 1:\n%s", n, moved)
	}
	if after := shellPID(t, s, ids[0], dir); after != before {
		t.Fatalf("a moved lane's pane changed shell pid %s -> %s", before, after)
	}
}

// TestMoveRefusesToEmptyATab checks the one refusal that is not about a bad
// argument: a cross-tab move that would leave the source tab with no panes at
// all. `##tab delete` already refuses to produce that state.
func TestMoveRefusesToEmptyATab(t *testing.T) {
	s := daemontest.Fresh(t)

	s.MustSucceed(t, "##new tab")
	// `##pane list` with no --tab reports whatever the daemon considers active,
	// which for a CLI-issued command is not the tab just created — so the new
	// tab's pane is asked for by name.
	second := paneIDsIn(t, s, "2")
	if len(second) != 1 {
		t.Fatalf("expected exactly one pane in the new tab, got %d", len(second))
	}

	out := s.MustFail(t, "##move pane "+second[0]+" to-tab 1")
	if !strings.Contains(out, "no panes") {
		t.Errorf("the refusal does not say why:\n%s", out)
	}
	// And nothing moved.
	if got := paneIDsIn(t, s, "2"); len(got) != 1 {
		t.Errorf("a refused move still changed the layout: tab 2 now has %d panes", len(got))
	}
}

// TestMoveRejectsMalformedCommands checks that ##move reports a non-zero status
// for every shape a script could get wrong, rather than printing a complaint and
// exiting 0 — the exact class design 021 found five of.
func TestMoveRejectsMalformedCommands(t *testing.T) {
	s := daemontest.Shared(t)
	for _, cmd := range []string{
		"##move",
		"##move window to-lane 1",
		"##move pane",
		"##move pane to-lane no-such-lane",
		"##move pane to-stack no-such-stack",
		"##move lane no-such-lane to-tab 1",
		"##move tab 1 sideways",
		"##move pane to-lane 1 --index 0",
	} {
		t.Run(cmd, func(t *testing.T) { s.MustFail(t, cmd) })
	}
}

// TestMoveWithNoArgsIsHelp — a bare `##move` is a question, and a question that
// exits non-zero would abort a script that only wanted the usage. It is listed
// in the malformed set above as `##move` with no subject; here we assert the
// explicit help form succeeds.
func TestMoveHelpSucceeds(t *testing.T) {
	s := daemontest.Shared(t)
	out := s.MustSucceed(t, "##move help")
	for _, want := range []string{"to-lane", "to-stacked-pane", "--tab"} {
		if !strings.Contains(out, want) {
			t.Errorf("##move help never mentions %q:\n%s", want, out)
		}
	}
}
