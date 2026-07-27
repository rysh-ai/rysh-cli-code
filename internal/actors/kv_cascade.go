package actors

import (
	"time"

	"github.com/asynkron/protoactor-go/actor"
)

// ---------------------------------------------------------------------------
// Race-free KV serialisation cascade
// ---------------------------------------------------------------------------
//
// Persistence used to serialise the tab tree with DIRECT cross-actor method
// calls: the workspace goroutine called TabActor.ToKV(), which called
// LaneActor.ToKV(), which called PaneGroupActor.ToKV(). Each of those actors
// mutates the very fields being read from its OWN goroutine, so the reads raced
// with, for example, a resize applying flex/rowFlex. proto.actor only
// guarantees sequential access when state is touched inside the owning actor's
// Receive — reaching into the struct from outside voids that guarantee.
//
// Two observed consequences: torn/garbage weights, and (during the async
// restore cascade, when a lane actor has its laneRefs populated but its child
// groups have not started yet) persisting a lane with correct Flex but EMPTY
// PaneGroups — clobbering good KV with a structurally broken document.
//
// The fix inverts the direction: instead of reaching in, each level ASKS the
// level below to serialise itself, and that work runs inside the callee's own
// Receive. These are in-process proto.actor request/reply messages (not NATS),
// so they need no codec registration and are delivered straight to the target's
// mailbox — the same mechanism already used for the agent/humanoid registries.
//
// The call graph is strictly downward (workspace → tab → lane → group) and no
// actor blocks on an ancestor, so the nested futures cannot deadlock. Every
// level falls back to the direct read if the future fails (actor stopped or
// timed out), which keeps the old behaviour rather than losing state — a
// slightly-racy write still beats no write at all.

// defaultFlex is the layout weight a lane/pane-group is created with when it
// has no siblings to average against. equalizeLanes/equalizeGroups reset to the
// same value, so an equalized layout and a freshly-seeded one agree.
const defaultFlex = 10

// kvCascadeTimeout bounds each hop of the serialisation cascade. It is short on
// purpose: this runs on the debounced persist path (at most once every couple
// of seconds) and on shutdown, where blocking for long would stall the daemon.
// On expiry the caller falls back to the direct read.
const kvCascadeTimeout = 500 * time.Millisecond

// tabKVRequest asks a TabActor to serialise itself on its own goroutine.
type tabKVRequest struct{}

// tabKVReply carries the serialised tab.
type tabKVReply struct{ kv tabKV }

// laneKVRequest asks a LaneActor to serialise itself on its own goroutine.
type laneKVRequest struct{}

// laneKVReply carries the serialised lane.
type laneKVReply struct{ kv laneKV }

// paneGroupKVRequest asks a PaneGroupActor to serialise itself on its own
// goroutine. Pane groups are the leaves of the KV tree (they store only pane
// id/title refs, not pane content), so this hop needs no further cascade.
type paneGroupKVRequest struct{}

// paneGroupKVReply carries the serialised pane group.
type paneGroupKVReply struct{ kv paneGroupKV }

// requestKV performs one hop of the cascade: it asks the actor behind pid to
// serialise itself and returns the reply. ok is false when the actor is gone,
// times out, or answers with an unexpected type — the caller then falls back to
// a direct read.
func requestKV[T any](ctx actor.Context, pid *actor.PID, request interface{}) (T, bool) {
	var zero T
	if pid == nil {
		return zero, false
	}
	res, err := ctx.RequestFuture(pid, request, kvCascadeTimeout).Result()
	if err != nil {
		return zero, false
	}
	reply, ok := res.(T)
	if !ok {
		return zero, false
	}
	return reply, true
}
