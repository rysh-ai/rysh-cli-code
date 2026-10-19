// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
)

// The macmini-rysh-elect daemon crashed repeatedly with
// "panic: send on closed channel" at board.go sendToClient: a web client asked
// for a board and disconnected before the reply came back; the hub closed its
// send channel, and the handler's goroutine then sent on it. A reply to a gone
// client must be dropped, never panic.

func TestReplyAfterDisconnectIsDropped(t *testing.T) {
	c := &wsClient{send: make(chan []byte, 1)}
	c.closeSend()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("reply to a disconnected client panicked: %v", r)
		}
	}()
	sendToClient(c, []byte(`{}`))
	if c.trySend([]byte(`{}`)) {
		t.Fatalf("trySend reported delivery to a closed client")
	}
	c.closeSend() // idempotent: a second close (ctx shutdown after unregister) must not panic
}

// Race the real board_get path against the hub unregistering the client.
func TestBoardGetClientDisconnectsMidQuery(t *testing.T) {
	s := newBoardTestServer(t, map[string]*board.Store{"desktop": board.New(0)})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.hub.run(ctx)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panicked: %v", r)
		}
	}()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		c := &wsClient{hub: s.hub, send: make(chan []byte, 1)}
		s.hub.register <- c
		s.handleBoardGet(c, json.RawMessage(`{"request_id":"r","pane_id":"p","board":"desktop"}`))
		wg.Add(1)
		go func() { defer wg.Done(); s.hub.unregister <- c }()
	}
	wg.Wait()
	time.Sleep(500 * time.Millisecond) // let the in-flight board replies land after the closes
}
