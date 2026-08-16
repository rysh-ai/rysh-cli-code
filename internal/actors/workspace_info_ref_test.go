// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// F-55 — `##lane info <ref>` and `##panegroup info <ref>` ignored their
// positional argument and reported the CALLER's lane/stack instead: a full,
// confident, exit-0 answer about something the caller had not asked about.
// Found live — `##lane info 3` answered for lane 1 ("position 1 of 3") on a
// three-lane tab, and `##pg info nonexistent-ref-xyz` described the caller's
// own stack. That is F-16 (fixed for `##pane info` in c80c27e) unfixed in two
// more families.
//
// These tests pin all three of F-16's rules in both families:
//
//	1. REFUSE, DO NOT GUESS  — an unresolvable ref errors naming the ref
//	2. DO NOT FOCUS          — a cross-tab read does not move the human's cursor
//	3. NO ARGUMENT KEEPS WORKING — the bare form still reports the caller's own

// serveTabSnapshot answers tab.<id>.snapshot with exactly the snapshot given,
// so a test can lay out lanes and stacks precisely.
func serveTabSnapshot(t *testing.T, nc *nats.Conn, codecs *msg.CodecRegistry, snap domain.TabSnapshot) {
	t.Helper()
	sub, err := nc.Subscribe(msg.T("tab", snap.ID, "snapshot"), func(m *nats.Msg) {
		var env msg.NATSEnvelope
		if err := json.Unmarshal(m.Data, &env); err != nil {
			t.Errorf("responder %s: envelope: %v", snap.ID, err)
			return
		}
		re := &msg.RequestEnvelope{ReplyTo: env.ReplyTo, NC: nc, Codecs: codecs}
		if err := re.Reply(&msg.MsgTabSnapshotReply{Snapshot: snap}); err != nil {
			t.Errorf("responder %s: reply: %v", snap.ID, err)
		}
	})
	if err != nil {
		t.Fatalf("subscribe responder for %s: %v", snap.ID, err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
}

const (
	laneBuild = "11111111-aaaa-4444-8888-000000000001"
	laneTest  = "22222222-bbbb-4444-8888-000000000002"
	laneShip  = "33333333-cccc-4444-8888-000000000003"
	laneBeta  = "99999999-dddd-4444-8888-000000000009"

	stackTop    = "aaaaaaaa-1111-4444-8888-000000000001"
	stackBottom = "eeeeeeee-1111-4444-8888-00000000000e"
	stackTest   = "bbbbbbbb-2222-4444-8888-000000000002"
	stackBeta   = "dddddddd-9999-4444-8888-00000000000d"

	callerPane = "pCaller"
)

// infoWorkspace builds the two-tab fixture every test below shares:
//
//	tab-1 "alpha"   <- the caller is here, in lane 1's FIRST stack
//	  lane 1 "build" (laneBuild)
//	    stack 1 (stackTop)     pCaller, pTop2
//	    stack 2 (stackBottom)  pBottom
//	  lane 2 "test"  (laneTest)   stack 1 (stackTest)  pTest
//	  lane 3 "ship"  (laneShip)   stack 1 (cccccccc…)  pShip
//	tab-2 "beta"
//	  lane 1 ""      (laneBeta)   stack 1 (stackBeta)  pBeta
func infoWorkspace(t *testing.T) *WorkspaceActor {
	t.Helper()
	nc := startInProcessNATS(t)
	codecs := msg.DefaultCodecRegistry()

	alpha := domain.TabSnapshot{
		ID: "tab-1", Title: "alpha", ActivePaneID: callerPane,
		Lanes: []domain.LaneSnapshot{
			{
				ID: laneBuild, Name: "build", Flex: 10, ActivePaneID: callerPane,
				PaneGroups: []domain.PaneGroupSnapshot{
					{ID: stackTop, ActivePaneID: callerPane, Panes: []domain.PaneSnapshot{
						{ID: callerPane, Title: "caller"},
						{ID: "pTop2", Title: "top-two"},
					}},
					{ID: stackBottom, Panes: []domain.PaneSnapshot{{ID: "pBottom", Title: "bottom"}}},
				},
			},
			{
				ID: laneTest, Name: "test", Flex: 10,
				PaneGroups: []domain.PaneGroupSnapshot{
					{ID: stackTest, Panes: []domain.PaneSnapshot{{ID: "pTest", Title: "tester"}}},
				},
			},
			{
				ID: laneShip, Name: "ship", Flex: 7,
				PaneGroups: []domain.PaneGroupSnapshot{
					{ID: "cccccccc-3333-4444-8888-000000000003", Panes: []domain.PaneSnapshot{{ID: "pShip", Title: "shipper"}}},
				},
			},
		},
	}
	beta := domain.TabSnapshot{
		ID: "tab-2", Title: "beta",
		Lanes: []domain.LaneSnapshot{{
			ID: laneBeta, Flex: 10,
			PaneGroups: []domain.PaneGroupSnapshot{
				{ID: stackBeta, Panes: []domain.PaneSnapshot{{ID: "pBeta", Title: "beta-pane"}}},
			},
		}},
	}
	serveTabSnapshot(t, nc, codecs, alpha)
	serveTabSnapshot(t, nc, codecs, beta)

	return &WorkspaceActor{
		pub:          msg.NewNATSPublisher(nc, codecs),
		tabs:         []*tabInfo{{id: "tab-1", title: "alpha"}, {id: "tab-2", title: "beta"}},
		activeTabIdx: 0,
		activePaneID: callerPane,
	}
}

func laneInfo(t *testing.T, w *WorkspaceActor, args ...string) string {
	t.Helper()
	var out strings.Builder
	w.handleLaneCommand(&out, callerPane, append([]string{"info"}, args...))
	return out.String()
}

func stackInfo(t *testing.T, w *WorkspaceActor, args ...string) string {
	t.Helper()
	var out strings.Builder
	w.handlePaneGroupCommand(&out, callerPane, append([]string{"info"}, args...))
	return out.String()
}

// field pulls "  name        : build" out of an info block.
func field(out, key string) string {
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// ##lane info
// ---------------------------------------------------------------------------

// Rule 3. The bare form is what people and scripts already use.
func TestLaneInfoBareReportsCallersLane(t *testing.T) {
	w := infoWorkspace(t)
	out := laneInfo(t, w)
	if got := field(out, "name"); got != "build" {
		t.Errorf("bare ##lane info reported lane %q, want the caller's lane \"build\"\n%s", got, out)
	}
	if got := field(out, "position"); got != "1 of 3" {
		t.Errorf("position = %q, want \"1 of 3\"\n%s", got, out)
	}
	if w.ryshFail != nil {
		t.Errorf("bare form failed: %v", w.ryshFail)
	}
}

// THE regression. This is the exact command that exposed F-55 live: on a
// three-lane tab, `##lane info 3` reported lane 1.
func TestLaneInfoByIndexReportsThatLane(t *testing.T) {
	w := infoWorkspace(t)
	out := laneInfo(t, w, "3")

	if got := field(out, "name"); got != "ship" {
		t.Errorf("##lane info 3 reported lane %q, want \"ship\" — the ref was ignored\n%s", got, out)
	}
	if got := field(out, "position"); got != "3 of 3" {
		t.Errorf("position = %q, want \"3 of 3\"\n%s", got, out)
	}
	if got := field(out, "id"); got != laneShip {
		t.Errorf("id = %q, want %q", got, laneShip)
	}
	// The caller's own lane must not leak into the answer at all — the old
	// behaviour printed it in full and looked completely plausible.
	if strings.Contains(out, "build") || strings.Contains(out, laneBuild) {
		t.Errorf("the caller's lane appears in the answer for lane 3:\n%s", out)
	}
	// flex is read off the reported lane, not the caller's.
	if got := field(out, "flex"); got != "7" {
		t.Errorf("flex = %q, want \"7\" (lane 3's) — fields must all come from the named lane\n%s", got, out)
	}
}

func TestLaneInfoByNameAndByFullID(t *testing.T) {
	for _, ref := range []string{"test", laneTest} {
		w := infoWorkspace(t)
		out := laneInfo(t, w, ref)
		if got := field(out, "id"); got != laneTest {
			t.Errorf("##lane info %q reported id %q, want %q\n%s", ref, got, laneTest, out)
		}
		if got := field(out, "position"); got != "2 of 3" {
			t.Errorf("##lane info %q position = %q, want \"2 of 3\"", ref, got)
		}
	}
}

// The eight characters `##lane list` prints must be a ref you can paste back.
// Before this change they were not: ResolveLane matched only the full uuid, so
// the id the listing showed you was rejected with "lane not found".
func TestLaneInfoByIDPrefixFromTheListing(t *testing.T) {
	w := infoWorkspace(t)
	out := laneInfo(t, w, laneShip[:8])
	if got := field(out, "id"); got != laneShip {
		t.Errorf("8-char prefix %q reported id %q, want %q\n%s", laneShip[:8], got, laneShip, out)
	}
}

// Rule 1. The old behaviour answered about the caller; anything unresolvable
// must now be an error that names what was asked for.
func TestLaneInfoUnknownRefRefusesRatherThanGuessing(t *testing.T) {
	w := infoWorkspace(t)
	out := laneInfo(t, w, "nonexistent-ref-xyz")

	if w.ryshFail == nil {
		t.Error("an unresolvable lane ref exited 0 — it must fail, or the wrong answer stays invisible")
	}
	if !strings.Contains(out, "nonexistent-ref-xyz") {
		t.Errorf("the error does not name the ref that failed:\n%s", out)
	}
	if strings.Contains(out, "lane info") || field(out, "flex") != "" {
		t.Errorf("an info block was printed for an unresolvable ref:\n%s", out)
	}
}

// Rule 2. A lane in another tab is readable, and reading it does not move the
// human's focus — `info` is a read.
func TestLaneInfoCrossTabDoesNotMoveFocus(t *testing.T) {
	w := infoWorkspace(t)
	out := laneInfo(t, w, laneBeta)

	if got := field(out, "tab"); got != "beta" {
		t.Errorf("tab = %q, want \"beta\" — a lane id is session-unique and must resolve across tabs\n%s", got, out)
	}
	if w.activeTabIdx != 0 {
		t.Errorf("activeTabIdx = %d, want 0 — a read must not switch the active tab", w.activeTabIdx)
	}
	if w.activePaneID != callerPane {
		t.Errorf("activePaneID = %q, want %q — a read must not move the cursor", w.activePaneID, callerPane)
	}
}

// An index is per-tab, so it must NOT be swept across tabs: tab-2 has a lane 1,
// and `##lane info 1` from tab-1 means tab-1's lane 1, always.
func TestLaneInfoIndexStaysInTheCallersTab(t *testing.T) {
	w := infoWorkspace(t)
	out := laneInfo(t, w, "1")
	if got := field(out, "id"); got != laneBuild {
		t.Errorf("##lane info 1 reported %q, want the caller's tab's lane 1 (%q)\n%s", got, laneBuild, out)
	}
}

// ---------------------------------------------------------------------------
// ##panegroup info
// ---------------------------------------------------------------------------

func TestStackInfoBareReportsCallersStack(t *testing.T) {
	w := infoWorkspace(t)
	out := stackInfo(t, w)
	if got := field(out, "id"); got != stackTop {
		t.Errorf("bare ##pg info reported %q, want the caller's stack %q\n%s", got, stackTop, out)
	}
	if w.ryshFail != nil {
		t.Errorf("bare form failed: %v", w.ryshFail)
	}
}

// The live symptom for this family: any ref at all returned the caller's stack.
func TestStackInfoByRefReportsThatStack(t *testing.T) {
	w := infoWorkspace(t)
	out := stackInfo(t, w, stackTest)

	if got := field(out, "id"); got != stackTest {
		t.Errorf("##pg info %q reported %q — the ref was ignored\n%s", stackTest, got, out)
	}
	if got := field(out, "lane"); !strings.HasPrefix(got, "2 ") {
		t.Errorf("lane = %q, want lane 2 (where that stack lives)\n%s", got, out)
	}
	// The pane listing must be the named stack's, not the caller's.
	if !strings.Contains(out, "pTest") {
		t.Errorf("the named stack's pane is missing from the listing:\n%s", out)
	}
	if strings.Contains(out, callerPane) || strings.Contains(out, "pTop2") {
		t.Errorf("the caller's own panes leaked into another stack's listing:\n%s", out)
	}
}

// A stack index is scoped to a LANE, so `##pg info 2` means the caller's lane's
// second stack — never some other lane's.
func TestStackInfoByIndexIsScopedToCallersLane(t *testing.T) {
	w := infoWorkspace(t)
	out := stackInfo(t, w, "2")
	if got := field(out, "id"); got != stackBottom {
		t.Errorf("##pg info 2 reported %q, want the caller's lane's second stack %q\n%s", got, stackBottom, out)
	}
	if got := field(out, "group"); got != "2 of 2" {
		t.Errorf("group = %q, want \"2 of 2\"", got)
	}
}

func TestStackInfoByIDPrefixFromTheListing(t *testing.T) {
	w := infoWorkspace(t)
	out := stackInfo(t, w, stackTest[:8])
	if got := field(out, "id"); got != stackTest {
		t.Errorf("8-char prefix %q reported %q, want %q\n%s", stackTest[:8], got, stackTest, out)
	}
}

func TestStackInfoUnknownRefRefusesRatherThanGuessing(t *testing.T) {
	w := infoWorkspace(t)
	out := stackInfo(t, w, "nonexistent-ref-xyz")

	if w.ryshFail == nil {
		t.Error("an unresolvable stack ref exited 0 — it must fail")
	}
	if !strings.Contains(out, "nonexistent-ref-xyz") {
		t.Errorf("the error does not name the ref that failed:\n%s", out)
	}
	if strings.Contains(out, "pane group info") {
		t.Errorf("an info block was printed for an unresolvable ref:\n%s", out)
	}
}

func TestStackInfoCrossTabDoesNotMoveFocus(t *testing.T) {
	w := infoWorkspace(t)
	out := stackInfo(t, w, stackBeta)

	if got := field(out, "tab"); got != "beta" {
		t.Errorf("tab = %q, want \"beta\"\n%s", got, out)
	}
	if w.activeTabIdx != 0 || w.activePaneID != callerPane {
		t.Errorf("focus moved: activeTabIdx=%d activePaneID=%q", w.activeTabIdx, w.activePaneID)
	}
}
