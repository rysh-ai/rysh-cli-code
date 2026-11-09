// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"sync"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// ANSA dispatch — routing a message without stopping the session.
//
// A route is slow by design. It enumerates the session's panes (a snapshot
// request per tab), probes the target (up to ansaProbeTimeout), and then
// delivers and confirms the submit, which waits ansaSubmitRecheck. About two
// seconds per message, every one of them on purpose.
//
// It used to run inside the WorkspaceActor's Receive. The workspace actor is
// the session's coordinator, so every route blocked EVERYTHING addressed to
// ws.inbox — ## commands, snapshots, every other agent's message — for those
// two seconds. Measured with 30 agents sending at once (2026-11-09): the
// deliveries completed at 2, 4, 6 … 28 s, strictly one after another, and 16 of
// the 30 senders hit the CLI's 30 s timeout and were told their message failed.
// Each of those requests then sat in the actor's unbounded mailbox and was
// still delivered later, after its sender had given up.
//
// Now the actor resolves nothing slow itself: it copies its tab ids and hands
// the route to a worker, which replies to the caller when it finishes. Two
// rules keep that safe:
//
//   - ONE DELIVERY AT A TIME PER TARGET PANE. A delivery clears the target's
//     composer, types, and submits; two of them interleaving on one pane would
//     garble both. Routes to different panes run in parallel; routes to the
//     same pane queue on that pane's lane, in arrival order as far as the
//     mutex gives it (Go mutexes are not FIFO under contention, so strict
//     arrival order between two senders racing for the same pane is not
//     promised — it never was: they raced for the mailbox before).
//   - NO LATE DELIVERY. A route that waited on its lane past ansaRouteDeadline
//     is refused with AnsaErrExpired instead of delivered, because its sender
//     has stopped listening.
//
// The number of routes in flight is capped (ansaRouteSlots). Excess routes wait
// for a slot inside their own goroutine — never in the actor — and fall under
// the same deadline.

const (
	// ansaRouteDeadline is how long a route may wait for its turn before it is
	// refused. Below the CLI's 30 s request timeout (internal/cli
	// defaultTimeout) with room for the route itself (~2 s) and the reply.
	ansaRouteDeadline = 25 * time.Second

	// ansaRouteSlots caps routes in flight. Each holds a few NATS requests at a
	// time; sixteen is far above what one session's agents send concurrently
	// and well below anything that strains the bus.
	ansaRouteSlots = 16
)

// ansaDispatcher owns the per-target lanes and the slot pool. Safe for
// concurrent use.
type ansaDispatcher struct {
	mu    sync.Mutex
	lanes map[string]*ansaLane
	slots chan struct{}
	// now is the clock; a seam for tests.
	now func() time.Time
}

// ansaLane serialises deliveries to one pane. refs counts holders and waiters
// so an idle lane is dropped rather than kept for every pane ever addressed.
type ansaLane struct {
	mu   sync.Mutex
	refs int
}

func newAnsaDispatcher(slots int) *ansaDispatcher {
	if slots <= 0 {
		slots = ansaRouteSlots
	}
	return &ansaDispatcher{
		lanes: map[string]*ansaLane{},
		slots: make(chan struct{}, slots),
		now:   time.Now,
	}
}

// lock takes paneID's lane and returns its release. Blocks while another
// delivery to the same pane is in progress.
func (d *ansaDispatcher) lock(paneID string) (unlock func()) {
	d.mu.Lock()
	l := d.lanes[paneID]
	if l == nil {
		l = &ansaLane{}
		d.lanes[paneID] = l
	}
	l.refs++
	d.mu.Unlock()

	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		d.mu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(d.lanes, paneID)
		}
		d.mu.Unlock()
	}
}

// laneCount is the number of lanes held or waited on. For tests.
func (d *ansaDispatcher) laneCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.lanes)
}

// spawn runs fn on its own goroutine once a slot is free. It never blocks the
// caller, which is the point: the caller is the workspace actor.
func (d *ansaDispatcher) spawn(fn func()) {
	go func() {
		d.slots <- struct{}{}
		defer func() { <-d.slots }()
		fn()
	}()
}

// route resolves `to` at the edge and routes the message through r, holding
// the target's lane for the delivery. accepted is when the request arrived;
// the zero time means "no deadline" (a synchronous caller that is itself still
// waiting, such as a human's ##ansa send).
//
// r must be safe to use from the calling goroutine: the workspace actor passes
// a detached router (ansaRouterDetached) when calling from a worker.
func (d *ansaDispatcher) route(r *ansaRouter, accepted time.Time, from, to, mode, text string) *msg.MsgAnsaRouteResult {
	panes, err := r.tr.Panes()
	if err != nil {
		return msg.AnsaRefusal(msg.AnsaErrDirectory,
			"cannot enumerate the session's panes, so %q can be neither found nor ruled out: %v", to, err)
	}

	// THE EDGE. @name becomes a pane uuid here and nowhere deeper.
	target, refusal := ansaResolveTarget(panes, to)
	if refusal != nil {
		return refusal
	}

	unlock := d.lock(target.ID)
	defer unlock()

	if !accepted.IsZero() {
		if waited := d.now().Sub(accepted); waited > ansaRouteDeadline {
			return msg.AnsaRefusal(msg.AnsaErrExpired,
				"pane %s (%s) was busy with earlier messages for %s, longer than the sender waits; nothing was delivered — send it again",
				target.ID, ansaPersona(target), waited.Round(time.Second))
		}
	}

	return r.Route(msg.NewAnsaRoute(from, target.ID, mode, text))
}
