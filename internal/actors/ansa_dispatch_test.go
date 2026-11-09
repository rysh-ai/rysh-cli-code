// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// slowAnsaTransport is the fake transport with a Deliver that takes `delay`,
// recording how many deliveries overlapped — in total and on any one pane.
type slowAnsaTransport struct {
	*fakeAnsaTransport
	delay time.Duration

	mu         sync.Mutex
	inFlight   int
	maxInFlght int
	perPane    map[string]int
	maxPerPane int
}

func (s *slowAnsaTransport) Deliver(paneID, mode, text, program string) error {
	s.mu.Lock()
	s.inFlight++
	if s.inFlight > s.maxInFlght {
		s.maxInFlght = s.inFlight
	}
	s.perPane[paneID]++
	if s.perPane[paneID] > s.maxPerPane {
		s.maxPerPane = s.perPane[paneID]
	}
	s.mu.Unlock()

	time.Sleep(s.delay)

	s.mu.Lock()
	s.inFlight--
	s.perPane[paneID]--
	s.mu.Unlock()
	return s.fakeAnsaTransport.Deliver(paneID, mode, text, program)
}

func newSlowAnsa(n int, delay time.Duration) (*slowAnsaTransport, []string) {
	f := &fakeAnsaTransport{}
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)
		f.panes = append(f.panes, ansaPane{ID: ids[i], GivenName: fmt.Sprintf("agent-%d", i+1), Meta: map[string]string{}})
	}
	return &slowAnsaTransport{fakeAnsaTransport: f, delay: delay, perPane: map[string]int{}}, ids
}

// routeAll fires one route per target through d.spawn — the path the
// workspace actor uses — and waits for every answer.
func routeAll(d *ansaDispatcher, r *ansaRouter, targets []string) []*msg.MsgAnsaRouteResult {
	var wg sync.WaitGroup
	out := make([]*msg.MsgAnsaRouteResult, len(targets))
	accepted := d.now()
	for i, to := range targets {
		wg.Add(1)
		i, to := i, to
		d.spawn(func() {
			defer wg.Done()
			out[i] = d.route(r, accepted, "", to, "", "work order")
		})
	}
	wg.Wait()
	return out
}

// Routes to DIFFERENT panes must not wait for each other. This is the
// bottleneck itself: every route used to run on the workspace actor's one
// goroutine, so N messages to N agents took N × the route time.
func TestAnsaDispatchDifferentTargetsRunInParallel(t *testing.T) {
	const n, delay = 10, 200 * time.Millisecond
	tr, ids := newSlowAnsa(n, delay)
	d := newAnsaDispatcher(ansaRouteSlots)

	start := time.Now()
	res := routeAll(d, &ansaRouter{tr: tr}, ids)
	elapsed := time.Since(start)

	for i, r := range res {
		if !r.OK {
			t.Fatalf("route %d refused: %s", i, ansaRefusalLine(r))
		}
	}
	if got := len(tr.sent()); got != n {
		t.Fatalf("delivered %d messages, want %d", got, n)
	}
	if serial := time.Duration(n) * delay; elapsed >= serial/2 {
		t.Fatalf("%d routes to %d panes took %s; one after another would be %s — they are not running in parallel", n, n, elapsed, serial)
	}
	if tr.maxInFlght < 2 {
		t.Fatalf("at most %d delivery in flight at once, want several", tr.maxInFlght)
	}
}

// Routes to the SAME pane must never overlap: a delivery clears the composer,
// types and submits, and two interleaved would garble both messages.
func TestAnsaDispatchSameTargetIsSerialised(t *testing.T) {
	const n, delay = 5, 50 * time.Millisecond
	tr, ids := newSlowAnsa(1, delay)
	d := newAnsaDispatcher(ansaRouteSlots)

	targets := make([]string, n)
	for i := range targets {
		targets[i] = ids[0]
	}
	start := time.Now()
	res := routeAll(d, &ansaRouter{tr: tr}, targets)
	elapsed := time.Since(start)

	for i, r := range res {
		if !r.OK {
			t.Fatalf("route %d refused: %s", i, ansaRefusalLine(r))
		}
	}
	if tr.maxPerPane != 1 {
		t.Fatalf("%d deliveries overlapped on one pane, want 1 at a time", tr.maxPerPane)
	}
	if got := len(tr.sent()); got != n {
		t.Fatalf("delivered %d of %d", got, n)
	}
	if min := time.Duration(n) * delay; elapsed < min {
		t.Fatalf("%d serial deliveries finished in %s, under the %s they take back to back", n, elapsed, min)
	}
	if lanes := d.laneCount(); lanes != 0 {
		t.Fatalf("%d lane(s) left behind after every route finished, want 0", lanes)
	}
}

// A route that waited past the deadline is refused, not delivered: its sender
// has already been told it failed, so delivering would be a message heard
// after its own failure report — and twice, if the agent retried.
func TestAnsaDispatchExpiredRouteIsNotDelivered(t *testing.T) {
	tr, ids := newSlowAnsa(1, 0)
	d := newAnsaDispatcher(ansaRouteSlots)
	t0 := time.Unix(1_700_000_000, 0)
	d.now = func() time.Time { return t0.Add(ansaRouteDeadline + time.Second) }

	res := d.route(&ansaRouter{tr: tr}, t0, "", ids[0], "", "late work order")

	if res.OK || res.Code != msg.AnsaErrExpired {
		t.Fatalf("got OK=%v code=%q, want a refusal with %q", res.OK, res.Code, msg.AnsaErrExpired)
	}
	if got := len(tr.sent()); got != 0 {
		t.Fatalf("an expired route delivered %d message(s), want 0", got)
	}
}

// The zero accepted time is a synchronous caller still waiting for its answer
// (a human's ##ansa send): no deadline applies, however long the lane took.
func TestAnsaDispatchSyncCallerHasNoDeadline(t *testing.T) {
	tr, ids := newSlowAnsa(1, 0)
	d := newAnsaDispatcher(ansaRouteSlots)
	d.now = func() time.Time { return time.Unix(1_700_000_000, 0).Add(time.Hour) }

	if res := d.route(&ansaRouter{tr: tr}, time.Time{}, "", ids[0], "", "x"); !res.OK {
		t.Fatalf("sync route refused: %s", ansaRefusalLine(res))
	}
}

// The edge still resolves @names and still refuses an unknown target — moving
// the route off the actor must not move it past the edge.
func TestAnsaDispatchKeepsEdgeResolution(t *testing.T) {
	tr, ids := newSlowAnsa(2, 0)
	d := newAnsaDispatcher(ansaRouteSlots)
	r := &ansaRouter{tr: tr}

	res := d.route(r, time.Time{}, "", "@agent-2", "", "hello")
	if !res.OK || res.TargetPaneID != ids[1] {
		t.Fatalf("@agent-2 routed to %q (OK=%v), want %s", res.TargetPaneID, res.OK, ids[1])
	}
	if res := d.route(r, time.Time{}, "", "@nobody", "", "hello"); res.OK || res.Code != msg.AnsaErrUnknownTarget {
		t.Fatalf("@nobody: OK=%v code=%q, want %q", res.OK, res.Code, msg.AnsaErrUnknownTarget)
	}
}

// `ansa keys` to a pane must wait while a delivery holds its lane: presses
// landing mid-typing would end up inside the message being composed.
func TestAnsaKeysWaitForTheTargetsLane(t *testing.T) {
	tr, ids := newSlowAnsa(1, 0)
	d := newAnsaDispatcher(ansaRouteSlots)
	r := &ansaRouter{tr: tr, lanes: d}

	unlock := d.lock(ids[0]) // a delivery in progress
	done := make(chan *ansaKeysOutcome, 1)
	go func() {
		done <- r.Keys(ansaKeysAuditEntry{}, ids[0], []string{"esc"}, true,
			func(ansaKeysAuditEntry) error { return nil })
	}()

	select {
	case <-done:
		t.Fatal("keys were sent while a delivery held the pane's lane")
	case <-time.After(100 * time.Millisecond):
	}
	tr.mu.Lock()
	sentWhileHeld := len(tr.keyWrites)
	tr.mu.Unlock()
	if sentWhileHeld != 0 {
		t.Fatalf("%d key write(s) reached the pane while its lane was held", sentWhileHeld)
	}

	unlock()
	select {
	case out := <-done:
		if out.Result == nil || !out.Result.OK {
			t.Fatalf("keys refused after the lane freed: %+v", out.Result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("keys never went out after the lane freed")
	}
}
