// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// The daemon for macmini-rysh-elect died twice, thirteen hours apart, on the
// same line:
//
//	panic: send on closed channel
//	rysh/internal/web.sendToClient  board.go:186
//	rysh/internal/web.(*Server).handleBoardGet.func1  board.go:170
//
// and took every agent in the session with it both times. The web board
// polls, so a viewer whose socket drops while a board query is in flight is
// not an edge case — over a day of polling it is a certainty.
//
// The mechanism: the hub is the only goroutine that closes a client's `send`,
// and it does so on unregister and when it drops a slow client. Handlers that
// answer ONE client (board_get, clipboard_copy, completion_get) do the slow
// part off the read loop and send from a goroutine the hub knows nothing
// about. `select { case c.send <- f: default: }` looks like it makes that safe
// and does not: a send on a CLOSED channel panics even inside a select with a
// default. The default only covers a FULL channel.
//
// These tests drive the real hub's close path, not a hand-closed channel, so
// they fail for the reason production did.

// disconnectViaHub registers c with a running hub, unregisters it, and waits
// until the hub has closed c.send — the exact state a handler goroutine finds
// when its client went away mid-request.
func disconnectViaHub(t *testing.T, s *Server, c *wsClient) {
	t.Helper()
	s.hub.register <- c
	s.hub.unregister <- c
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-c.send:
			if !ok {
				return // closed by the hub
			}
		case <-deadline:
			t.Fatal("hub never closed the client's send channel")
		}
	}
}

// newGatedBoardServer is newBoardTestServer with a recorder that blocks inside
// the query until the test says go — which is what parks handleBoardGet's
// goroutine in board.Ask long enough for its client to disconnect.
func newGatedBoardServer(t *testing.T) (s *Server, entered <-chan struct{}, release chan<- struct{}) {
	t.Helper()
	nc := startInProcessNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())
	s = NewServer(23235, "send-after-close", pub, nc, pub.Codecs())
	s.hub = newHub()

	in := make(chan struct{}, 1)
	gate := make(chan struct{})
	store := board.New(16)
	subs, err := board.ServeQueries(nc, func(string) *board.Store {
		select {
		case in <- struct{}{}:
		default:
		}
		<-gate
		return store
	}, nil)
	if err != nil {
		t.Fatalf("stand up the recorder: %v", err)
	}
	t.Cleanup(func() {
		for _, sub := range subs {
			_ = sub.Unsubscribe()
		}
	})
	return s, in, gate
}

func runHub(t *testing.T, s *Server) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go s.hub.run(ctx)
}

// TestBoardReplyAfterDisconnectDoesNotCrashTheDaemon is the production crash,
// on the success branch (board.go:170 in the trace).
func TestBoardReplyAfterDisconnectDoesNotCrashTheDaemon(t *testing.T) {
	s, entered, release := newGatedBoardServer(t)
	runHub(t, s)

	c := &wsClient{hub: s.hub, send: make(chan []byte, 4)}
	s.handleBoardGet(c, json.RawMessage(`{"request_id":"r1","pane_id":"p1","board":"fleets"}`))

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the board query never reached the recorder")
	}
	disconnectViaHub(t, s, c) // the viewer's socket drops mid-query
	close(release)            // ...and then the answer arrives

	// Nothing to observe on success: the reply is dropped. On failure the
	// handler goroutine panics and takes the test binary down with it, which is
	// the daemon crash reproduced. Give the in-process round trip time to land.
	time.Sleep(500 * time.Millisecond)
}

// TestBoardErrorReplyAfterDisconnectDoesNotCrashTheDaemon covers the other
// send site in the same goroutine (board.go:139): the recorder does not answer
// within boardQueryTimeout, so the handler reports the error — to a client that
// is already gone.
func TestBoardErrorReplyAfterDisconnectDoesNotCrashTheDaemon(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out boardQueryTimeout")
	}
	s, entered, release := newGatedBoardServer(t)
	t.Cleanup(func() { close(release) }) // unblock the recorder after the test
	runHub(t, s)

	c := &wsClient{hub: s.hub, send: make(chan []byte, 4)}
	s.handleBoardGet(c, json.RawMessage(`{"request_id":"r2","pane_id":"p1","board":"fleets"}`))

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the board query never reached the recorder")
	}
	disconnectViaHub(t, s, c)
	time.Sleep(boardQueryTimeout + time.Second) // Ask times out; the error reply is sent
}

// TestClipboardReplyAfterDisconnectDoesNotCrashTheDaemon covers the same
// pattern in clipboard_copy, whose reply is also sent from a goroutine.
func TestClipboardReplyAfterDisconnectDoesNotCrashTheDaemon(t *testing.T) {
	s := &Server{hub: newHub()}
	runHub(t, s)
	c := &wsClient{hub: s.hub, send: make(chan []byte, 4)}
	disconnectViaHub(t, s, c)

	s.replyClipboardContent(c, clipboardCopyCmd{RequestID: "r3", PaneID: "p1"}, "text", false, "")
}

// TestTrySendRacesCloseSendSafely hammers the new primitive directly: many
// goroutines sending while the hub's close lands in the middle. Meaningful
// under -race, and it would panic without the lock even without -race.
func TestTrySendRacesCloseSendSafely(t *testing.T) {
	for round := 0; round < 200; round++ {
		c := &wsClient{send: make(chan []byte, 1)}
		done := make(chan struct{})
		for g := 0; g < 8; g++ {
			go func() {
				for i := 0; i < 50; i++ {
					c.trySend([]byte("x"))
				}
				done <- struct{}{}
			}()
		}
		go func() { // the writer pump, draining until the hub closes
			for range c.send {
			}
		}()
		c.closeSend()
		c.closeSend() // idempotent: the hub may reach both unregister and slow-drop
		for g := 0; g < 8; g++ {
			<-done
		}
		if c.trySend([]byte("late")) {
			t.Fatal("trySend reported success on a closed client")
		}
	}
}

// TestNoRawSendOnAClientChannelOutsideTheHub pins the CLASS, not the three
// instances. Every one of the three crash sites looked safe — a select with a
// default — and the next handler someone adds will look safe the same way.
// Only three places may send on a client's channel directly:
//
//	hub.go   the hub goroutine, which is the channel's sole closer; and
//	         trySend itself
//	seed.go  runs BEFORE the client is registered, so the hub cannot have
//	         closed the channel yet (documented at seedStreamClient)
//
// Anything else must call trySend.
func TestNoRawSendOnAClientChannelOutsideTheHub(t *testing.T) {
	allowed := map[string]bool{"hub.go": true, "seed.go": true}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	raw := regexp.MustCompile(`\bsend\s*<-`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || allowed[f] {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			code := line
			if j := strings.Index(code, "//"); j >= 0 {
				code = code[:j]
			}
			if raw.MatchString(code) {
				t.Errorf("%s:%d sends on a client channel directly — use c.trySend: a "+
					"select with a default does NOT survive a channel the hub closed:\n\t%s",
					f, i+1, strings.TrimSpace(line))
			}
		}
	}
}
