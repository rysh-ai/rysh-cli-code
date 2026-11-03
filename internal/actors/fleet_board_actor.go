// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
	"github.com/rysh-ai/rysh-cli-code/internal/fleet"
	"github.com/rysh-ai/rysh-cli-code/internal/fleetboard"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// FleetBoardActor keeps the session-wide fleet board: one row per registered
// fleet (design 030 §9.2, founder ruling D-25, wave W2). The rows themselves
// live in internal/fleetboard; this file is the wiring.
//
// INPUTS, all read-only:
//  1. the fleet registry, asked over msg.FleetQuerySubject at start and every
//     registryInterval (the AFA's own membership poll is the same shape). Not
//     the fleet.update request stream: that carries refused requests too;
//  2. every board, through its own board.Subscribe with NO persistence — ABLA
//     already records every post, and recording twice would double the KV.
//
// OUTPUTS: rows in KV rysh-fleetboard-<session> (rebuilt from there at start,
// before the registry answers, so a restart does not depend on ABLA's lazy
// restore of named boards); answers on msg.FleetBoardQuerySubject; one
// fleetboard.ReplyRow per changed row on msg.FleetBoardUpdateSubject. No agent
// pane subscribes to either subject, so reading the fleet board wakes nobody.
//
// IT NEVER WRITES A BOARD. Door enforcement and tagging are W3, rendering W4.
type FleetBoardActor struct {
	nc     *nats.Conn
	codecs *msg.CodecRegistry

	mu      sync.Mutex
	board   *fleetboard.Board
	persist *fleetboard.Persistence
	started bool
	stopped bool

	sub      *board.Subscriber
	querySub *nats.Subscription
	done     chan struct{}
	wg       sync.WaitGroup

	// registryInterval is how often the registry is asked. A field so tests
	// need not wait 20 s.
	registryInterval time.Duration
}

const (
	fleetBoardRegistryInterval = 20 * time.Second
	fleetBoardRegistryTimeout  = 1500 * time.Millisecond
)

// NewFleetBoardActor builds the actor. A nil conn degrades to no fleet board;
// a nil kv to one that forgets on restart. Neither stops the session.
func NewFleetBoardActor(nc *nats.Conn, codecs *msg.CodecRegistry, kv nats.KeyValue) *FleetBoardActor {
	a := &FleetBoardActor{
		nc:               nc,
		codecs:           codecs,
		board:            fleetboard.New(),
		done:             make(chan struct{}),
		registryInterval: fleetBoardRegistryInterval,
	}
	if kv != nil {
		a.persist = fleetboard.NewPersistence(kv)
	}
	return a
}

// Receive makes it a root-spawned singleton beside the AFA (cmd/rysh).
func (a *FleetBoardActor) Receive(ctx actor.Context) {
	switch ctx.Message().(type) {
	case *actor.Started:
		if err := a.Start(); err != nil {
			slog.Warn("fleet board not started", "err", err)
		}
	case *actor.Stopping:
		a.Stop()
	}
}

// Start rebuilds the rows from storage, starts serving, then follows the
// boards and the registry. Calling it twice is a no-op.
func (a *FleetBoardActor) Start() error {
	a.mu.Lock()
	if a.started || a.stopped || a.nc == nil {
		a.mu.Unlock()
		return nil
	}
	a.started = true
	a.mu.Unlock()

	if rows, err := a.persist.LoadAll(); err != nil {
		slog.Warn("fleet board: stored rows could not be read; starting empty", "err", err)
	} else {
		a.mu.Lock()
		a.board.Load(rows)
		a.mu.Unlock()
	}

	qs, err := fleetboard.Serve(a.nc, a.rows)
	if err != nil {
		return err
	}
	// Its own tagger (W3): a fleet-board post with no valid door stamp is
	// marked Unverified and does not set any fleet's status.
	sub, err := board.SubscribeTagged(a.nc, a.codecs, 0, nil, newFleetBoardTagger(a.nc).tag)
	if err != nil {
		_ = qs.Unsubscribe()
		return err
	}
	a.mu.Lock()
	a.querySub, a.sub = qs, sub
	a.mu.Unlock()

	a.wg.Add(2)
	go a.followBoards(sub)
	go a.followRegistry()
	return nil
}

// Stop halts the actor. Idempotent.
func (a *FleetBoardActor) Stop() {
	a.mu.Lock()
	if a.stopped {
		a.mu.Unlock()
		return
	}
	a.stopped = true
	qs, sub := a.querySub, a.sub
	a.querySub, a.sub = nil, nil
	a.mu.Unlock()

	if qs != nil {
		_ = qs.Unsubscribe()
	}
	if sub != nil {
		sub.Close()
	}
	close(a.done)
	a.wg.Wait()
}

func (a *FleetBoardActor) rows() []fleetboard.Row {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.board.Rows()
}

func (a *FleetBoardActor) followBoards(sub *board.Subscriber) {
	defer a.wg.Done()
	events := sub.Events()
	for {
		select {
		case <-a.done:
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			if ev.Kind != board.EventPost || ev.Post == nil {
				continue
			}
			a.mu.Lock()
			row := a.board.ApplyPost(ev.Board, ev.Post)
			a.mu.Unlock()
			if row != nil {
				a.saveAndPublish(*row)
			}
		}
	}
}

func (a *FleetBoardActor) followRegistry() {
	defer a.wg.Done()
	t := time.NewTicker(a.registryInterval)
	defer t.Stop()
	for {
		a.syncRegistry()
		select {
		case <-a.done:
			return
		case <-t.C:
		}
	}
}

// syncRegistry applies one registry answer. No answer changes nothing: a
// registry that is busy or not up yet must not empty the board.
func (a *FleetBoardActor) syncRegistry() {
	reply, err := fleet.Ask(a.nc, fleet.Query{}, fleetBoardRegistryTimeout)
	if err != nil {
		return
	}
	a.mu.Lock()
	if a.stopped {
		a.mu.Unlock()
		return
	}
	changed, removed := a.board.ApplyRegistry(reply.Fleets, time.Now().UnixMilli())
	a.mu.Unlock()
	for _, r := range changed {
		a.saveAndPublish(r)
	}
	for _, name := range removed {
		if err := a.persist.Delete(name); err != nil {
			slog.Warn("fleet board: stored row not deleted", "fleet", name, "err", err)
		}
	}
}

func (a *FleetBoardActor) saveAndPublish(r fleetboard.Row) {
	if err := a.persist.Save(r); err != nil {
		slog.Warn("fleet board: row not stored", "fleet", r.Fleet, "err", err)
	}
	now := time.Now().UnixMilli()
	data, err := json.Marshal(fleetboard.ReplyRow{Row: r, Health: r.Health(now), Summary: r.Summary()})
	if err != nil {
		return
	}
	_ = a.nc.Publish(msg.FleetBoardUpdateSubject(), data)
}
