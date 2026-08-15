// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
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

	// ONE ACTOR, MANY BOARDS (design 028). A session may hold several boards —
	// one per fleet, plus the session board — and this actor records all of
	// them from one wildcard subscription.
	//
	// WHY A MAP IN ONE ACTOR RATHER THAN ONE ACTOR PER BOARD, which is what
	// design 028 §6.3 first proposed: the invariant to protect is ONE WRITER
	// PER BOARD, because two Persistence values over one board both write
	// post-…0001 and the second overwrites the first. A map owned by a single
	// actor makes a second writer unconstructible, which is strictly stronger
	// than a SpawnNamed collision that merely fails loudly — and it needs no
	// supervisor, no lifecycle message, and nothing to tell it a new board
	// exists. §6.3 is amended to match.
	stores   map[string]*board.Store
	persists map[string]*board.Persistence
	// restored marks boards whose history has been replayed into their store.
	restored map[string]bool

	sub       *board.Subscriber
	restoreMu sync.Mutex
	// aliveSubs answer liveness requests while this actor is listening — the
	// default board's subject and the wildcard that covers every named one.
	aliveSubs []*nats.Subscription
	// querySub answers READ requests — `rysh board tail` — while this actor is
	// listening. Same lifetime as aliveSub and for the same reason: a stopped
	// recorder must go quiet, so that a reader's timeout is an honest "I do not
	// know" instead of a confidently empty board.
	querySubs []*nats.Subscription
	done      chan struct{}
}

// NewAgentBoardListenerActor builds the collector. A nil conn or a nil kv is
// tolerated: the actor degrades (to no feed, or to a live-only board) rather
// than taking the session down with it. agents-board is a monitoring view, and
// a monitoring view that stops a session from starting is worse than no view.
func NewAgentBoardListenerActor(nc *nats.Conn, codecs *msg.CodecRegistry, kv nats.KeyValue) *AgentBoardListenerActor {
	return &AgentBoardListenerActor{
		nc:       nc,
		codecs:   codecs,
		kv:       kv,
		stores:   map[string]*board.Store{},
		persists: map[string]*board.Persistence{},
		restored: map[string]bool{},
		done:     make(chan struct{}),
	}
}

// storeFor returns the in-memory board for an id, creating it on first sight.
//
// Creation is lazy because a board is not announced to anybody: it exists as
// soon as a message is addressed to it. That is what makes `##board open
// --board <id>` work with no fleet layer, no registry and no restart — the
// recorder is already listening to every board id there could ever be.
func (a *AgentBoardListenerActor) storeFor(id string) *board.Store {
	id = msg.NormalizeBoardID(id)
	a.mu.Lock()
	defer a.mu.Unlock()
	if s := a.stores[id]; s != nil {
		return s
	}
	s := board.New(0)
	a.stores[id] = s
	return s
}

// persistFor returns the SINGLE writer for a board, creating it on first sight.
//
// The map is the invariant: there is exactly one *Persistence per board id in
// this process, so the ordinal that names its keys cannot be duplicated. A
// newly created one primes its ordinal from the KV before its first write
// (board.Persistence.prime), so a board with history does not restart at 1 and
// overwrite it.
func (a *AgentBoardListenerActor) persistFor(id string) *board.Persistence {
	id = msg.NormalizeBoardID(id)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.kv == nil {
		return nil
	}
	if p, ok := a.persists[id]; ok {
		return p
	}
	p := board.NewPersistence(a.kv, id)
	a.persists[id] = p
	return p
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
// loss (TestTwoWritersThatBothPrimedFirstStillDestroyHistory pins what survives
// of that hazard after 028 primed the ordinal). That is why this
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
	a.mu.Unlock()

	// RESTORE BEFORE SUBSCRIBE. History first, then the live tail; the other
	// order interleaves restored posts among live ones and drops yesterday's
	// milestones into the middle of today's stream. It also continues the
	// persistence ordinal, so this process cannot overwrite keys the previous
	// one wrote.
	//
	// Only the SESSION board is restored here, because it is the only board
	// known to exist before a message names one. Every other board is restored
	// the first time it is heard from or read (ensureBoard), under a lock that
	// keeps history ahead of the live tail for that board too.
	a.ensureBoard(msg.DefaultBoardID)

	sub, err := board.Subscribe(a.nc, a.codecs, board.DefaultBufferSize, a.persistFor)
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
	//
	// Both shapes: the session board's subject, and the wildcard that answers
	// for every named board. ONE RECORDER ANSWERS FOR ALL OF THEM and that is
	// honest — this actor hears every board through one wildcard subscription,
	// so "am I recording board X" has the same answer for every X.
	for _, subject := range []string{msg.BoardAliveSubject(msg.DefaultBoardID), msg.BoardAlivePattern()} {
		if aliveSub, aerr := a.nc.Subscribe(subject, func(m *nats.Msg) {
			_ = m.Respond([]byte(msg.BoardAliveReply))
		}); aerr == nil {
			a.mu.Lock()
			a.aliveSubs = append(a.aliveSubs, aliveSub)
			a.mu.Unlock()
		}
	}

	// Answer "what is ON the board?" from the authoritative in-memory store,
	// for as long as this actor is listening.
	//
	// THIS ACTOR IS THE BOARD'S READ PATH AND NOTHING ELSE MAY BE. The header
	// above says ABLA "is not the board's query surface"; that was true when
	// the only reader was a TUI in another process, which reads the subjects
	// and the KV because a store handle cannot cross a process boundary. A
	// short-lived CLI has no such constraint — it can ask — and asking is
	// strictly better than a second call site deriving the KV bucket name,
	// which is precisely how F-23 opened rysh-board-default while the daemon
	// wrote rysh-board-<session> and failed LOOKING HEALTHY.
	//
	// The resolver is ensureBoard, not a bare map read: a query is very often
	// the FIRST thing that touches a board after a restart (`rysh board tail
	// --board epic-07` before that fleet has posted again), and answering it
	// from an unrestored store would report an empty board that has history
	// sitting in the KV — the F-23 shape, where the failure looks like health.
	if querySubs, qerr := board.ServeQueries(a.nc, a.ensureBoard, a.livePaneIDs); qerr == nil {
		a.mu.Lock()
		a.querySubs = querySubs
		a.mu.Unlock()
	}

	go a.pump(sub, done)
}

// ensureBoard returns a board's store with its history already replayed.
//
// The lock is held ACROSS the restore, not just around the flag, so a second
// caller waits for history rather than being handed a half-filled store. That
// is the same "restore before the live tail" rule Start applies to the session
// board, extended to a board discovered mid-session — the pump calls this
// before applying an event, so a board's first live post can never land ahead
// of the history it belongs after.
func (a *AgentBoardListenerActor) ensureBoard(id string) *board.Store {
	id = msg.NormalizeBoardID(id)

	a.restoreMu.Lock()
	defer a.restoreMu.Unlock()

	store := a.storeFor(id)

	a.mu.Lock()
	done := a.restored[id]
	a.mu.Unlock()
	if done {
		return store
	}

	_, _, _ = a.persistFor(id).Restore(store)

	a.mu.Lock()
	a.restored[id] = true
	a.mu.Unlock()
	return store
}

// ablaSnapshotTimeout bounds the who-is-alive lookup that reconciles the roster
// on a board query (F-26).
//
// IT IS DELIBERATELY WELL UNDER THE CLI'S OWN BUDGET. `rysh board tail` waits
// cli.boardQueryTimeout (5s) for the whole answer, and this lookup happens
// INSIDE that window — so a long timeout here would spend the caller's budget
// and turn a slow workspace into "the recorder did not answer", which is the
// one message that surface must never say untruthfully. Reconciliation is an
// improvement to the answer, not a precondition for having one: if the
// workspace cannot say who is alive quickly, the roster is served as recorded
// and marked unreconciled.
const ablaSnapshotTimeout = 1500 * time.Millisecond

// livePaneIDs asks the workspace which panes exist right now.
//
// WHY A ROOT-SPAWNED ACTOR MAY ASK THIS AT ALL. workspace_ansa.go:23-30 warns
// that the workspace's own doors must not round-trip through an actor that
// asks the workspace for a snapshot: the workspace would be blocked in Receive
// waiting on a reply only it can serve. ABLA is spawned at the ACTOR-SYSTEM
// ROOT (cmd/rysh/main.go), not under the workspace, so this is one actor asking
// another and no self-request exists. The handler also runs on the NATS
// subscription goroutine rather than in ABLA's mailbox, so a slow workspace
// delays a board query and never the recording.
//
// The agent-hosting filter is domain.PaneCanHostAnAgent — the SAME predicate
// the TUI's seedRosterFromSnapshot uses. One rule, two surfaces: an approval,
// replay or board pane cannot hold an agent, so it can hold no roster entry
// either, and both surfaces must agree about that or they disagree about who
// exists — which is the defect being fixed.
func (a *AgentBoardListenerActor) livePaneIDs() (map[string]bool, error) {
	a.mu.Lock()
	nc := a.nc
	codecs := a.codecs
	a.mu.Unlock()
	if nc == nil {
		return nil, fmt.Errorf("abla: no bus, cannot ask which panes are alive")
	}

	pub := msg.NewNATSPublisher(nc, codecs)
	reply, err := pub.Request(
		msg.T("ws", "snapshot"),
		&msg.MsgGetWorkspaceSnapshot{LayoutOnly: true, NoHistories: true},
		ablaSnapshotTimeout,
	)
	if err != nil {
		// Returned as an ERROR, never as an empty set. See board.LivePanesFunc:
		// an empty set here would delete the entire roster.
		return nil, err
	}
	snapReply, ok := reply.(*msg.MsgWorkspaceSnapshotReply)
	if !ok {
		return nil, fmt.Errorf("abla: unexpected reply %T from the workspace snapshot", reply)
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

// pump drains the live feed into the store. The subscriber has already written
// each message to the KV by the time it appears here, so this goroutine falling
// behind costs an in-memory update and never the record.
// Each event names the board it was delivered to, so this routes rather than
// applying blindly — and it resolves through ensureBoard, so a board first seen
// here gets its history before its first live post.
func (a *AgentBoardListenerActor) pump(sub *board.Subscriber, done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		case ev := <-sub.Events():
			a.ensureBoard(ev.Board).ApplyEvent(ev)
		}
	}
}

// Stop unsubscribes and halts the pump. Idempotent.
// unsubscribeAlive stops answering liveness requests. Called from Stop so a
// stopped recorder goes quiet rather than continuing to claim it records.
func (a *AgentBoardListenerActor) unsubscribeAlive() {
	for _, sub := range a.aliveSubs {
		_ = sub.Unsubscribe()
	}
	a.aliveSubs = nil
}

// unsubscribeQueries stops answering board reads. Called from Stop alongside
// unsubscribeAlive: a stopped recorder must not keep serving a snapshot of a
// board it is no longer collecting, because that answer looks exactly like a
// live one and is quietly frozen.
func (a *AgentBoardListenerActor) unsubscribeQueries() {
	for _, sub := range a.querySubs {
		_ = sub.Unsubscribe()
	}
	a.querySubs = nil
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
	a.unsubscribeQueries()
	a.mu.Unlock()

	if done != nil {
		close(done)
	}
	if sub != nil {
		sub.Close()
	}
}

// Store is the SESSION board's authoritative in-memory copy. Exposed for
// callers in this process; a TUI in another process cannot use it (see the file
// header) and reads the subjects and the KV instead.
func (a *AgentBoardListenerActor) Store() *board.Store { return a.StoreFor(msg.DefaultBoardID) }

// StoreFor is the same for a named board, with its history restored on first
// touch. A board nobody has posted to yet reads as empty, which is what it is.
func (a *AgentBoardListenerActor) StoreFor(id string) *board.Store { return a.ensureBoard(id) }

// Boards lists the boards this recorder currently holds in memory, session
// board included. It is what an operator asks instead of guessing from panes.
func (a *AgentBoardListenerActor) Boards() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.stores))
	for id := range a.stores {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

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
