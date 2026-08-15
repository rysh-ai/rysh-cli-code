// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Tests for W1-3: `##ansa interrupt [<pane-id> | --fleet]`.
//
// THESE ASSERT ON THE WIRE, and for this verb that is not a stylistic
// preference. The failure mode being guarded against is an interrupt reaching a
// pane it should have spared — the human's shell, the roadmap supervisor, the
// board claude — and that failure is COMPLETELY INVISIBLE in a return value. A
// test that checks "the call succeeded and reported 2 panes" passes just as
// happily when a third pane also got ESC. So the assertion is: subscribe to the
// spared pane's rawinput subject and require ZERO messages on it.

// ansaInterruptFleetSnapshot is a session with two fleet panes and two panes
// that belong to nobody — the shape the whole safety argument is about.
func ansaInterruptFleetSnapshot() *domain.WorkspaceSnapshot {
	return &domain.WorkspaceSnapshot{
		Tabs: []domain.TabSnapshot{{
			ID: "tab-1",
			Lanes: []domain.LaneSnapshot{{
				ID: "lane-1",
				PaneGroups: []domain.PaneGroupSnapshot{{
					ID: "group-1",
					Panes: []domain.PaneSnapshot{
						{ID: "pane-mgr", GivenName: "mgr-01",
							Meta: map[string]string{"fleet.role": "manager", "fleet.name": "agent-fleet"}},
						{ID: "pane-wkr", GivenName: "wkr-01",
							Meta: map[string]string{"fleet.role": "worker", "fleet.name": "agent-fleet"}},
						// The human's own shell. No fleet meta.
						{ID: "pane-human", GivenName: "shell"},
						// The board claude: deliberately carries no fleet.* meta,
						// so it is spared without anybody listing it.
						{ID: "pane-board", GivenName: "board-claude",
							Meta: map[string]string{"claude.session_id": "abc123"}},
					},
				}},
			}},
		}},
	}
}

// serveAnsaSnapshot answers T("ws","snapshot") with the given snapshot, so the
// REAL natsAnsaTransport can be exercised over a real bus.
func serveAnsaSnapshot(t *testing.T, nc *nats.Conn, codecs *msg.CodecRegistry, snap *domain.WorkspaceSnapshot) {
	t.Helper()
	sub, err := nc.Subscribe(msg.T("ws", "snapshot"), func(m *nats.Msg) {
		// The reply subject travels INSIDE the envelope, not in the NATS reply
		// header: msg.NATSPublisher.Request mints its own inbox and Publish()es,
		// so m.Respond has nothing to answer. Reading env.ReplyTo is what a real
		// actor does (see internal/bridge).
		var in msg.NATSEnvelope
		if err := json.Unmarshal(m.Data, &in); err != nil || in.ReplyTo == "" {
			return
		}
		reply := &msg.MsgWorkspaceSnapshotReply{Snapshot: *snap}
		payload, perr := json.Marshal(reply)
		if perr != nil {
			return
		}
		data, eerr := json.Marshal(msg.NATSEnvelope{
			TypeTag: codecs.TagOf(reply),
			Payload: payload,
		})
		if eerr != nil {
			return
		}
		_ = nc.Publish(in.ReplyTo, data)
	})
	if err != nil {
		t.Fatalf("subscribe ws.snapshot: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
}

// watchRawInput counts MsgRawKeyInput messages landing on a pane's rawinput
// subject, and keeps the payloads so a test can say WHICH bytes arrived.
type rawInputWatch struct {
	ch chan *msg.MsgRawKeyInput
}

func watchRawInput(t *testing.T, nc *nats.Conn, codecs *msg.CodecRegistry, paneID string) *rawInputWatch {
	t.Helper()
	w := &rawInputWatch{ch: make(chan *msg.MsgRawKeyInput, 16)}
	sub, err := nc.Subscribe(msg.T("pane", paneID, "rawinput"), func(m *nats.Msg) {
		_, decoded := ansaDecode(t, codecs, m.Data)
		if k, ok := decoded.(*msg.MsgRawKeyInput); ok {
			w.ch <- k
		}
	})
	if err != nil {
		t.Fatalf("subscribe rawinput for %s: %v", paneID, err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	return w
}

func (w *rawInputWatch) next(t *testing.T, what string) *msg.MsgRawKeyInput {
	t.Helper()
	select {
	case k := <-w.ch:
		return k
	case <-time.After(3 * time.Second):
		t.Fatalf("no MsgRawKeyInput arrived for %s", what)
		return nil
	}
}

// requireSilent asserts that NOTHING arrived on this pane's rawinput.
func (w *rawInputWatch) requireSilent(t *testing.T, paneID string) {
	t.Helper()
	select {
	case k := <-w.ch:
		t.Fatalf("pane %s received MsgRawKeyInput %q — it carries no %s metadata and MUST "+
			"have been spared. --fleet is selecting more than the fleet",
			paneID, string(k.Data), ansaMetaFleetRole)
	default:
	}
}

// TestAnsaInterruptFleetSparesEveryPaneWithNoFleetRole is THE test.
//
// Two fleet panes and two panes that belong to nobody. After `--fleet`, the two
// fleet panes must have received exactly one ESC each and the other two must
// have received NOTHING AT ALL — asserted by counting on their subjects, not by
// reading the command's own account of what it did.
func TestAnsaInterruptFleetSparesEveryPaneWithNoFleetRole(t *testing.T) {
	nc := newABLATestNATS(t)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)
	serveAnsaSnapshot(t, nc, codecs, ansaInterruptFleetSnapshot())

	mgr := watchRawInput(t, nc, codecs, "pane-mgr")
	wkr := watchRawInput(t, nc, codecs, "pane-wkr")
	human := watchRawInput(t, nc, codecs, "pane-human")
	board := watchRawInput(t, nc, codecs, "pane-board")

	r := &ansaRouter{tr: &natsAnsaTransport{pub: pub}}
	outcomes, refusal := r.InterruptFleet("")
	if refusal != nil {
		t.Fatalf("InterruptFleet refused: %s", refusal.Error)
	}

	// THE WIRE ASSERTIONS COME FIRST, before any check on what the call
	// REPORTED. If both are wrong the failure must print the bytes that
	// actually moved, not the command's own account of itself — the whole
	// reason this test exists is that the account is the thing that lies.

	// The fleet panes were reached: the optimistic path, proven REACHABLE and
	// not merely safe (design 026 §5.4a). These block, which also flushes the
	// bus so the silence checks below are meaningful.
	if got := mgr.next(t, "pane-mgr"); string(got.Data) != string(ansaInterruptKey) {
		t.Errorf("pane-mgr got %q, want ESC", string(got.Data))
	}
	if got := wkr.next(t, "pane-wkr"); string(got.Data) != string(ansaInterruptKey) {
		t.Errorf("pane-wkr got %q, want ESC", string(got.Data))
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	time.Sleep(100 * time.Millisecond) // let any WRONG publish land before we claim silence

	// And the spared panes are SILENT. This is the assertion the order was
	// written around.
	human.requireSilent(t, "pane-human")
	board.requireSilent(t, "pane-board")

	// Only now, what the call claimed.
	if len(outcomes) != 2 {
		t.Fatalf("interrupted %d panes, want 2: %+v", len(outcomes), outcomes)
	}
	for _, o := range outcomes {
		if o.Err != nil {
			t.Fatalf("interrupt for %s failed: %v", o.PaneID, o.Err)
		}
	}
}

// TestAnsaInterruptSendsOneEscAndNoCR.
//
// ESC+CR would SUBMIT rather than interrupt — it is what the delivery path
// does, and routing interrupt through it is the single worst available bug on
// this verb. One message, one byte.
func TestAnsaInterruptSendsOneEscAndNoCR(t *testing.T) {
	nc := newABLATestNATS(t)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)

	const pane = "pane-uuid-running-claude"
	w := watchRawInput(t, nc, codecs, pane)

	if err := ansaInterruptPane(pub, pane); err != nil {
		t.Fatalf("ansaInterruptPane: %v", err)
	}

	first := w.next(t, pane)
	if len(first.Data) != 1 || first.Data[0] != 0x1b {
		t.Fatalf("interrupt byte = %#v, want a single 0x1b (ESC)", first.Data)
	}
	if first.PaneID != pane {
		t.Errorf("PaneID = %q, want %q", first.PaneID, pane)
	}

	// Nothing else follows. A CR here would submit whatever is in the composer.
	select {
	case extra := <-w.ch:
		t.Fatalf("a second message followed the ESC: %q — ESC+CR SUBMITS a prompt instead "+
			"of interrupting one", string(extra.Data))
	case <-time.After(500 * time.Millisecond):
	}
}

// TestAnsaInterruptFleetRefusesWhenNoPaneCarriesFleetMeta.
//
// Fails CLOSED: with metadata unpopulated, --fleet interrupts nothing. And it
// says so as a REFUSAL — "0 panes interrupted" printed as a normal result reads
// as "the fleet is idle", when it far more often means the meta was never
// written.
func TestAnsaInterruptFleetRefusesWhenNoPaneCarriesFleetMeta(t *testing.T) {
	tr := &fakeAnsaTransport{panes: []ansaPane{
		{ID: "pane-a", GivenName: "shell", Meta: map[string]string{}},
		{ID: "pane-b", GivenName: "board-claude", Meta: map[string]string{"claude.session_id": "x"}},
	}}
	r := &ansaRouter{tr: tr}

	outcomes, refusal := r.InterruptFleet("")
	if refusal == nil {
		t.Fatalf("--fleet with no fleet meta returned success and %d outcomes — silence here "+
			"reads as 'nothing to do' when it means 'the metadata is missing'", len(outcomes))
	}
	if refusal.Code != msg.AnsaErrUnknownTarget {
		t.Errorf("refusal code = %q", refusal.Code)
	}
	if n := len(tr.interruptedPanes()); n != 0 {
		t.Fatalf("%d panes were interrupted despite the refusal: %v", n, tr.interruptedPanes())
	}
}

// TestAnsaInterruptRefusesAnAmbiguousNameAndInterruptsNothing.
//
// The addressing rule, applied to this verb: guessing would cancel a turn
// belonging to an agent the caller never named. The LENGTH of interrupted is
// the assertion — a refusal that still interrupted is worse than no refusal.
func TestAnsaInterruptRefusesAnAmbiguousNameAndInterruptsNothing(t *testing.T) {
	tr := &fakeAnsaTransport{panes: twoPanesSharingAName()}

	target, refusal := ansaResolveTarget(tr.panes, "wkr-01")
	if refusal == nil {
		t.Fatalf("an ambiguous name resolved to %s instead of being refused", target.ID)
	}
	if refusal.Code != msg.AnsaErrAmbiguousTarget {
		t.Errorf("code = %q, want %q", refusal.Code, msg.AnsaErrAmbiguousTarget)
	}
	if len(refusal.Candidates) != 2 {
		t.Errorf("refusal carries %d candidate ids, want 2 — a refusal without the ids is a "+
			"wall, not an error", len(refusal.Candidates))
	}
	if n := len(tr.interruptedPanes()); n != 0 {
		t.Fatalf("%d panes were interrupted while resolving an ambiguous name", n)
	}
}

// TestAnsaInterruptRefusesANameAtTheRouter: the router addresses BY ID only.
// A name arriving there is a caller that skipped edge resolution, and resolving
// it silently would put a name where an id belongs.
func TestAnsaInterruptRefusesANameAtTheRouter(t *testing.T) {
	tr := &fakeAnsaTransport{panes: twoPanesSharingAName()}
	r := &ansaRouter{tr: tr}

	res := r.Interrupt("mgr-01")
	if res.OK {
		t.Fatal("the router accepted a NAME as an interrupt address")
	}
	if res.Code != msg.AnsaErrNotAnID {
		t.Errorf("code = %q, want %q", res.Code, msg.AnsaErrNotAnID)
	}
	if n := len(tr.interruptedPanes()); n != 0 {
		t.Fatalf("%d panes were interrupted: %v", n, tr.interruptedPanes())
	}

	// An unknown id is its own code, and also interrupts nothing.
	res = r.Interrupt("pane-uuid-that-never-existed")
	if res.OK || res.Code != msg.AnsaErrUnknownTarget {
		t.Errorf("unknown id gave OK=%v code=%q", res.OK, res.Code)
	}
	if n := len(tr.interruptedPanes()); n != 0 {
		t.Fatalf("%d panes were interrupted for an unknown id", n)
	}
}

// TestAnsaInterruptSingleTargetReachesExactlyThatPane — the optimistic path for
// the single-target form, and it must reach a pane WITHOUT fleet meta too:
// naming a pane explicitly is a different act from sweeping a fleet, and the
// fleet.role rule belongs to --fleet alone.
func TestAnsaInterruptSingleTargetReachesExactlyThatPane(t *testing.T) {
	tr := &fakeAnsaTransport{panes: []ansaPane{
		{ID: "pane-human", GivenName: "shell", Meta: map[string]string{}},
		{ID: "pane-wkr", GivenName: "wkr-01", Meta: map[string]string{"fleet.role": "worker"}},
	}}
	r := &ansaRouter{tr: tr}

	res := r.Interrupt("pane-human")
	if !res.OK {
		t.Fatalf("naming a non-fleet pane explicitly was refused: %s", res.Error)
	}
	got := tr.interruptedPanes()
	if len(got) != 1 || got[0] != "pane-human" {
		t.Fatalf("interrupted %v, want exactly [pane-human]", got)
	}
}

// TestAnsaInterruptFleetReportsAPerPaneFailureAndKeepsGoing: one pane's
// publish failure must not abort the sweep. An abort halfway through, reported
// as one error, leaves the operator unable to tell which half ran.
func TestAnsaInterruptFleetReportsAPerPaneFailureAndKeepsGoing(t *testing.T) {
	tr := &fakeAnsaTransport{
		panes: []ansaPane{
			{ID: "pane-a", GivenName: "wkr-a", Meta: map[string]string{"fleet.role": "worker"}},
			{ID: "pane-b", GivenName: "wkr-b", Meta: map[string]string{"fleet.role": "worker"}},
		},
		interruptErr: errors.New("nats: connection closed"),
	}
	r := &ansaRouter{tr: tr}

	outcomes, refusal := r.InterruptFleet("")
	if refusal != nil {
		t.Fatalf("unexpected refusal: %s", refusal.Error)
	}
	if len(outcomes) != 2 {
		t.Fatalf("the sweep stopped early: %d outcomes, want 2", len(outcomes))
	}
	for _, o := range outcomes {
		if o.Err == nil {
			t.Errorf("pane %s reported success despite a failing transport", o.PaneID)
		}
	}
}

// TestAnsaInterruptDoesNotProbe pins the ONE place this verb diverges from
// Route, so the divergence stays a decision rather than becoming an
// inconsistency somebody "fixes".
//
// A pane worth interrupting is a pane that is stuck, and a stuck pane is
// precisely the one least likely to answer a snapshot request. Probing first
// would refuse to interrupt exactly the panes that need it — and would report
// that refusal as prudence.
func TestAnsaInterruptDoesNotProbe(t *testing.T) {
	tr := &fakeAnsaTransport{
		panes:    []ansaPane{{ID: "pane-wedged", GivenName: "wkr-01", Meta: map[string]string{"fleet.role": "worker"}}},
		probeErr: errors.New("no responders — this pane is wedged"),
	}
	r := &ansaRouter{tr: tr}

	res := r.Interrupt("pane-wedged")
	if !res.OK {
		t.Fatalf("a wedged pane could not be interrupted because the probe failed: %s — "+
			"that refuses exactly the panes this verb exists for", res.Error)
	}
	if len(tr.probed) != 0 {
		t.Fatalf("Interrupt probed %v; it must not probe at all", tr.probed)
	}
	if got := tr.interruptedPanes(); len(got) != 1 || got[0] != "pane-wedged" {
		t.Fatalf("interrupted %v, want [pane-wedged]", got)
	}
}

// TestAnsaUsageDocumentsInterrupt — F-19 again: an undocumented verb is an
// invisible one, and the usage table is how ## verbs stay documented.
func TestAnsaUsageDocumentsInterrupt(t *testing.T) {
	var out strings.Builder
	w := &WorkspaceActor{}
	_ = w.ansaUsage(&out, "test")
	s := out.String()
	for _, want := range []string{"##ansa interrupt", "--fleet"} {
		if !strings.Contains(s, want) {
			t.Errorf("##ansa usage does not document %q:\n%s", want, s)
		}
	}
}

// THE CEO IS PART OF THE FLEET, and "stop all fleet" that leaves the fleet's own
// CEO running has not stopped the fleet.
//
// Filed by the founder after watching a demo in which the CEO pane kept working
// through a `--fleet` interrupt. THE CAUSE WAS NOT HERE: ansaFleetPanes selects
// on any non-empty fleet.role, `ceo` included, and fleetctl writes fleet.role on
// every roster member including the CEO (fleetctl.py:946, member built :905).
// The demo's CEO pane simply carried no fleet meta at all, because the demo
// staged it as an untagged control — a defect in the shoot, not in the router.
//
// The test exists anyway, and this is why: the reported symptom is exactly what
// a "restrict --fleet to manager and worker" change would produce, that change
// looks reasonable in isolation, and nothing else would catch it. A rule nobody
// pins is a rule that gets tidied away.
func TestAnsaInterruptFleetIncludesTheCEO(t *testing.T) {
	snap := ansaInterruptFleetSnapshot()
	// A CEO pane, tagged the way fleetctl tags one.
	snap.Tabs[0].Lanes[0].PaneGroups[0].Panes = append(
		snap.Tabs[0].Lanes[0].PaneGroups[0].Panes,
		domain.PaneSnapshot{ID: "pane-ceo", GivenName: "fleet-ceo",
			Meta: map[string]string{"fleet.role": "ceo", "fleet.name": "agent-fleet"}},
	)

	nc := newABLATestNATS(t)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)
	serveAnsaSnapshot(t, nc, codecs, snap)

	ceo := watchRawInput(t, nc, codecs, "pane-ceo")
	human := watchRawInput(t, nc, codecs, "pane-human")

	r := &ansaRouter{tr: &natsAnsaTransport{pub: pub}}
	outcomes, refusal := r.InterruptFleet("")
	if refusal != nil {
		t.Fatalf("InterruptFleet refused: %s", refusal.Error)
	}

	// ON THE WIRE first: the CEO's pane must actually receive the ESC.
	k := ceo.next(t, "the CEO pane's interrupt")
	if len(k.Data) != 1 || k.Data[0] != 0x1b {
		t.Fatalf("the CEO pane got %q, want a single ESC — "+
			"`stop all fleet` must stop the fleet's own CEO", k.Data)
	}
	human.requireSilent(t, "pane-human")

	var sawCEO bool
	for _, o := range outcomes {
		if o.PaneID == "pane-ceo" && o.Role == "ceo" {
			sawCEO = true
		}
	}
	if !sawCEO {
		t.Fatal("the CEO was not among the reported targets, so a caller reading the " +
			"report would believe the whole fleet was stopped when it was not")
	}
}

// THE BOARD CLAUDE MUST BE INTERRUPTED LAST, and this is asserted on ARRIVAL
// ORDER rather than on the target list, because the list is what the code
// intended and the wire is what happened.
//
// The board claude carries a fleet-wide interrupt: a human types "stop all
// fleet" into the board, it decides, it runs the verb. If its own ESC lands
// while it is still dispatching, it cancels the turn doing the dispatching and
// the fleet is left half-stopped — with the board reporting a stop that never
// finished. Founder ruling 2026-08-10, and the failure it prevents is
// receipt-without-delivery inflicted on itself.
//
// One wildcard subscription, so the order observed IS the order on the bus and
// not an artefact of how many goroutines were watching.
func TestAnsaInterruptFleetStopsTheBoardClaudeLast(t *testing.T) {
	snap := ansaInterruptFleetSnapshot()
	// The board claude's pane, tagged so it IS a target — the ordering only
	// matters when it is one.
	snap.Tabs[0].Lanes[0].PaneGroups[0].Panes = append(
		snap.Tabs[0].Lanes[0].PaneGroups[0].Panes,
		domain.PaneSnapshot{ID: "pane-boardclaude", GivenName: boardAgentName,
			Meta: map[string]string{"fleet.role": "board", "fleet.name": "agent-fleet"}},
		domain.PaneSnapshot{ID: "pane-ceo2", GivenName: "fleet-ceo",
			Meta: map[string]string{"fleet.role": "ceo", "fleet.name": "agent-fleet"}},
	)

	nc := newABLATestNATS(t)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)
	serveAnsaSnapshot(t, nc, codecs, snap)

	// ONE subscription over every pane's rawinput: arrival order on a single
	// subscription is the bus's order.
	arrivals := make(chan string, 32)
	sub, err := nc.Subscribe(msg.T("pane", "*", "rawinput"), func(m *nats.Msg) {
		parts := strings.Split(m.Subject, ".")
		if len(parts) >= 3 {
			arrivals <- parts[len(parts)-2]
		}
	})
	if err != nil {
		t.Fatalf("subscribe wildcard rawinput: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	r := &ansaRouter{tr: &natsAnsaTransport{pub: pub}}
	if _, refusal := r.InterruptFleet(""); refusal != nil {
		t.Fatalf("InterruptFleet refused: %s", refusal.Error)
	}

	var order []string
	deadline := time.After(3 * time.Second)
	want := 4 // mgr, wkr, ceo2, boardclaude — the human's shell carries no fleet meta
	for len(order) < want {
		select {
		case id := <-arrivals:
			order = append(order, id)
		case <-deadline:
			t.Fatalf("only %d of %d interrupts arrived: %v", len(order), want, order)
		}
	}

	last := order[len(order)-1]
	if last != "pane-boardclaude" {
		t.Fatalf("the board claude was interrupted at position %v, not last (order: %v).\n"+
			"It carries the dispatch: stopping it before the final target means the "+
			"fleet-wide interrupt was cut off half-delivered", last, order)
	}
	for _, id := range order[:len(order)-1] {
		if id == "pane-boardclaude" {
			t.Fatalf("the board claude appears before the end of the order: %v", order)
		}
	}
}
