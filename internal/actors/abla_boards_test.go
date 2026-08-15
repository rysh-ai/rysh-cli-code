// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// ABLA with several boards (design 028, `E-39`).
//
// One recorder, one wildcard subscription, one store and one writer PER BOARD.
// These tests run with no TUI and no fleet layer: the point of E-39 is that a
// second board works with nothing but a board id.

const boardQueryBoardsTimeout = 3 * time.Second

// TestABLARecordsEveryBoardFromOneSubscription is the acceptance criterion.
//
// Nothing tells ABLA that a board exists — no registry, no message, no restart.
// A post addressed to a board id it has never seen must be recorded anyway,
// because that is what makes `##board open --board <id>` work on its own.
func TestABLARecordsEveryBoardFromOneSubscription(t *testing.T) {
	nc := newABLATestNATS(t)
	kv := newABLATestKV(t, nc)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)

	abla := NewAgentBoardListenerActor(nc, codecs, kv)
	abla.Start()
	t.Cleanup(abla.Stop)

	if err := msg.SendBoardPost(pub, "epic-07", post(ablaPaneA, "wkr-07", "seven done", "", 100)); err != nil {
		t.Fatalf("SendBoardPost(epic-07): %v", err)
	}
	if err := msg.SendBoardPost(pub, "epic-08", post(ablaPaneB, "wkr-08", "eight done", "", 101)); err != nil {
		t.Fatalf("SendBoardPost(epic-08): %v", err)
	}
	if err := msg.SendBoardPost(pub, "", post(ablaPaneA, "human", "session note", "", 102)); err != nil {
		t.Fatalf("SendBoardPost(session): %v", err)
	}

	ablaWaitFor(t, "all three boards recorded", func() bool {
		return abla.StoreFor("epic-07").Stats().Posts == 1 &&
			abla.StoreFor("epic-08").Stats().Posts == 1 &&
			abla.Store().Stats().Posts == 1
	})

	// Isolation, asserted on content rather than on counts: a count of one can
	// be the wrong one.
	for _, c := range []struct{ board, want string }{
		{"epic-07", "seven done"},
		{"epic-08", "eight done"},
		{msg.DefaultBoardID, "session note"},
	} {
		threads := abla.StoreFor(c.board).Threads()
		if len(threads) != 1 || threads[0].Root == nil || threads[0].Root.Text != c.want {
			t.Errorf("board %q holds %v, want exactly %q", c.board, threads, c.want)
		}
	}

	if got := abla.Boards(); len(got) != 3 {
		t.Errorf("Boards() = %v, want the three that were posted to", got)
	}
}

// TestBoardQueryAnswersPerBoard — `rysh board tail --board epic-07` must read
// epic-07 and not the session board. The read path is the surface an agent in a
// pane uses, so a query that silently answered from the wrong board would be
// invisible to everyone.
func TestBoardQueryAnswersPerBoard(t *testing.T) {
	nc := newABLATestNATS(t)
	kv := newABLATestKV(t, nc)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)

	abla := NewAgentBoardListenerActor(nc, codecs, kv)
	abla.Start()
	t.Cleanup(abla.Stop)

	_ = msg.SendBoardPost(pub, "epic-07", post(ablaPaneA, "wkr-07", "seven only", "", 100))
	_ = msg.SendBoardPost(pub, "", post(ablaPaneB, "human", "session only", "", 101))
	ablaWaitFor(t, "both boards recorded", func() bool {
		return abla.StoreFor("epic-07").Stats().Posts == 1 && abla.Store().Stats().Posts == 1
	})

	seven, err := board.Ask(nc, "epic-07", board.Query{}, boardQueryBoardsTimeout)
	if err != nil {
		t.Fatalf("Ask(epic-07): %v", err)
	}
	if len(seven.Threads) != 1 || seven.Threads[0].Root.Text != "seven only" {
		t.Fatalf("epic-07 answered %v, want only its own post", seven.Threads)
	}

	sess, err := board.Ask(nc, "", board.Query{}, boardQueryBoardsTimeout)
	if err != nil {
		t.Fatalf("Ask(session): %v", err)
	}
	if len(sess.Threads) != 1 || sess.Threads[0].Root.Text != "session only" {
		t.Fatalf("the session board answered %v, want only its own post", sess.Threads)
	}
}

// TestQueryingAnUnknownBoardIsAnEmptyAnswerNotARefusal.
//
// The distinction matters more than it looks. ABLA hears every board through a
// wildcard, so a board nobody has posted to IS empty, and saying so is true.
// Refusing would make an honest reader report ErrNoRecorder — "nothing can be
// said about what was posted" — for a board that is simply quiet, which is the
// F-20 shape with the sign flipped.
func TestQueryingAnUnknownBoardIsAnEmptyAnswerNotARefusal(t *testing.T) {
	nc := newABLATestNATS(t)
	kv := newABLATestKV(t, nc)
	codecs := msg.DefaultCodecRegistry()

	abla := NewAgentBoardListenerActor(nc, codecs, kv)
	abla.Start()
	t.Cleanup(abla.Stop)

	reply, err := board.Ask(nc, "never-posted-to", board.Query{}, boardQueryBoardsTimeout)
	if err != nil {
		t.Fatalf("Ask(never-posted-to) = %v, want an empty answer from a live recorder", err)
	}
	if len(reply.Threads) != 0 {
		t.Fatalf("an unposted board answered with %d threads", len(reply.Threads))
	}
}

// TestStoppedRecorderGoesQuietForEveryBoard. A stopped recorder must stop
// answering — for named boards too, not just the one whose subject was
// unsubscribed by name. A wildcard left subscribed after Stop would serve a
// frozen snapshot that looks exactly like a live one.
func TestStoppedRecorderGoesQuietForEveryBoard(t *testing.T) {
	nc := newABLATestNATS(t)
	kv := newABLATestKV(t, nc)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)

	abla := NewAgentBoardListenerActor(nc, codecs, kv)
	abla.Start()

	_ = msg.SendBoardPost(pub, "epic-07", post(ablaPaneA, "wkr-07", "before stop", "", 100))
	ablaWaitFor(t, "the named board recorded", func() bool {
		return abla.StoreFor("epic-07").Stats().Posts == 1
	})
	if _, err := board.Ask(nc, "epic-07", board.Query{}, boardQueryBoardsTimeout); err != nil {
		t.Fatalf("a live recorder refused a named board: %v", err)
	}

	abla.Stop()

	if _, err := board.Ask(nc, "epic-07", board.Query{}, 300*time.Millisecond); err == nil {
		t.Fatal("a stopped recorder still answered for epic-07: a frozen board is " +
			"indistinguishable from a live one")
	}
	if _, err := nc.Request(msg.BoardAliveSubject("epic-07"), nil, 300*time.Millisecond); err == nil {
		t.Fatal("a stopped recorder still claimed to be recording epic-07")
	}
}

// TestNamedBoardSurvivesARestart is founder gate 2 (the board persists) applied
// to a board that is not the session board — and it exercises the lazy path:
// the new recorder has never heard of epic-07 until the query arrives, so the
// history has to be restored on first touch rather than at Start.
func TestNamedBoardSurvivesARestart(t *testing.T) {
	nc := newABLATestNATS(t)
	kv := newABLATestKV(t, nc)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)

	first := NewAgentBoardListenerActor(nc, codecs, kv)
	first.Start()
	_ = msg.SendBoardPost(pub, "epic-07", post(ablaPaneA, "wkr-07", "written yesterday", "", 100))
	ablaWaitFor(t, "the post was recorded", func() bool {
		return first.StoreFor("epic-07").Stats().Posts == 1
	})
	first.Stop()

	// A new process: new actor, same KV. Nothing has told it epic-07 exists.
	second := NewAgentBoardListenerActor(nc, codecs, kv)
	second.Start()
	t.Cleanup(second.Stop)

	reply, err := board.Ask(nc, "epic-07", board.Query{}, boardQueryBoardsTimeout)
	if err != nil {
		t.Fatalf("Ask(epic-07) after restart: %v", err)
	}
	if len(reply.Threads) != 1 || reply.Threads[0].Root.Text != "written yesterday" {
		t.Fatalf("epic-07 came back as %v, want yesterday's post", reply.Threads)
	}

	// And the restored board must not have inherited the session board's keys,
	// nor vice versa.
	sess, err := board.Ask(nc, "", board.Query{}, boardQueryBoardsTimeout)
	if err != nil {
		t.Fatalf("Ask(session): %v", err)
	}
	if len(sess.Threads) != 0 {
		t.Fatalf("the session board restored %v, but everything was posted to epic-07", sess.Threads)
	}
}

// TestOneWriterPerBoardByConstruction.
//
// Design 028 §6.3 originally proposed one ABLA per board, relying on SpawnNamed
// to make a second writer fail loudly. This is the amended invariant: one actor
// owns a map, so a second Persistence for a board cannot be constructed at all
// — and two different boards never share one.
func TestOneWriterPerBoardByConstruction(t *testing.T) {
	nc := newABLATestNATS(t)
	kv := newABLATestKV(t, nc)
	abla := NewAgentBoardListenerActor(nc, msg.DefaultCodecRegistry(), kv)

	a1 := abla.persistFor("epic-07")
	a2 := abla.persistFor("epic-07")
	if a1 != a2 {
		t.Fatal("two Persistence values for one board: both would start at ordinal 0 " +
			"and the second would overwrite the first's history")
	}
	if abla.persistFor("epic-08") == a1 {
		t.Fatal("two boards share one writer: their posts would share an ordinal and a key space")
	}
	if abla.persistFor("") == a1 {
		t.Fatal("a named board and the session board share one writer")
	}
}
