// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"testing"
	"time"
)

// waitFor polls cond until it holds or the deadline passes. The hub mutates its
// counters on its own goroutine, so a register/unregister is observable only
// after that goroutine has processed the channel send.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The 200ms poll in pollSnapshots builds a FULL workspace snapshot — every
// pane's content, measured at 4.4 MB on a 50-pane session — and then hands it
// only to clients that did not opt into the content plane. hasPlain() is what
// lets the poll skip that work entirely, so it must track exactly the non-stream
// clients and must not be fooled by stream clients coming and going.
func TestHubPlainCountGatesTheFullSnapshotPoll(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := newHub()
	go h.run(ctx)

	if h.hasPlain() {
		t.Fatal("a hub with no clients must not ask for the full-snapshot poll")
	}

	// A stream client renders from layout-only snapshots plus deltas, so it must
	// NOT keep the expensive poll alive.
	stream := &wsClient{hub: h, send: make(chan []byte, 4), streamContent: true}
	h.register <- stream
	waitFor(t, "stream client registered", func() bool { return h.hasStream() })
	if h.hasPlain() {
		t.Fatal("a stream-only hub still requested the full-snapshot poll")
	}

	// A plain client is the one consumer that needs it.
	plain := &wsClient{hub: h, send: make(chan []byte, 4), streamContent: false}
	h.register <- plain
	waitFor(t, "plain client registered", func() bool { return h.hasPlain() })

	// Losing the stream client must not disturb the plain client's demand.
	h.unregister <- stream
	waitFor(t, "stream client unregistered", func() bool { return !h.hasStream() })
	if !h.hasPlain() {
		t.Fatal("unregistering a stream client wrongly cleared the plain-client demand")
	}

	// Once the last plain client leaves, the poll must go quiet again.
	h.unregister <- plain
	waitFor(t, "plain client unregistered", func() bool { return !h.hasPlain() })
}

// A client too slow to keep up is dropped inside the broadcast loop rather than
// through unregister. That path has to decrement the same counter, or a single
// stalled browser tab would pin the 4.4 MB poll on forever.
func TestHubSlowPlainClientDropClearsPlainCount(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := newHub()
	go h.run(ctx)

	// An unbuffered send channel with no reader: the very first broadcast hits
	// the default branch and drops the client.
	plain := &wsClient{hub: h, send: make(chan []byte), streamContent: false}
	h.register <- plain
	waitFor(t, "plain client registered", func() bool { return h.hasPlain() })

	h.sendWhere([]byte("payload"), func(*wsClient) bool { return true })

	waitFor(t, "slow plain client dropped", func() bool { return !h.hasPlain() })
}
