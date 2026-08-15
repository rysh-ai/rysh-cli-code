// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/fleet"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// AFA — the AgentFleetActor. One per session, always on (design 028 §6.5).
//
// WHY IT EXISTS. Until now a fleet was a JSON file in `.rysh/fleet/`, written by
// whichever `fleetctl` process ran last, shared by every fleet in the workspace
// and tracked by nothing. Two failures are on record from live runs: a sibling
// fleet's teardown DELETED a live fleet's manifest, which took the tool down for
// every fleet at once; and `fleetctl worktree` reported `created: true` for a
// directory that did not exist. Both are the same class — the registry was a
// file anybody could clobber and nobody owned — and both get linearly worse as
// the session goes from one fleet to twenty-five.
//
// WHAT IT OWNS: the list of fleets, their membership, and the fleet ↔ board
// mapping. It is the authority; the manifest is demoted to a cache.
//
// WHAT IT DOES NOT OWN, and this boundary is the reason it stays small: it
// creates no panes, starts no claude, makes no worktrees and delivers no
// messages. `fleetctl` still does all of that. An actor that grew those would
// be a second implementation of the driver, in a language where the driver is
// harder to iterate on.
//
// IT IS ALSO NOT THE BOARD'S LIFECYCLE. Design 028 §6.5 gave it "spawn and stop
// the fleet's ABLA child"; there is no such child — E-39 shipped ONE recorder
// that hears every board through a wildcard, so a fleet's board needs nothing
// spawned and nothing torn down. What remains is the mapping, which is what
// `Fleet.BoardID` is.

// afaReconcileInterval is how often membership is checked against the panes
// that actually exist.
//
// It is a POLL and it is not apologised for: pane deletion has no event this
// actor can subscribe to, and the alternative — reconciling inside the query
// handler — is a deadlock (see fleet.ServeQueries). The number decides only how
// stale an answer can be, and every answer reports its own age, so a reader is
// never misled about which it got.
const afaReconcileInterval = 20 * time.Second

// afaSnapshotTimeout bounds the who-is-alive lookup.
//
// Well under the caller's budget for the same reason ABLA's is: a slow
// workspace must degrade the FRESHNESS of an answer, never turn a healthy
// session into "the registry did not answer".
const afaSnapshotTimeout = 1500 * time.Millisecond

// AgentFleetActor is the session-scoped fleet registry.
//
// Spawned at the actor-system root with the session (see cmd/rysh), NOT under a
// workspace: `fleetctl` talks to it before any fleet exists, and it must
// outlive every workspace switch.
type AgentFleetActor struct {
	nc     *nats.Conn
	codecs *msg.CodecRegistry
	kv     nats.KeyValue

	mu      sync.Mutex
	started bool
	stopped bool

	reg     *fleet.Registry
	persist *fleet.Persistence

	querySub  *nats.Subscription
	updateSub *nats.Subscription
	done      chan struct{}

	// reconciledAt is unix millis of the last successful membership check. Read
	// from the query goroutine, written by the reconcile loop.
	reconciledAt atomic.Int64
}

// NewAgentFleetActor builds the registry actor. A nil conn or nil kv is
// tolerated: it degrades to no registry, or to one that forgets on restart,
// rather than taking the session down. The same rule the board's actors follow
// — a session must start even when a coordination surface cannot.
func NewAgentFleetActor(nc *nats.Conn, codecs *msg.CodecRegistry, kv nats.KeyValue) *AgentFleetActor {
	return &AgentFleetActor{
		nc:     nc,
		codecs: codecs,
		kv:     kv,
		reg:    fleet.New(),
		done:   make(chan struct{}),
	}
}

// Receive is the proto.actor entry point.
func (a *AgentFleetActor) Receive(ctx actor.Context) {
	switch ctx.Message().(type) {
	case *actor.Started:
		a.Start()
	case *actor.Stopping:
		a.Stop()
	}
}

// Start restores the registry, then serves it. IDEMPOTENT: a second call is a
// no-op.
//
// RESTORE BEFORE SUBSCRIBE, the same ordering ABLA uses and for a sharper
// reason here: a `fleetctl` that asks "does fleet epic-07 exist?" during the
// window between subscribing and restoring would be told no, and would stand up
// a second one over the top of the first.
func (a *AgentFleetActor) Start() {
	a.mu.Lock()
	if a.started || a.stopped || a.nc == nil {
		a.mu.Unlock()
		return
	}
	a.started = true
	a.persist = fleet.NewPersistence(a.kv)
	reg, persist, done := a.reg, a.persist, a.done
	a.mu.Unlock()

	_, _ = persist.Restore(reg)

	if sub, err := fleet.ServeQueries(a.nc, reg, a.reconciledAt.Load); err == nil {
		a.mu.Lock()
		a.querySub = sub
		a.mu.Unlock()
	}

	// Every accepted write is persisted immediately rather than on a timer.
	// A fleet recorded in memory and lost on restart is the failure this actor
	// exists to fix, and a flush interval is just a smaller window for it.
	if sub, err := fleet.ServeUpdates(a.nc, reg, func(op string, f *fleet.Fleet, name string) {
		switch op {
		case fleet.OpForget:
			_ = persist.Delete(name)
		default:
			if f != nil {
				_ = persist.Save(f)
			}
		}
	}); err == nil {
		a.mu.Lock()
		a.updateSub = sub
		a.mu.Unlock()
	}

	go a.reconcileLoop(done)
}

// reconcileLoop drops members whose panes have gone.
//
// ON ITS OWN GOROUTINE AND ON ITS OWN CLOCK — never inside the query handler.
// The workspace is a caller of that handler (`##fleet list` runs inside it), and
// this loop asks the workspace for a snapshot; doing both in one call path is a
// deadlock broken only by a timeout, which would look like a dead registry on a
// perfectly healthy session.
func (a *AgentFleetActor) reconcileLoop(done <-chan struct{}) {
	t := time.NewTicker(afaReconcileInterval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			a.reconcileOnce()
		}
	}
}

// reconcileOnce checks membership against the live panes. Exported behaviour is
// in Registry.Reconcile; what is decided here is what happens when the lookup
// FAILS, and the answer is nothing at all: no drop, and no timestamp update, so
// the staleness a reader sees is honest.
func (a *AgentFleetActor) reconcileOnce() {
	live, err := a.livePaneIDs()
	if err != nil || len(live) == 0 {
		return
	}
	a.reg.Reconcile(live)
	a.reconciledAt.Store(time.Now().UnixMilli())

	// Persist what changed. Cheap: a session holds tens of fleets, not
	// thousands, and the alternative is a registry whose in-memory truth and
	// durable copy disagree after every closed pane.
	a.mu.Lock()
	persist := a.persist
	a.mu.Unlock()
	for _, f := range a.reg.List() {
		copyOf := f
		_ = persist.Save(&copyOf)
	}
}

// livePaneIDs asks the workspace which panes exist right now.
//
// The same round trip ABLA makes, and legal for the same reason: this actor is
// spawned at the ACTOR-SYSTEM ROOT, not under the workspace, so this is one
// actor asking another rather than the workspace asking itself. It runs on this
// actor's own goroutine, so a slow workspace delays a reconcile and never the
// registry's answers.
//
// The filter is domain.PaneCanHostAnAgent — the same predicate the board's
// roster uses. A fleet member lives in a pane that can host an agent; a board
// pane or an approval pane cannot be one, and if the two surfaces disagreed
// about that they would disagree about who exists.
func (a *AgentFleetActor) livePaneIDs() (map[string]bool, error) {
	a.mu.Lock()
	nc, codecs := a.nc, a.codecs
	a.mu.Unlock()
	if nc == nil {
		return nil, fmt.Errorf("afa: no bus, cannot ask which panes are alive")
	}

	pub := msg.NewNATSPublisher(nc, codecs)
	reply, err := pub.Request(
		msg.T("ws", "snapshot"),
		&msg.MsgGetWorkspaceSnapshot{LayoutOnly: true, NoHistories: true},
		afaSnapshotTimeout,
	)
	if err != nil {
		// An ERROR, never an empty set: Registry.Reconcile treats empty as
		// "caller does not know" and retains everything, and collapsing the two
		// here would empty every fleet's roster the first time the workspace was
		// busy.
		return nil, err
	}
	snapReply, ok := reply.(*msg.MsgWorkspaceSnapshotReply)
	if !ok {
		return nil, fmt.Errorf("afa: unexpected reply %T from the workspace snapshot", reply)
	}

	live := make(map[string]bool)
	for p := range domain.PanesInWorkspace(&snapReply.Snapshot) {
		if !domain.PaneCanHostAnAgent(p.PaneType) {
			continue
		}
		live[p.ID] = true
	}
	return live, nil
}

// Stop unsubscribes and halts the reconcile loop. Idempotent.
//
// A stopped registry GOES QUIET rather than serving a frozen list, for the
// reason the board's recorder does: a caller's timeout is an honest "I do not
// know", while a stale answer served confidently is not.
func (a *AgentFleetActor) Stop() {
	a.mu.Lock()
	if a.stopped {
		a.mu.Unlock()
		return
	}
	a.stopped = true
	done := a.done
	if a.querySub != nil {
		_ = a.querySub.Unsubscribe()
		a.querySub = nil
	}
	if a.updateSub != nil {
		_ = a.updateSub.Unsubscribe()
		a.updateSub = nil
	}
	a.mu.Unlock()

	if done != nil {
		close(done)
	}
}

// Registry is the live registry. Exposed for callers in this process; a TUI or
// CLI in another process asks over msg.FleetQuerySubject instead.
func (a *AgentFleetActor) Registry() *fleet.Registry { return a.reg }

// Listening reports whether the registry is serving.
func (a *AgentFleetActor) Listening() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.querySub != nil && !a.stopped
}

// ReconcileNow runs one membership check immediately. For tests and for the
// caller that has just torn a fleet down and wants the roster to reflect it
// without waiting out the interval.
func (a *AgentFleetActor) ReconcileNow() { a.reconcileOnce() }
