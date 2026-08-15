// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// board_get is the app's and the web UI's ONLY window onto a board, because an
// agents-board pane is shell-less and therefore has no buffer for a renderer to
// draw (see board.go). These tests pin the three properties that decide whether
// that window tells the truth: the right board comes back, it comes back to one
// client, and a board that could not be read is never reported as an empty one.

// newBoardTestServer stands up an in-process NATS with a recorder answering
// board queries from stores the caller controls.
//
// recorded is keyed by RESOLVED board id, which is what lets a test assert
// which board a request landed on rather than merely that it got an answer.
func newBoardTestServer(t *testing.T, recorded map[string]*board.Store) *Server {
	t.Helper()
	nc := startInProcessNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())
	s := NewServer(23234, "board-test-session", pub, nc, pub.Codecs())
	s.hub = newHub()

	subs, err := board.ServeQueries(nc, func(id string) *board.Store {
		return recorded[id]
	}, nil)
	if err != nil {
		t.Fatalf("stand up the recorder: %v", err)
	}
	t.Cleanup(func() {
		for _, sub := range subs {
			_ = sub.Unsubscribe()
		}
	})
	return s
}

// awaitBoardResult reads the one reply the handler pushes to the asking client.
func awaitBoardResult(t *testing.T, c *wsClient) map[string]interface{} {
	t.Helper()
	select {
	case raw := <-c.send:
		var env struct {
			Type string                 `json:"type"`
			Data map[string]interface{} `json:"data"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("decode board reply: %v", err)
		}
		if env.Type != "board_result" {
			t.Fatalf("reply type = %q, want board_result", env.Type)
		}
		return env.Data
	case <-time.After(10 * time.Second):
		t.Fatal("board_get never answered — a board view that hangs is " +
			"indistinguishable from a fleet that has said nothing")
		return nil
	}
}

func boardRequest(t *testing.T, s *Server, params string) (*wsClient, map[string]interface{}) {
	t.Helper()
	c := &wsClient{send: make(chan []byte, 4)}
	s.handleBoardGet(c, json.RawMessage(params))
	return c, awaitBoardResult(t, c)
}

// TestBoardGetReturnsTheNamedBoardToTheAskerOnly covers the feature and its
// isolation property together.
//
// The isolation half is not hygiene here the way it is for clipboard_copy: a
// board id names a FLEET (design 028), so a broadcast would deliver one fleet's
// board to a window rendering another's — carrying the pane_id that window is
// keyed by, so it would overwrite the right pane with the wrong fleet's work.
func TestBoardGetReturnsTheNamedBoardToTheAskerOnly(t *testing.T) {
	desktop := board.New(0)
	desktop.Apply(msg.NewBoardPost("pane-a", "one-tetra", msg.BoardKindMilestone,
		"E12 closed and holding", 1000))
	other := board.New(0)
	other.Apply(msg.NewBoardPost("pane-b", "key-hornet", msg.BoardKindMilestone,
		"a different fleet entirely", 1000))

	s := newBoardTestServer(t, map[string]*board.Store{
		"desktop": desktop,
		"launch":  other,
	})

	// A live hub is what makes the leak check below mean anything: if this
	// handler ever grew a broadcast, the bystander would receive it. NewServer
	// does not build the hub — Start() does, and this harness skips Start().
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.hub.run(ctx)

	bystander := &wsClient{hub: s.hub, send: make(chan []byte, 4)}
	s.hub.register <- bystander

	// The ASKER is registered too, so that a handler which broadcast instead of
	// replying would still satisfy every assertion below and be caught by the
	// leak check alone. Without this, a broadcast fails the test on "never
	// answered" — the right verdict for the wrong reason, and one that would
	// stop pinning isolation the day the asker happened to be registered.
	c := &wsClient{hub: s.hub, send: make(chan []byte, 4)}
	s.hub.register <- c
	s.handleBoardGet(c, json.RawMessage(`{"request_id":"r1","pane_id":"p1","board":"desktop"}`))
	data := awaitBoardResult(t, c)

	if got := data["board"]; got != "desktop" {
		t.Fatalf("board = %v, want desktop", got)
	}
	if got := data["request_id"]; got != "r1" {
		t.Fatalf("request_id = %v, want r1 — an uncorrelated reply cannot be "+
			"matched to the pane that asked", got)
	}
	if _, bad := data["error"]; bad {
		t.Fatalf("unexpected error on a board that is being recorded: %v", data["error"])
	}
	threads, _ := data["threads"].([]interface{})
	if len(threads) != 1 {
		t.Fatalf("threads = %d, want 1", len(threads))
	}
	if body, _ := json.Marshal(threads[0]); !strings.Contains(string(body), "E12 closed and holding") {
		t.Fatalf("the desktop board's post is missing from its own answer: %s", body)
	}
	if body, _ := json.Marshal(threads[0]); strings.Contains(string(body), "a different fleet entirely") {
		t.Fatal("another fleet's post came back on the desktop board")
	}

	select {
	case leaked := <-bystander.send:
		t.Fatalf("board_result reached a client that did not ask: %s", leaked)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestBoardGetReportsAnUnreadableBoardRatherThanAnEmptyOne is the test this
// whole path exists to keep honest.
//
// An empty board and a board nobody is recording render identically — a quiet
// fleet and a dead recorder look the same on screen — and only one of them is a
// reason to go find the daemon. board.Ask draws that line (ErrNoRecorder rather
// than a zero QueryReply) and it survives the hop to the client only if the
// handler forwards it. A regression here restores F-20/F-23 one process further
// out, where it is even harder to see.
func TestBoardGetReportsAnUnreadableBoardRatherThanAnEmptyOne(t *testing.T) {
	// No recorder at all: the query goes out and nothing answers.
	nc := startInProcessNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())
	s := NewServer(23235, "board-test-session", pub, nc, pub.Codecs())
	s.hub = newHub()

	_, data := boardRequest(t, s, `{"request_id":"r2","pane_id":"p1","board":"desktop"}`)

	if _, ok := data["threads"]; ok {
		t.Fatal("an unanswered query came back carrying a thread list — that " +
			"renders as a clean, confident, empty board, which is the exact " +
			"failure board.ErrNoRecorder exists to prevent")
	}
	msgText, _ := data["error"].(string)
	if msgText == "" {
		t.Fatal("no error reported for a board with no recorder")
	}
	if noRec, _ := data["no_recorder"].(bool); !noRec {
		t.Fatalf("no_recorder = false for an unanswered query (error was %q); "+
			"the client cannot tell a missing recorder from a bug in the hop",
			msgText)
	}
}

// TestBoardGetResolvesPaneMetaThroughTheSharedPredicate pins that the server
// does not trust the board id a renderer sends.
//
// Both inputs here are ones a pane can really carry: no board.id meta at all
// (every pre-028 pane), and a value that could never be a subject token. Both
// must land on the session board — the same answer the TUI renders for the same
// pane, because both go through msg.BoardIDFromMeta.
func TestBoardGetResolvesPaneMetaThroughTheSharedPredicate(t *testing.T) {
	session := board.New(0)
	session.Apply(msg.NewBoardPost("pane-a", "someone", msg.BoardKindMilestone,
		"on the session board", 1000))
	s := newBoardTestServer(t, map[string]*board.Store{msg.DefaultBoardID: session})

	for _, tc := range []struct{ name, params string }{
		{"no board.id meta", `{"request_id":"r3","pane_id":"p1"}`},
		{"meta that cannot be a subject token", `{"request_id":"r4","pane_id":"p1","board":"not a board!"}`},
		{"a case variant of the default", `{"request_id":"r5","pane_id":"p1","board":"SESSION"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, data := boardRequest(t, s, tc.params)
			if got := data["board"]; got != msg.DefaultBoardID {
				t.Fatalf("board = %v, want %s", got, msg.DefaultBoardID)
			}
			if _, bad := data["error"]; bad {
				t.Fatalf("unexpected error: %v", data["error"])
			}
			if threads, _ := data["threads"].([]interface{}); len(threads) != 1 {
				t.Fatalf("threads = %d, want the session board's 1", len(threads))
			}
		})
	}
}

// TestBoardGetIgnoresARequestWithNoID guards the correlation contract from the
// other side: a reply that cannot be matched to a request is worse than none,
// because the client would apply it to whichever pane asked last.
func TestBoardGetIgnoresARequestWithNoID(t *testing.T) {
	s := newBoardTestServer(t, map[string]*board.Store{msg.DefaultBoardID: board.New(0)})
	c := &wsClient{send: make(chan []byte, 4)}
	s.handleBoardGet(c, json.RawMessage(`{"pane_id":"p1","board":"desktop"}`))
	select {
	case raw := <-c.send:
		t.Fatalf("answered a request with no request_id: %s", raw)
	case <-time.After(300 * time.Millisecond):
	}
}
