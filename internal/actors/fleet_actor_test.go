// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/fleet"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// The fleet registry actor (design 028 §6.5, `E-40`). Real NATS, real
// JetStream, real subjects — the discipline the board's actor tests set.

const fleetTestTimeout = 3 * time.Second

func newFleetTestKV(t *testing.T, nc *nats.Conn) nats.KeyValue {
	t.Helper()
	js, err := nc.JetStream()
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	kv, err := js.CreateKeyValue(fleet.BucketConfig("afa-" + t.Name()))
	if err != nil {
		t.Fatalf("CreateKeyValue: %v", err)
	}
	return kv
}

func startFleetActor(t *testing.T, nc *nats.Conn, kv nats.KeyValue) *AgentFleetActor {
	t.Helper()
	a := NewAgentFleetActor(nc, msg.DefaultCodecRegistry(), kv)
	a.Start()
	t.Cleanup(a.Stop)
	return a
}

func registerFleet(t *testing.T, nc *nats.Conn, f fleet.Fleet) {
	t.Helper()
	if _, err := fleet.Send(nc, fleet.Update{Op: fleet.OpRegister, Fleet: f}, fleetTestTimeout); err != nil {
		t.Fatalf("register %q: %v", f.Name, err)
	}
}

// TestTheRegistryAnswersOverTheBus is the acceptance criterion: a fleet
// registered through the bus is readable through the bus, by anything that can
// reach the session — which is what makes the daemon the authority instead of a
// file in .rysh/fleet.
func TestTheRegistryAnswersOverTheBus(t *testing.T) {
	nc := newABLATestNATS(t)
	startFleetActor(t, nc, newFleetTestKV(t, nc))

	registerFleet(t, nc, fleet.Fleet{Name: "epic-07", Source: "tracks/fleet/epic07.md"})
	registerFleet(t, nc, fleet.Fleet{Name: "epic-08"})

	reply, err := fleet.Ask(nc, fleet.Query{}, fleetTestTimeout)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if len(reply.Fleets) != 2 {
		t.Fatalf("registry holds %d fleets, want 2: %+v", len(reply.Fleets), reply.Fleets)
	}
	if reply.Fleets[0].Name != "epic-07" || reply.Fleets[1].Name != "epic-08" {
		t.Fatalf("fleets came back as %+v, want a stable alphabetical order", reply.Fleets)
	}
	// The board id defaults to the name — one namespace, so a fleet and its
	// board cannot drift apart.
	if reply.Fleets[0].BoardID != "epic-07" {
		t.Errorf("board id = %q, want the fleet's own name", reply.Fleets[0].BoardID)
	}

	one, err := fleet.Ask(nc, fleet.Query{Name: "epic-08"}, fleetTestTimeout)
	if err != nil {
		t.Fatalf("Ask(epic-08): %v", err)
	}
	if len(one.Fleets) != 1 || one.Fleets[0].Name != "epic-08" {
		t.Fatalf("named query answered %+v", one.Fleets)
	}
}

// TestAnUnansweredQueryIsNotAnEmptySession.
//
// The two facts a registry reader must never confuse. With no actor running,
// Ask must return ErrNoRegistry and a NIL reply — never a usable-looking empty
// list, which would tell a caller this session runs no fleets and send it off to
// create duplicates of fleets that are already up.
func TestAnUnansweredQueryIsNotAnEmptySession(t *testing.T) {
	nc := newABLATestNATS(t) // no actor started

	reply, err := fleet.Ask(nc, fleet.Query{}, 300*time.Millisecond)
	if err == nil {
		t.Fatalf("a session with no registry answered: %+v", reply)
	}
	if reply != nil {
		t.Fatalf("an error came back WITH a reply (%+v); a caller that forgets to "+
			"check err would render it as an empty session", reply)
	}
	if !strings.Contains(err.Error(), "did not answer") {
		t.Errorf("error is %q, want it to say nobody answered", err)
	}
}

// TestAStoppedRegistryGoesQuiet — a stopped actor must stop answering rather
// than serving a frozen list. A stale answer served confidently is the failure;
// a timeout is an honest "I do not know".
func TestAStoppedRegistryGoesQuiet(t *testing.T) {
	nc := newABLATestNATS(t)
	a := NewAgentFleetActor(nc, msg.DefaultCodecRegistry(), newFleetTestKV(t, nc))
	a.Start()
	registerFleet(t, nc, fleet.Fleet{Name: "epic-07"})

	a.Stop()

	if _, err := fleet.Ask(nc, fleet.Query{}, 300*time.Millisecond); err == nil {
		t.Fatal("a stopped registry still answered")
	}
	if _, err := fleet.Send(nc, fleet.Update{Op: fleet.OpRegister,
		Fleet: fleet.Fleet{Name: "epic-09"}}, 300*time.Millisecond); err == nil {
		t.Fatal("a stopped registry still accepted a registration")
	}
}

// TestTheRegistrySurvivesARestart is the reason it moved out of a JSON file
// nobody owned: a fleet, its board, its roadmap and every member's resumable
// session id must come back after the daemon does.
func TestTheRegistrySurvivesARestart(t *testing.T) {
	nc := newABLATestNATS(t)
	kv := newFleetTestKV(t, nc)

	first := NewAgentFleetActor(nc, msg.DefaultCodecRegistry(), kv)
	first.Start()
	registerFleet(t, nc, fleet.Fleet{
		Name: "epic-07", Source: "tracks/fleet/epic07.md", RoadmapDir: "new_roadmap/fleet"})
	if _, err := fleet.Send(nc, fleet.Update{Op: fleet.OpMemberUpsert, Name: "epic-07",
		Member: fleet.Member{PaneID: "p1", Label: "wkr-07", Role: "worker",
			SessionID: "resume-me"}}, fleetTestTimeout); err != nil {
		t.Fatalf("member upsert: %v", err)
	}
	first.Stop()

	// A new process over the same KV.
	second := NewAgentFleetActor(nc, msg.DefaultCodecRegistry(), kv)
	second.Start()
	t.Cleanup(second.Stop)

	reply, err := fleet.Ask(nc, fleet.Query{Name: "epic-07"}, fleetTestTimeout)
	if err != nil {
		t.Fatalf("Ask after restart: %v", err)
	}
	if len(reply.Fleets) != 1 {
		t.Fatalf("epic-07 did not come back: %+v", reply.Fleets)
	}
	f := reply.Fleets[0]
	if f.Source == "" || f.RoadmapDir == "" {
		t.Errorf("the fleet came back without its identity: %+v", f)
	}
	if len(f.Members) != 1 || f.Members[0].SessionID != "resume-me" {
		t.Fatalf("members did not survive: %+v — a lost session id is an agent "+
			"nobody can resume", f.Members)
	}
}

// TestForgettingAFleetRemovesItDurably: a forgotten fleet must not walk back in
// after a restart, which is what a delete that only touched memory would do.
func TestForgettingAFleetRemovesItDurably(t *testing.T) {
	nc := newABLATestNATS(t)
	kv := newFleetTestKV(t, nc)

	first := NewAgentFleetActor(nc, msg.DefaultCodecRegistry(), kv)
	first.Start()
	registerFleet(t, nc, fleet.Fleet{Name: "epic-07"})
	if _, err := fleet.Send(nc, fleet.Update{Op: fleet.OpForget, Name: "epic-07"}, fleetTestTimeout); err != nil {
		t.Fatalf("forget: %v", err)
	}
	first.Stop()

	second := NewAgentFleetActor(nc, msg.DefaultCodecRegistry(), kv)
	second.Start()
	t.Cleanup(second.Stop)

	reply, err := fleet.Ask(nc, fleet.Query{}, fleetTestTimeout)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if len(reply.Fleets) != 0 {
		t.Fatalf("a forgotten fleet came back after a restart: %+v", reply.Fleets)
	}
}

// TestAnUnreadableUpdateIsRefusedNotGuessed — the registry never invents an
// operation. A garbled write answered with "ok" would leave the caller believing
// a fleet exists.
func TestAnUnreadableUpdateIsRefusedNotGuessed(t *testing.T) {
	nc := newABLATestNATS(t)
	startFleetActor(t, nc, newFleetTestKV(t, nc))

	m, err := nc.Request(msg.FleetUpdateSubject(), []byte("{not json"), fleetTestTimeout)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	var reply fleet.UpdateReply
	if err := json.Unmarshal(m.Data, &reply); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if reply.OK || reply.Err == "" {
		t.Fatalf("an unreadable update was accepted: %+v", reply)
	}

	// And an unknown op is refused rather than treated as a register.
	if _, err := fleet.Send(nc, fleet.Update{Op: "delete-everything", Name: "x"}, fleetTestTimeout); err == nil {
		t.Fatal("an unknown op was accepted")
	}
}

// TestReconcileDropsMembersWhosePanesAreGone, and does it against a REAL
// workspace snapshot round trip — the same path the live actor uses, so a
// change to the snapshot shape fails here rather than in production.
func TestReconcileDropsMembersWhosePanesAreGone(t *testing.T) {
	nc := newABLATestNATS(t)
	codecs := msg.DefaultCodecRegistry()
	a := startFleetActor(t, nc, newFleetTestKV(t, nc))

	// A workspace where only p1 exists.
	serveAnsaSnapshot(t, nc, codecs, &domain.WorkspaceSnapshot{
		Tabs: []domain.TabSnapshot{{
			ID: "tab-1",
			Lanes: []domain.LaneSnapshot{{
				ID: "lane-1",
				PaneGroups: []domain.PaneGroupSnapshot{{
					ID:    "group-1",
					Panes: []domain.PaneSnapshot{{ID: "p1", GivenName: "wkr-07"}},
				}},
			}},
		}},
	})

	registerFleet(t, nc, fleet.Fleet{Name: "epic-07"})
	for _, id := range []string{"p1", "p-closed"} {
		if _, err := fleet.Send(nc, fleet.Update{Op: fleet.OpMemberUpsert, Name: "epic-07",
			Member: fleet.Member{PaneID: id}}, fleetTestTimeout); err != nil {
			t.Fatalf("member upsert %s: %v", id, err)
		}
	}

	a.ReconcileNow()

	reply, err := fleet.Ask(nc, fleet.Query{Name: "epic-07"}, fleetTestTimeout)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	members := reply.Fleets[0].Members
	if len(members) != 1 || members[0].PaneID != "p1" {
		t.Fatalf("roster after reconcile = %+v, want only the pane that exists", members)
	}
	if reply.ReconciledAt == 0 {
		t.Error("the answer does not say when membership was last checked; a reader " +
			"cannot tell a fresh roster from a stale one")
	}
}

// TestAFailedSnapshotLookupChangesNothing.
//
// THE MOST DANGEROUS PATH IN THIS ACTOR. Nothing answers the snapshot request,
// so the lookup fails — and a failure reported as "no panes exist" would empty
// every fleet's roster at once. The registry must be untouched, and it must not
// claim to have reconciled.
func TestAFailedSnapshotLookupChangesNothing(t *testing.T) {
	nc := newABLATestNATS(t)
	a := startFleetActor(t, nc, newFleetTestKV(t, nc)) // nobody serves ws.snapshot

	registerFleet(t, nc, fleet.Fleet{Name: "epic-07"})
	if _, err := fleet.Send(nc, fleet.Update{Op: fleet.OpMemberUpsert, Name: "epic-07",
		Member: fleet.Member{PaneID: "p1"}}, fleetTestTimeout); err != nil {
		t.Fatalf("member upsert: %v", err)
	}

	a.ReconcileNow()

	reply, err := fleet.Ask(nc, fleet.Query{Name: "epic-07"}, fleetTestTimeout)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if len(reply.Fleets[0].Members) != 1 {
		t.Fatalf("a failed lookup emptied the roster: %+v", reply.Fleets[0].Members)
	}
	if reply.ReconciledAt != 0 {
		t.Error("a failed reconcile stamped a timestamp: the answer now claims a " +
			"freshness it does not have")
	}
}
