// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"testing"
	"time"

	"github.com/asynkron/protoactor-go/actor"
)

// The equivalence tests cover the fallback branch. This one covers the branch
// that actually runs in production: a real proto.actor request/reply over the
// mailbox. Without it, a broken Respond would make every call fall back to the
// racy direct read and the fix would be a silent no-op.

type kvGoMsg struct{} // trigger

// kvResponder answers the cascade request, standing in for a PaneGroupActor.
type kvResponder struct {
	kv    paneGroupKV
	wrong bool // reply with an unexpected type instead
}

func (r *kvResponder) Receive(ctx actor.Context) {
	if _, ok := ctx.Message().(*paneGroupKVRequest); ok {
		if r.wrong {
			ctx.Respond(&kvGoMsg{})
			return
		}
		ctx.Respond(&paneGroupKVReply{kv: r.kv})
	}
}

type kvCallResult struct {
	kv paneGroupKV
	ok bool
}

// kvCaller invokes requestKV from inside a real Receive, as the lane does.
type kvCaller struct {
	target *actor.PID
	out    chan kvCallResult
}

func (c *kvCaller) Receive(ctx actor.Context) {
	if _, ok := ctx.Message().(*kvGoMsg); ok {
		reply, ok := requestKV[*paneGroupKVReply](ctx, c.target, &paneGroupKVRequest{})
		res := kvCallResult{ok: ok}
		if ok && reply != nil {
			res.kv = reply.kv
		}
		c.out <- res
	}
}

func runKVCall(t *testing.T, responder *kvResponder) kvCallResult {
	t.Helper()
	system := actor.NewActorSystem()
	defer system.Shutdown()

	target := system.Root.Spawn(actor.PropsFromProducer(func() actor.Actor { return responder }))
	out := make(chan kvCallResult, 1)
	caller := system.Root.Spawn(actor.PropsFromProducer(func() actor.Actor {
		return &kvCaller{target: target, out: out}
	}))

	system.Root.Send(caller, &kvGoMsg{})
	select {
	case res := <-out:
		return res
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the cascade call to complete")
		return kvCallResult{}
	}
}

// Happy path: the reply is delivered and unwrapped, so production uses the
// race-free path rather than silently falling back.
func TestRequestKVOverRealMailbox(t *testing.T) {
	want := paneGroupKV{
		ID:         "g0",
		ActivePane: 1,
		PaneRefs:   []paneKV{{ID: "p0", Title: "one"}, {ID: "p1", Title: "two"}},
	}
	res := runKVCall(t, &kvResponder{kv: want})

	if !res.ok {
		t.Fatal("requestKV reported failure over a live mailbox — production would " +
			"fall back to the racy direct read on every persist")
	}
	if res.kv.ID != want.ID || res.kv.ActivePane != want.ActivePane || len(res.kv.PaneRefs) != 2 {
		t.Errorf("reply not carried through correctly: got %+v, want %+v", res.kv, want)
	}
}

// An unexpected reply type must fail closed so the caller falls back, rather
// than panicking inside the persist path.
func TestRequestKVWrongReplyTypeFallsBack(t *testing.T) {
	res := runKVCall(t, &kvResponder{wrong: true})
	if res.ok {
		t.Error("expected ok=false for an unexpected reply type")
	}
}
