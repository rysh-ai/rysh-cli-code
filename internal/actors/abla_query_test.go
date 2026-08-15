// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Tests for the board's READ path, end to end over a real NATS server against a
// real ABLA (design 027, work order W1-1).
//
// The headline is NOT the round trip — it is
// TestBoardQueryWithNoRecorderIsNotAnEmptyBoard. Every expensive defect on this
// track was a system reporting an empty or successful state it had not earned:
// F-20's roster read "1 agent" because nothing was listening, F-23's restore
// opened the wrong KV bucket and rendered an empty board as a quiet one. A read
// path that answers "nothing" when it means "I could not ask" would be the
// third instance of the same shape, and it would be the worst of the three,
// because this one is the surface an agent is told to trust.

const boardQueryTestTimeout = 2 * time.Second

// TestBoardTailRoundTripsThroughTheRecorder: a post published on the bus is
// heard by ABLA and comes back to a reader that ASKS — no KV bucket is opened
// by the reader, and no TUI exists anywhere in this test.
func TestBoardTailRoundTripsThroughTheRecorder(t *testing.T) {
	nc := newABLATestNATS(t)
	kv := newABLATestKV(t, nc)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)

	abla := NewAgentBoardListenerActor(nc, codecs, kv)
	abla.Start()
	t.Cleanup(abla.Stop)

	thread := msg.MintThreadID(ablaPaneA, 1)
	if err := msg.SendBoardPost(pub, "", post(ablaPaneA, "mgr-01", "wave 1 done", thread, 100)); err != nil {
		t.Fatalf("SendBoardPost: %v", err)
	}
	if err := msg.SendBoardPost(pub, "", post(ablaPaneB, "wkr-01", "confirmed", thread, 101)); err != nil {
		t.Fatalf("SendBoardPost: %v", err)
	}
	if err := msg.SendBoardRegister(pub, "", msg.NewBoardRegister(ablaPaneA, "mgr-01", 1)); err != nil {
		t.Fatalf("SendBoardRegister: %v", err)
	}
	ablaWaitFor(t, "both posts in the recorder", func() bool {
		return abla.Store().Stats().Posts == 2
	})

	reply, err := board.Ask(nc, "", board.Query{}, boardQueryTestTimeout)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if len(reply.Threads) != 1 {
		t.Fatalf("want 1 thread back, got %d: %+v", len(reply.Threads), reply.Threads)
	}
	th := reply.Threads[0]
	if th.Root == nil || th.Root.Text != "wave 1 done" || th.Root.Persona != "mgr-01" {
		t.Fatalf("root did not survive the round trip: %+v", th.Root)
	}
	if len(th.Replies) != 1 || th.Replies[0].Text != "confirmed" {
		t.Fatalf("reply did not survive the round trip: %+v", th.Replies)
	}
	if th.Key != thread {
		t.Fatalf("thread key = %q, want %q", th.Key, thread)
	}
	if reply.Stats.Posts != 2 {
		t.Fatalf("Stats.Posts = %d, want 2", reply.Stats.Posts)
	}
	if len(reply.Roster) != 1 || reply.Roster[0].Persona != "mgr-01" {
		t.Fatalf("roster did not survive the round trip: %+v", reply.Roster)
	}
}

// TestBoardQueryWithNoRecorderIsNotAnEmptyBoard is the test this order was
// written around.
//
// A real bus is up — the connection is healthy, the subject is right, the
// request goes out — and NO ABLA is running. The reader must come back with
// ErrNoRecorder and a NIL reply. An empty QueryReply here would be a claim that
// the fleet has posted nothing, made by a caller that never heard from anyone.
func TestBoardQueryWithNoRecorderIsNotAnEmptyBoard(t *testing.T) {
	nc := newABLATestNATS(t) // a live bus, deliberately with nothing serving the board

	reply, err := board.Ask(nc, "", board.Query{}, 300*time.Millisecond)

	if err == nil {
		t.Fatalf("Ask succeeded with no recorder running and returned %+v — "+
			"an unanswered query rendered as an empty board is the F-20/F-23 defect", reply)
	}
	if !errors.Is(err, board.ErrNoRecorder) {
		t.Fatalf("Ask error = %v, want it to wrap ErrNoRecorder so a caller can branch on it", err)
	}
	if reply != nil {
		t.Fatalf("Ask returned a non-nil reply (%+v) alongside its error — a caller that "+
			"forgets to check err must have NOTHING to render, not an empty board", reply)
	}
}

// TestBoardQueryStopsAnsweringWhenTheRecorderStops: a stopped recorder goes
// quiet.
//
// Same property the liveness subject has, and it matters more here. A stopped
// ABLA still holds its last in-memory board; if it kept answering, a reader
// would get a frozen snapshot that is indistinguishable from a live one and
// would grow staler by the minute with nothing saying so.
func TestBoardQueryStopsAnsweringWhenTheRecorderStops(t *testing.T) {
	nc := newABLATestNATS(t)
	kv := newABLATestKV(t, nc)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)

	abla := NewAgentBoardListenerActor(nc, codecs, kv)
	abla.Start()

	if err := msg.SendBoardPost(pub, "", post(ablaPaneA, "mgr-01", "still here", "", 100)); err != nil {
		t.Fatalf("SendBoardPost: %v", err)
	}
	ablaWaitFor(t, "the post to reach the recorder", func() bool {
		return abla.Store().Stats().Posts == 1
	})
	if _, err := board.Ask(nc, "", board.Query{}, boardQueryTestTimeout); err != nil {
		t.Fatalf("Ask before Stop: %v", err)
	}

	abla.Stop()

	reply, err := board.Ask(nc, "", board.Query{}, 300*time.Millisecond)
	if err == nil {
		t.Fatalf("a STOPPED recorder answered with %+v — a frozen board that looks live "+
			"is worse than one that admits it cannot be reached", reply)
	}
	if !errors.Is(err, board.ErrNoRecorder) {
		t.Fatalf("error after Stop = %v, want ErrNoRecorder", err)
	}
}

// TestBoardQueryIsServedOnTheSubjectBuiltFromT: the read path uses msg.T, so it
// follows the session prefix. A literal "rysh.board.query" works only in a
// session that happens to be named rysh — which is no real session.
func TestBoardQueryIsServedOnTheSubjectBuiltFromT(t *testing.T) {
	original := msg.SessionPrefix()
	t.Cleanup(func() { msg.SetSessionPrefix(original) })
	msg.SetSessionPrefix("macmini-rysh-elect")

	if got, want := msg.BoardQuerySubject(""), "macmini-rysh-elect.board.query"; got != want {
		t.Fatalf("BoardQuerySubject() = %q, want %q", got, want)
	}

	nc := newABLATestNATS(t)
	kv := newABLATestKV(t, nc)
	abla := NewAgentBoardListenerActor(nc, msg.DefaultCodecRegistry(), kv)
	abla.Start()
	t.Cleanup(abla.Stop)

	// Served on the prefixed subject...
	if _, err := nc.Request("macmini-rysh-elect.board.query", nil, boardQueryTestTimeout); err != nil {
		t.Fatalf("recorder does not answer on the session-prefixed subject: %v", err)
	}
	// ...and NOT on the default-session literal older docs quote.
	if _, err := nc.Request("rysh.board.query", nil, 300*time.Millisecond); err == nil {
		t.Fatal("the recorder answered on the literal \"rysh.board.query\" — the subject is " +
			"not being built from the session prefix")
	} else if !errors.Is(err, nats.ErrNoResponders) && !errors.Is(err, nats.ErrTimeout) {
		t.Fatalf("unexpected error probing the literal subject: %v", err)
	}
}

// TestBoardQueryRefusesAnUnreadableQueryRatherThanAnsweringEmpty: a malformed
// request is an ERROR, never `{"threads":[]}`. The caller has no way to detect
// the difference, so the recorder must not create one.
func TestBoardQueryRefusesAnUnreadableQueryRatherThanAnsweringEmpty(t *testing.T) {
	nc := newABLATestNATS(t)
	kv := newABLATestKV(t, nc)
	abla := NewAgentBoardListenerActor(nc, msg.DefaultCodecRegistry(), kv)
	abla.Start()
	t.Cleanup(abla.Stop)

	m, err := nc.Request(msg.BoardQuerySubject(""), []byte("{not json"), boardQueryTestTimeout)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	var reply board.QueryReply
	if uerr := json.Unmarshal(m.Data, &reply); uerr != nil {
		t.Fatalf("recorder answered with something unparseable: %q", m.Data)
	}
	if reply.Err == "" {
		t.Fatalf("an unreadable query was answered with a clean board (%q) — the caller "+
			"cannot tell that from a quiet fleet", m.Data)
	}
}

// ---------------------------------------------------------------------------
// F-26 — the ghost roster on the read path
// ---------------------------------------------------------------------------
//
// `rysh board tail --json` served ABLA's persisted roster verbatim, so panes
// deleted during a session kept their entries. Observed live: 7 roster entries
// for 6 live panes, while the rendered board showed 5 agents at the same
// instant. Two surfaces over one store, disagreeing about who exists.
//
// The rule already existed and was already correct — board.Store.RetainRoster,
// applied by the TUI's seedRosterFromSnapshot. The read path simply did not
// apply it. It is applied in ABLA's query handler and NOT in the CLI, because
// two copies of a rule is how F-18 happened.
//
// THE PRECONDITION THESE TESTS EXIST TO PROTECT: an empty or failed snapshot
// means "the caller does not know", NOT "nobody is here". Two of the four tests
// below are about nothing else.

// ablaLivePanesSnapshot builds a workspace snapshot containing the given pane
// ids as ordinary, agent-hosting panes.
func ablaLivePanesSnapshot(ids ...string) *domain.WorkspaceSnapshot {
	panes := make([]domain.PaneSnapshot, 0, len(ids))
	for _, id := range ids {
		panes = append(panes, domain.PaneSnapshot{ID: id, GivenName: "pane-" + id})
	}
	return &domain.WorkspaceSnapshot{
		Tabs: []domain.TabSnapshot{{
			ID:    "tab-1",
			Lanes: []domain.LaneSnapshot{{ID: "lane-1", PaneGroups: []domain.PaneGroupSnapshot{{ID: "g1", Panes: panes}}}},
		}},
	}
}

// registerTwoAgentsAndOneGhost puts three registrations into a running ABLA:
// two panes that will be alive, and one that will not.
func registerTwoAgentsAndOneGhost(t *testing.T, abla *AgentBoardListenerActor, pub *msg.NATSPublisher) {
	t.Helper()
	for _, r := range []*msg.MsgBoardRegister{
		msg.NewBoardRegister(ablaPaneA, "mgr-01", 1),
		msg.NewBoardRegister(ablaPaneB, "wkr-01", 2),
		msg.NewBoardRegister("cccc3333-dead-4dea-9dea-deaddeaddead", "ghost-01", 3),
	} {
		if err := msg.SendBoardRegister(pub, "", r); err != nil {
			t.Fatalf("SendBoardRegister: %v", err)
		}
	}
	ablaWaitFor(t, "three registrations in the recorder", func() bool {
		return len(abla.Store().Roster()) == 3
	})
}

// TestBoardTailOmitsAPaneThatNoLongerExists is F-26 itself.
//
// It shows a roster query that OMITS a dead pane — not a test that merely calls
// the reconcile. Three registrations go in, two panes are alive, and the answer
// must name exactly those two.
func TestBoardTailOmitsAPaneThatNoLongerExists(t *testing.T) {
	nc := newABLATestNATS(t)
	kv := newABLATestKV(t, nc)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)
	serveAnsaSnapshot(t, nc, codecs, ablaLivePanesSnapshot(ablaPaneA, ablaPaneB))

	abla := NewAgentBoardListenerActor(nc, codecs, kv)
	abla.Start()
	t.Cleanup(abla.Stop)
	registerTwoAgentsAndOneGhost(t, abla, pub)

	reply, err := board.Ask(nc, "", board.Query{}, boardQueryTestTimeout)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	// The GHOST first. If both this and the disclosure flag are wrong, the
	// failure must print the roster that came back, not a boolean about it.
	for _, e := range reply.Roster {
		if e.PaneID != ablaPaneA && e.PaneID != ablaPaneB {
			t.Errorf("roster names pane %s (%s), which does not exist — a closed pane is "+
				"still being reported as an agent (F-26)", e.PaneID, e.Persona)
		}
	}
	if len(reply.Roster) != 2 {
		t.Fatalf("roster has %d entries for 2 live panes: %+v", len(reply.Roster), reply.Roster)
	}
	if !reply.RosterReconciled {
		t.Fatal("the answer claims its roster was not reconciled, but the snapshot was served")
	}
}

// TestBoardTailKeepsLivePanesWithTheirPersonas proves the OPTIMISTIC path is
// reachable, not merely that the cautious one is safe (026 §5.4a).
//
// Reconciling must drop ghosts and NOTHING ELSE: registrations remain
// authoritative for what a pane is CALLED, so the personas that survive must be
// the announced ones and not names re-derived from the snapshot.
func TestBoardTailKeepsLivePanesWithTheirPersonas(t *testing.T) {
	nc := newABLATestNATS(t)
	kv := newABLATestKV(t, nc)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)
	serveAnsaSnapshot(t, nc, codecs, ablaLivePanesSnapshot(ablaPaneA, ablaPaneB))

	abla := NewAgentBoardListenerActor(nc, codecs, kv)
	abla.Start()
	t.Cleanup(abla.Stop)
	registerTwoAgentsAndOneGhost(t, abla, pub)

	reply, err := board.Ask(nc, "", board.Query{}, boardQueryTestTimeout)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	byID := map[string]string{}
	for _, e := range reply.Roster {
		byID[e.PaneID] = e.Persona
	}
	if got := byID[ablaPaneA]; got != "mgr-01" {
		t.Errorf("pane A persona = %q, want %q — the ANNOUNCED name must survive "+
			"reconciliation; the snapshot decides who exists, not what they are called", got, "mgr-01")
	}
	if got := byID[ablaPaneB]; got != "wkr-01" {
		t.Errorf("pane B persona = %q, want %q", got, "wkr-01")
	}
}

// TestBoardTailLeavesTheRosterAloneWhenTheSnapshotFAILS.
//
// THE PRECONDITION. No snapshot responder exists, so the lookup errors. The
// FULL roster — ghost included — must come back untouched. Reconciling against
// a failed round trip would delete every entry the first time the workspace was
// busy, which is strictly worse than the ghost it fixes.
//
// The assertion is the surviving roster, not the absence of an error: a version
// that wiped the roster and returned cleanly would pass an error check.
func TestBoardTailLeavesTheRosterAloneWhenTheSnapshotFAILS(t *testing.T) {
	nc := newABLATestNATS(t)
	kv := newABLATestKV(t, nc)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)
	// Deliberately NO snapshot responder.

	abla := NewAgentBoardListenerActor(nc, codecs, kv)
	abla.Start()
	t.Cleanup(abla.Stop)
	registerTwoAgentsAndOneGhost(t, abla, pub)

	reply, err := board.Ask(nc, "", board.Query{}, boardQueryTestTimeout)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if len(reply.Roster) != 3 {
		t.Fatalf("roster has %d entries, want all 3 — a snapshot that COULD NOT BE ASKED was "+
			"treated as 'nobody is here', so the roster was deleted: %+v", len(reply.Roster), reply.Roster)
	}
	if reply.RosterReconciled {
		t.Error("the answer claims a reconciled roster although the lookup failed — a caller " +
			"cannot then tell an authoritative roster from a best-effort one")
	}
}

// TestBoardTailLeavesTheRosterAloneWhenTheSnapshotIsEMPTY.
//
// A DIFFERENT INPUT from the test above and it must be verified separately:
// that one is "I could not ask", this one is "I asked and was told nothing".
// Both are non-destructive, and only testing one of them would leave the other
// free to regress — they travel through different branches.
func TestBoardTailLeavesTheRosterAloneWhenTheSnapshotIsEMPTY(t *testing.T) {
	nc := newABLATestNATS(t)
	kv := newABLATestKV(t, nc)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)
	// A responder that answers successfully with ZERO panes.
	serveAnsaSnapshot(t, nc, codecs, &domain.WorkspaceSnapshot{})

	abla := NewAgentBoardListenerActor(nc, codecs, kv)
	abla.Start()
	t.Cleanup(abla.Stop)
	registerTwoAgentsAndOneGhost(t, abla, pub)

	reply, err := board.Ask(nc, "", board.Query{}, boardQueryTestTimeout)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if len(reply.Roster) != 3 {
		t.Fatalf("roster has %d entries, want all 3 — an EMPTY snapshot was read as 'nobody "+
			"is here' instead of 'the caller does not know': %+v", len(reply.Roster), reply.Roster)
	}
	if reply.RosterReconciled {
		t.Error("an empty snapshot was reported as a reconciled roster")
	}
}

// TestBoardTailRosterExcludesPanesThatCannotHostAnAgent: the filter is the SAME
// predicate the TUI uses (domain.PaneCanHostAnAgent). If the two surfaces used
// different rules they would disagree about who exists — which is the defect
// being fixed, reintroduced from the other side.
func TestBoardTailRosterExcludesPanesThatCannotHostAnAgent(t *testing.T) {
	nc := newABLATestNATS(t)
	kv := newABLATestKV(t, nc)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)

	// Pane A is an ordinary pane; pane B exists but is an agents-board pane and
	// therefore cannot host an agent.
	snap := ablaLivePanesSnapshot(ablaPaneA)
	snap.Tabs[0].Lanes[0].PaneGroups[0].Panes = append(
		snap.Tabs[0].Lanes[0].PaneGroups[0].Panes,
		domain.PaneSnapshot{ID: ablaPaneB, GivenName: "the-board", PaneType: domain.PaneTypeAgentsBoard},
	)
	serveAnsaSnapshot(t, nc, codecs, snap)

	abla := NewAgentBoardListenerActor(nc, codecs, kv)
	abla.Start()
	t.Cleanup(abla.Stop)
	registerTwoAgentsAndOneGhost(t, abla, pub)

	reply, err := board.Ask(nc, "", board.Query{}, boardQueryTestTimeout)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if len(reply.Roster) != 1 || reply.Roster[0].PaneID != ablaPaneA {
		t.Fatalf("roster = %+v, want only the one agent-hosting pane", reply.Roster)
	}
}
