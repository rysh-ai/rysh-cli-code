package actors

import (
	"sync"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// ABLA — the AgentBoardListenerActor. One per session, always on.
//
// WHY IT EXISTS. Before this actor, the ONLY board subscriber and the ONLY
// writer to the board KV lived in the TUI (internal/tui/board_view.go,
// setupBoardSubscriptions). So the board heard nothing and recorded nothing
// while no TUI had a board open. That is not a corner case: it is the normal
// state of a session. It was proved live — two named panes registered their
// personas and the board showed "1 agent", because every announcement was
// published before any board existed to hear it.
//
// THE SEPARATION, and it is the whole point: ABLA COLLECTS, THE TUI READS.
// ABLA owns the subscription that writes and it owns the KV. A TUI subscribes
// read-only and restores history read-only; it must never be handed a
// persister again. Blurring that line puts the session's memory back inside a
// process that may not be running.
//
// WHY THE TUI CANNOT SIMPLY SHARE THIS ACTOR'S STORE. The TUI is a SEPARATE
// PROCESS from the daemon that hosts this actor — `rysh attach` connects over
// NATS, it does not link against the daemon. An in-process store handle cannot
// cross that boundary, so "the view reads ABLA's Store" is not available as an
// option here. The view therefore keeps its own read-only subscription fed by
// the same subjects, plus a read-only restore from the same KV. That is not a
// preference between two defensible designs; the process boundary decides it.
//
// WHAT THIS ACTOR IS NOT. It is not the board's query surface, it does not
// render, and it does not know what a pane is. It subscribes, it records, it
// keeps an authoritative in-memory copy for whatever asks next.

// AgentBoardListenerActor is the session-scoped board collector.
//
// It is spawned at the actor-system root with the session (see cmd/rysh), NOT
// under a workspace, NOT with a pane and NOT with the TUI: it has to be
// listening before anything exists that could post.
type AgentBoardListenerActor struct {
	nc     *nats.Conn
	codecs *msg.CodecRegistry
	kv     nats.KeyValue

	mu      sync.Mutex
	started bool
	stopped bool
	store   *board.Store
	persist *board.Persistence
	sub     *board.Subscriber
	// aliveSub answers liveness requests while this actor is listening.
	aliveSub *nats.Subscription
	done    chan struct{}
}

// NewAgentBoardListenerActor builds the collector. A nil conn or a nil kv is
// tolerated: the actor degrades (to no feed, or to a live-only board) rather
// than taking the session down with it. agents-board is a monitoring view, and
// a monitoring view that stops a session from starting is worse than no view.
func NewAgentBoardListenerActor(nc *nats.Conn, codecs *msg.CodecRegistry, kv nats.KeyValue) *AgentBoardListenerActor {
	return &AgentBoardListenerActor{
		nc:     nc,
		codecs: codecs,
		kv:     kv,
		store:  board.New(0),
		done:   make(chan struct{}),
	}
}

// Receive is the proto.actor entry point.
func (a *AgentBoardListenerActor) Receive(ctx actor.Context) {
	switch ctx.Message().(type) {
	case *actor.Started:
		a.Start()
	case *actor.Stopping:
		a.Stop()
	}
}

// Start restores history, then subscribes, then pumps the live feed into the
// store. It is IDEMPOTENT: a second call is a no-op.
//
// SINGLE-WRITER IS A CORRECTNESS REQUIREMENT, NOT TIDINESS. Two board
// Persistence instances each keep their own arrival ordinal, so both write
// post-...0001 and the second Put OVERWRITES the first. A second writer does
// not duplicate history, it DESTROYS it — silently, with nothing reporting a
// loss (TestASecondWriterDestroysHistory pins the behaviour). That is why this
// method is idempotent, why the TUI passes a nil persister, and why the call
// site uses SpawnNamed: a second spawn under the same name fails rather than
// quietly starting a second writer.
//
// Errors are swallowed on purpose and reported through Err(): there is no
// caller that could do anything useful with them, and the alternative is a
// session that refuses to start because a monitoring view could not subscribe.
func (a *AgentBoardListenerActor) Start() {
	a.mu.Lock()
	if a.started || a.stopped || a.nc == nil {
		a.mu.Unlock()
		return
	}
	a.started = true
	store := a.store
	persist := board.NewPersistence(a.kv)
	a.persist = persist
	a.mu.Unlock()

	// RESTORE BEFORE SUBSCRIBE. History first, then the live tail; the other
	// order interleaves restored posts among live ones and drops yesterday's
	// milestones into the middle of today's stream. It also continues the
	// persistence ordinal, so this process cannot overwrite keys the previous
	// one wrote.
	_, _, _ = persist.Restore(store)

	sub, err := board.Subscribe(a.nc, a.codecs, board.DefaultBufferSize, persist)
	if err != nil {
		return
	}

	a.mu.Lock()
	a.sub = sub
	done := a.done
	a.mu.Unlock()

	// Answer "are you recording?" for as long as this actor is listening. The
	// subscription is torn down with the actor, so a dead recorder does not
	// reply and a caller's timeout is the honest answer rather than a stale
	// artifact's.
	if aliveSub, aerr := a.nc.Subscribe(msg.BoardAliveSubject(), func(m *nats.Msg) {
		_ = m.Respond([]byte(msg.BoardAliveReply))
	}); aerr == nil {
		a.mu.Lock()
		a.aliveSub = aliveSub
		a.mu.Unlock()
	}

	go a.pump(sub, store, done)
}

// pump drains the live feed into the store. The subscriber has already written
// each message to the KV by the time it appears here, so this goroutine falling
// behind costs an in-memory update and never the record.
func (a *AgentBoardListenerActor) pump(sub *board.Subscriber, store *board.Store, done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		case ev := <-sub.Events():
			store.ApplyEvent(ev)
		}
	}
}

// Stop unsubscribes and halts the pump. Idempotent.
// unsubscribeAlive stops answering liveness requests. Called from Stop so a
// stopped recorder goes quiet rather than continuing to claim it records.
func (a *AgentBoardListenerActor) unsubscribeAlive() {
	if a.aliveSub != nil {
		_ = a.aliveSub.Unsubscribe()
		a.aliveSub = nil
	}
}

func (a *AgentBoardListenerActor) Stop() {
	a.mu.Lock()
	if a.stopped {
		a.mu.Unlock()
		return
	}
	a.stopped = true
	sub := a.sub
	done := a.done
	a.unsubscribeAlive()
	a.mu.Unlock()

	if done != nil {
		close(done)
	}
	if sub != nil {
		sub.Close()
	}
}

// Store is the session's authoritative in-memory board. Exposed for callers in
// this process; a TUI in another process cannot use it (see the file header)
// and reads the subjects and the KV instead.
func (a *AgentBoardListenerActor) Store() *board.Store { return a.store }

// Dropped reports how many live events the render buffer refused. They are not
// lost — the KV write is unconditional — so this is "not applied to the
// in-memory copy", recoverable by restoring from the KV.
func (a *AgentBoardListenerActor) Dropped() uint64 {
	a.mu.Lock()
	sub := a.sub
	a.mu.Unlock()
	if sub == nil {
		return 0
	}
	return sub.Dropped()
}

// WriteErrors reports KV writes that failed. Non-zero means the board is
// live-only for those messages: delivered, but they will not survive a restart.
func (a *AgentBoardListenerActor) WriteErrors() uint64 {
	a.mu.Lock()
	sub := a.sub
	a.mu.Unlock()
	if sub == nil {
		return 0
	}
	return sub.WriteErrors()
}

// Listening reports whether the subscription is live. Used by tests and by
// anything that wants to say "the board is not recording" out loud rather than
// letting it be silently false.
func (a *AgentBoardListenerActor) Listening() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sub != nil && !a.stopped
}
