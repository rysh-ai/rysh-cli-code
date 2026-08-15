// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// TestAcceptanceTwoAgentsPostAndAReplyThreads is the epic's acceptance
// criterion, end to end, in one test:
//
//	"Two agents in two panes post to the board; a human watching the
//	 agents-board pane sees both, correctly attributed to their pane
//	 given-names, in real time. A threaded reply appears under its root, not
//	 as a new root."
//
// Every layer the real thing uses is exercised and nothing is faked: a REAL
// in-process NATS server, the REAL publisher an agent calls
// (msg.SendBoardPost), the REAL subscriber, the REAL store, and the REAL
// renderer that draws the pane. The other board tests each pin one layer; this
// one proves they are actually connected, which is the difference between
// "builds green" and "wired end-to-end and reachable by a user".
func TestAcceptanceTwoAgentsPostAndAReplyThreads(t *testing.T) {
	// --- a real bus -------------------------------------------------------
	opts := &server.Options{
		Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true,
		JetStream: true, StoreDir: t.TempDir(),
	}
	srv, err := server.NewServer(opts)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats server not ready")
	}
	t.Cleanup(srv.Shutdown)

	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(nc.Close)

	codecs := msg.DefaultCodecRegistry()
	sub, err := board.Subscribe(nc, codecs, 256, nil)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(sub.Close)

	pub := msg.NewNATSPublisher(nc, codecs)
	store := board.New(0)

	// --- two agents, two panes, two given-names ---------------------------
	// Agent A opens a thread. The thread id is minted by the POSTER, with no
	// round trip to the board — that is the design decision that lets an agent
	// post while blind to the board (design 025 §4.3).
	thread := msg.MintThreadID(boardPaneA, 1)

	rootPost := msg.NewBoardPost(boardPaneA, "mgr-01-agents-board-slack",
		msg.BoardKindMilestone, "wave 3 merged and green", 1_000)
	rootPost.ThreadID = thread

	replyPost := msg.NewBoardPost(boardPaneB, "wkr-01-agents-board-slack",
		msg.BoardKindReply, "view renders threads under roots", 2_000)
	replyPost.ThreadID = thread

	for _, p := range []*msg.MsgBoardPost{rootPost, replyPost} {
		if err := msg.SendBoardPost(pub, "", p); err != nil {
			t.Fatalf("SendBoardPost(%s): %v", p.Persona, err)
		}
	}

	// --- the human watches the pane ---------------------------------------
	deadline := time.After(10 * time.Second)
	for got := 0; got < 2; {
		select {
		case ev := <-sub.Events():
			store.Apply(ev.Post)
			got++
		case <-deadline:
			t.Fatalf("only %d of 2 posts arrived on the board within 10s", got)
		}
	}

	m := buildBoardModel(store)
	rendered := strings.Join(m.boardRows("", 100), "\n")

	// Both agents are visible, each under its own pane given-name.
	for _, persona := range []string{
		"mgr-01-agents-board-slack",
		"wkr-01-agents-board-slack",
	} {
		if !strings.Contains(rendered, persona) {
			t.Errorf("the board does not show %q; rendered:\n%s", persona, rendered)
		}
	}

	// The reply landed UNDER its root, not as a second root. Asserted on the
	// store's structure rather than on indentation, so the test survives a
	// cosmetic change to the renderer.
	threads := store.Threads()
	if len(threads) != 1 {
		t.Fatalf("the reply became a separate root: want 1 thread, got %d", len(threads))
	}
	th := threads[0]
	if th.Root == nil {
		t.Fatal("the thread has no root: the reply is stranded as provisional")
	}
	if n := len(th.Replies); n != 1 {
		t.Fatalf("want 1 reply under the root, got %d", n)
	}
	if got := th.Replies[0].Persona; got != "wkr-01-agents-board-slack" {
		t.Errorf("the reply is attributed to %q, not the agent that posted it", got)
	}
	if got := th.Root.Persona; got != "mgr-01-agents-board-slack" {
		t.Errorf("the root is attributed to %q, not the agent that posted it", got)
	}

	// And both texts actually reached the screen.
	for _, text := range []string{"wave 3 merged and green", "view renders threads under roots"} {
		if !strings.Contains(rendered, text) {
			t.Errorf("the board does not show %q; rendered:\n%s", text, rendered)
		}
	}
}
