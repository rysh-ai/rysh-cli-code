// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// streamPaneVT used to re-read the whole layout — a request to every tab, lane,
// group and pane — and re-pull every visible interactive pane's frame ten times
// a second for as long as any browser was open, whether or not anything moved.
// It is now driven by the daemon's change signals (vt_signals.go), with a slow
// resync and sweep as the safety net.

func countRequests(t *testing.T, nc *nats.Conn, subject string) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	sub, err := nc.Subscribe(subject, func(*nats.Msg) { n.Add(1) })
	if err != nil {
		t.Fatalf("subscribe %s: %v", subject, err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	_ = nc.Flush()
	return &n
}

func TestStreamPaneVTIsQuietWhenNothingChanges(t *testing.T) {
	s, nc, _ := newControlTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.hub = newHub()
	go s.hub.run(ctx)

	r := &vtResponder{t: t, nc: nc, paneID: "pane-1", screen: make(chan []string, 1)}
	r.cur = []string{"idle screen"}
	r.serve()
	structure := countRequests(t, nc, msg.T("ws", "snapshot"))
	frames := countRequests(t, nc, msg.T("pane", "pane-1", "snapshot"))

	client := &wsClient{hub: s.hub, send: make(chan []byte, 256), streamContent: true}
	s.hub.register <- client
	go s.streamPaneVT(ctx)

	countFrames(client, 1900*time.Millisecond)
	// The old loop made ~19 of each in this window.
	if got := structure.Load(); got > 2 {
		t.Errorf("layout read %d times in ~2s with nothing changing, want at most 2 (first read + resync)", got)
	}
	if got := frames.Load(); got > 3 {
		t.Errorf("idle pane's frame pulled %d times in ~2s, want at most 3 (first + 1s sweeps)", got)
	}
}

func TestStreamPaneVTFetchesOnRawDirtyBeforeTheSweep(t *testing.T) {
	s, nc, _ := newControlTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.hub = newHub()
	go s.hub.run(ctx)

	r := &vtResponder{t: t, nc: nc, paneID: "pane-1", screen: make(chan []string, 1)}
	r.cur = []string{"before"}
	r.serve()
	client := &wsClient{hub: s.hub, send: make(chan []byte, 256), streamContent: true}
	s.hub.register <- client
	go s.streamPaneVT(ctx)

	countFrames(client, 300*time.Millisecond) // the first frame
	r.screen <- []string{"after"}
	if err := msg.SendPaneRawDirty(msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry()), "pane-1"); err != nil {
		t.Fatalf("publish rawDirty: %v", err)
	}
	// Well inside the 1s sweep: only the signal can have caused it.
	got, last := countFrames(client, 400*time.Millisecond)
	if got == 0 || len(last) == 0 || last[0] != "after" {
		t.Fatalf("rawDirty did not bring the changed frame within 400ms (got %d frames, last %v)", got, last)
	}
}

// The sweep fallback: a change whose rawDirty signal is lost (here: never sent)
// still reaches the client. The contract (vt_signals.go): signals are hints — "a
// dropped message costs at most that much staleness [vtSweepInterval], never a
// stuck screen". The bound is the sweep interval plus a margin for the 100ms
// tick phase and the fetch; it is the constant, not a hard-coded second.
func TestStreamPaneVTSweepFallbackDeliversUnsignalledChange(t *testing.T) {
	s, nc, _ := newControlTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.hub = newHub()
	go s.hub.run(ctx)

	r := &vtResponder{t: t, nc: nc, paneID: "pane-1", screen: make(chan []string, 1)}
	r.cur = []string{"before"}
	r.serve()
	client := &wsClient{hub: s.hub, send: make(chan []byte, 256), streamContent: true}
	s.hub.register <- client
	go s.streamPaneVT(ctx)

	countFrames(client, 300*time.Millisecond) // the first frame
	r.screen <- []string{"after"}             // deliberately NO rawDirty
	got, last := countFrames(client, vtSweepInterval+500*time.Millisecond)
	if got == 0 || len(last) == 0 || last[0] != "after" {
		t.Fatalf("an unsignalled change was not swept within vtSweepInterval+500ms (got %d frames, last %v)", got, last)
	}
}
