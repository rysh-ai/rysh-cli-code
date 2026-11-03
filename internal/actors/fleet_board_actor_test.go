// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/fleet"
	"github.com/rysh-ai/rysh-cli-code/internal/fleetboard"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Tests for the FleetBoardActor (design 030 §9.2, wave W2).

const (
	fbHead   = "f1f1f1f1-0000-0000-0000-000000000001"
	fbWorker = "f1f1f1f1-0000-0000-0000-000000000002"
)

// fakeFleetRegistry answers msg.FleetQuerySubject like the AFA does.
type fakeFleetRegistry struct {
	mu     sync.Mutex
	fleets []fleet.Fleet
	sub    *nats.Subscription
}

func startFakeFleetRegistry(t *testing.T, nc *nats.Conn, fleets []fleet.Fleet) *fakeFleetRegistry {
	t.Helper()
	r := &fakeFleetRegistry{fleets: fleets}
	sub, err := nc.Subscribe(msg.FleetQuerySubject(), func(m *nats.Msg) {
		r.mu.Lock()
		data, _ := json.Marshal(fleet.Reply{Fleets: r.fleets})
		r.mu.Unlock()
		_ = m.Respond(data)
	})
	if err != nil {
		t.Fatal(err)
	}
	r.sub = sub
	return r
}

func (r *fakeFleetRegistry) set(fleets []fleet.Fleet) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fleets = fleets
}

func (r *fakeFleetRegistry) stop() { _ = r.sub.Unsubscribe() }

func fbFleet() fleet.Fleet {
	return fleet.Fleet{Name: "epic-a", BoardID: "epic-a", State: fleet.StateUp, Members: []fleet.Member{
		{PaneID: fbHead, Role: "ceo"},
		{PaneID: fbWorker, Role: "worker", Parent: fbHead},
	}}
}

func newFleetBoardTestKV(t *testing.T, nc *nats.Conn) nats.KeyValue {
	t.Helper()
	js, err := nc.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	kv, err := js.CreateKeyValue(fleetboard.BucketConfig("t-" + strings.ToLower(t.Name())))
	if err != nil {
		t.Fatalf("CreateKeyValue: %v", err)
	}
	return kv
}

func fbRow(t *testing.T, nc *nats.Conn, name string) (fleetboard.ReplyRow, bool) {
	t.Helper()
	reply, err := fleetboard.Ask(nc, 2*time.Second)
	if err != nil {
		return fleetboard.ReplyRow{}, false
	}
	for _, r := range reply.Rows {
		if r.Fleet == name {
			return r, true
		}
	}
	return fleetboard.ReplyRow{}, false
}

func startFleetBoardActor(t *testing.T, nc *nats.Conn, kv nats.KeyValue) *FleetBoardActor {
	t.Helper()
	a := NewFleetBoardActor(nc, msg.DefaultCodecRegistry(), kv)
	a.registryInterval = 50 * time.Millisecond
	if err := a.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(a.Stop)
	return a
}

// The W2 headline (design 030 §9.5): after a restart the query returns the
// pre-restart rows without anyone touching a board. The registry stops
// answering before the second actor starts, so the row can only come from
// the fleet board's own storage.
func TestFleetBoardRowsSurviveARestart(t *testing.T) {
	nc := newABLATestNATS(t)
	kv := newFleetBoardTestKV(t, nc)
	reg := startFakeFleetRegistry(t, nc, []fleet.Fleet{fbFleet()})
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())

	a := startFleetBoardActor(t, nc, kv)
	ablaWaitFor(t, "the registry's fleet to get a row", func() bool {
		_, ok := fbRow(t, nc, "epic-a")
		return ok
	})
	if err := msg.SendBoardPost(pub, "epic-a",
		msg.NewBoardPost(fbHead, "ceo", "MILESTONE", "M1 shipped", time.Now().UnixMilli())); err != nil {
		t.Fatal(err)
	}
	ablaWaitFor(t, "the milestone to reach the row", func() bool {
		r, ok := fbRow(t, nc, "epic-a")
		return ok && r.LastStatus != nil && r.LastStatus.Text == "M1 shipped"
	})
	a.Stop()
	reg.stop()

	startFleetBoardActor(t, nc, kv)
	r, ok := fbRow(t, nc, "epic-a")
	if !ok {
		t.Fatal("after the restart the fleet board has no row for epic-a")
	}
	if r.LastStatus == nil || r.LastStatus.Kind != "milestone" || r.LastStatus.Text != "M1 shipped" ||
		r.Head == nil || r.State != fleet.StateUp || r.Members != 2 {
		t.Fatalf("after the restart the row is %+v", r)
	}
	if r.Summary != "MILESTONE: M1 shipped" || r.Health != fleetboard.HealthActive {
		t.Fatalf("summary %q health %q", r.Summary, r.Health)
	}
}

// A forgotten fleet loses its row (and its stored key), and every change is
// published on the update subject.
func TestFleetBoardFollowsTheRegistryAndPublishesChanges(t *testing.T) {
	nc := newABLATestNATS(t)
	kv := newFleetBoardTestKV(t, nc)
	reg := startFakeFleetRegistry(t, nc, []fleet.Fleet{fbFleet()})

	var mu sync.Mutex
	var updates []string
	sub, err := nc.Subscribe(msg.FleetBoardUpdateSubject(), func(m *nats.Msg) {
		var r fleetboard.ReplyRow
		if json.Unmarshal(m.Data, &r) == nil {
			mu.Lock()
			updates = append(updates, r.Fleet+":"+r.State)
			mu.Unlock()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Unsubscribe()

	startFleetBoardActor(t, nc, kv)
	ablaWaitFor(t, "the row to be published", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(updates) > 0 && updates[0] == "epic-a:up"
	})

	reg.set(nil)
	ablaWaitFor(t, "the forgotten fleet's row to go", func() bool {
		_, ok := fbRow(t, nc, "epic-a")
		return !ok
	})
	if _, err := kv.Get("fb-epic-a"); err != nats.ErrKeyNotFound {
		t.Fatalf("the forgotten fleet's stored row is still there: %v", err)
	}
}

// The actor reads boards and never writes one: a post on the fleet's board
// reaches the row, and no post is published by the actor itself.
func TestFleetBoardNeverPostsToABoard(t *testing.T) {
	nc := newABLATestNATS(t)
	kv := newFleetBoardTestKV(t, nc)
	startFakeFleetRegistry(t, nc, []fleet.Fleet{fbFleet()})

	var mu sync.Mutex
	posts := 0
	sub, err := nc.Subscribe(msg.T("board", ">"), func(*nats.Msg) { mu.Lock(); posts++; mu.Unlock() })
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Unsubscribe()

	startFleetBoardActor(t, nc, kv)
	ablaWaitFor(t, "a row", func() bool { _, ok := fbRow(t, nc, "epic-a"); return ok })
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if posts != 0 {
		t.Fatalf("the fleet board actor published %d board messages", posts)
	}
}
