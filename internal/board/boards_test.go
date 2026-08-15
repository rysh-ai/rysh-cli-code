// SPDX-License-Identifier: Apache-2.0

package board

import (
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Many boards in one session (design 028, `E-39`). Real NATS, real JetStream,
// real subjects — the same rule the rest of this package's tests follow.

func boardPost(paneID, text string, ts int64) *msg.MsgBoardPost {
	return msg.NewBoardPost(paneID, "agent-"+paneID, msg.BoardKindMilestone, text, ts)
}

// drain collects events for a short while, indexed by board.
func drain(t *testing.T, s *Subscriber, want int) map[string][]Event {
	t.Helper()
	out := map[string][]Event{}
	deadline := time.After(3 * time.Second)
	for n := 0; n < want; n++ {
		select {
		case ev := <-s.Events():
			out[ev.Board] = append(out[ev.Board], ev)
		case <-deadline:
			t.Fatalf("timed out after %d of %d events: %v", n, want, out)
		}
	}
	return out
}

// TestAPostOnOneBoardNeverReachesAnother is the headline property of E-39.
//
// It is asserted on the STORES rather than only on the events, because the
// events are one demux away from the thing a human reads: two fleets whose
// posts land in one store look like one busy fleet, and nothing in the render
// path could tell them apart afterwards.
func TestAPostOnOneBoardNeverReachesAnother(t *testing.T) {
	nc := newTestNATS(t)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)

	sub, err := Subscribe(nc, codecs, 64, nil)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(sub.Close)

	if err := msg.SendBoardPost(pub, "epic-07", boardPost("pA", "seven works", 100)); err != nil {
		t.Fatalf("SendBoardPost(epic-07): %v", err)
	}
	if err := msg.SendBoardPost(pub, "epic-08", boardPost("pB", "eight works", 101)); err != nil {
		t.Fatalf("SendBoardPost(epic-08): %v", err)
	}
	if err := msg.SendBoardPost(pub, "", boardPost("pC", "session works", 102)); err != nil {
		t.Fatalf("SendBoardPost(session): %v", err)
	}

	stores := map[string]*Store{}
	for _, ev := range flatten(drain(t, sub, 3)) {
		if stores[ev.Board] == nil {
			stores[ev.Board] = New(0)
		}
		stores[ev.Board].ApplyEvent(ev)
	}

	for _, c := range []struct{ board, want string }{
		{"epic-07", "seven works"},
		{"epic-08", "eight works"},
		{msg.DefaultBoardID, "session works"},
	} {
		s := stores[c.board]
		if s == nil {
			t.Fatalf("board %q received nothing", c.board)
		}
		threads := s.Threads()
		if len(threads) != 1 {
			t.Fatalf("board %q holds %d threads, want exactly its own", c.board, len(threads))
		}
		if got := threads[0].Root.Text; got != c.want {
			t.Errorf("board %q rendered %q, want %q — a post crossed boards", c.board, got, c.want)
		}
	}
}

func flatten(byBoard map[string][]Event) []Event {
	var all []Event
	for _, evs := range byBoard {
		all = append(all, evs...)
	}
	return all
}

// TestLegacySubjectStillLandsOnTheSessionBoard is the compatibility half: a
// publisher built before board ids existed writes the three-token subject, and
// its posts must still arrive — tagged as the session board, not dropped for
// having no board token.
func TestLegacySubjectStillLandsOnTheSessionBoard(t *testing.T) {
	nc := newTestNATS(t)
	codecs := msg.DefaultCodecRegistry()

	sub, err := Subscribe(nc, codecs, 16, nil)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(sub.Close)

	// Published exactly as an old client would: straight onto the legacy
	// subject, not through the board-aware builder.
	pub := msg.NewNATSPublisher(nc, codecs)
	if err := pub.Send(msg.T("board", "post"), boardPost("pOld", "from an old client", 1)); err != nil {
		t.Fatalf("Send: %v", err)
	}

	ev := waitEvent(t, sub)
	if ev.Board != msg.DefaultBoardID {
		t.Fatalf("legacy post tagged board %q, want %q", ev.Board, msg.DefaultBoardID)
	}
	if ev.Post == nil || ev.Post.Text != "from an old client" {
		t.Fatalf("legacy post did not arrive intact: %+v", ev.Post)
	}
}

// TestEachBoardKeepsItsOwnOrdinal is the data-loss guard, per board.
//
// Two Persistence values over ONE board both start at ordinal 0, so both write
// post-…0001 and the second Put overwrites the first — history is destroyed,
// not duplicated (TestTwoWritersThatBothPrimedFirstStillDestroyHistory). Board ids scope that rule
// rather than weakening it: two boards write the same ordinal on purpose, and
// must not collide, because their keys are namespaced.
func TestEachBoardKeepsItsOwnOrdinal(t *testing.T) {
	nc := newTestNATS(t)
	kv := newTestKV(t, nc)

	seven := NewPersistence(kv, "epic-07")
	eight := NewPersistence(kv, "epic-08")
	session := NewPersistence(kv, "")

	for i, p := range []*Persistence{seven, eight, session} {
		if err := p.SavePost(boardPost("p", "post from writer", int64(i))); err != nil {
			t.Fatalf("SavePost: %v", err)
		}
	}

	for _, c := range []struct {
		board string
		want  int
	}{{"epic-07", 1}, {"epic-08", 1}, {msg.DefaultBoardID, 1}} {
		store := New(0)
		posts, _, err := NewPersistence(kv, c.board).Restore(store)
		if err != nil {
			t.Fatalf("Restore(%s): %v", c.board, err)
		}
		if posts != c.want {
			t.Errorf("board %q restored %d posts, want %d — boards are sharing keys",
				c.board, posts, c.want)
		}
	}
}

// TestANewWriterDoesNotOverwriteAnExistingBoard.
//
// A board's Persistence is created lazily, the first time a message names that
// board — long after its history was written by an earlier process. If it
// started at ordinal 0 it would overwrite post-…0001 on its first write, which
// is the exact failure the single-writer rule exists to prevent, arriving
// through the lazy-creation door instead of the second-actor door.
func TestANewWriterDoesNotOverwriteAnExistingBoard(t *testing.T) {
	nc := newTestNATS(t)
	kv := newTestKV(t, nc)

	yesterday := NewPersistence(kv, "epic-07")
	for i := 0; i < 3; i++ {
		if err := yesterday.SavePost(boardPost("pA", "yesterday", int64(i))); err != nil {
			t.Fatalf("SavePost: %v", err)
		}
	}

	// A fresh process, a fresh Persistence, no Restore first — the lazy path.
	today := NewPersistence(kv, "epic-07")
	if err := today.SavePost(boardPost("pB", "today", 99)); err != nil {
		t.Fatalf("SavePost: %v", err)
	}

	store := New(0)
	posts, _, err := NewPersistence(kv, "epic-07").Restore(store)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if posts != 4 {
		t.Fatalf("restored %d posts, want 4: a new writer overwrote history it did not read", posts)
	}
}

// TestRestoreDoesNotReplayAnotherBoardsHistory. One bucket holds every board in
// the session, so a restore that did not filter would replay 25 fleets onto
// whichever board restored first — and it would look like a busy board rather
// than like a bug.
func TestRestoreDoesNotReplayAnotherBoardsHistory(t *testing.T) {
	nc := newTestNATS(t)
	kv := newTestKV(t, nc)

	if err := NewPersistence(kv, "epic-07").SavePost(boardPost("pA", "sevens only", 1)); err != nil {
		t.Fatalf("SavePost: %v", err)
	}
	if err := NewPersistence(kv, "").SavePost(boardPost("pB", "session only", 2)); err != nil {
		t.Fatalf("SavePost: %v", err)
	}
	if err := NewPersistence(kv, "epic-07").SaveRegister(
		msg.NewBoardRegister("pA", "wkr-07", 1)); err != nil {
		t.Fatalf("SaveRegister: %v", err)
	}

	sevens := New(0)
	if _, regs, err := NewPersistence(kv, "epic-07").Restore(sevens); err != nil || regs != 1 {
		t.Fatalf("Restore(epic-07) = regs %d, err %v; want 1, nil", regs, err)
	}
	if threads := sevens.Threads(); len(threads) != 1 || threads[0].Root.Text != "sevens only" {
		t.Fatalf("epic-07 restored %v, want only its own post", threads)
	}

	sess := New(0)
	if _, regs, err := NewPersistence(kv, "").Restore(sess); err != nil || regs != 0 {
		t.Fatalf("Restore(session) = regs %d, err %v; want 0, nil — it read a named board's roster",
			regs, err)
	}
	if threads := sess.Threads(); len(threads) != 1 || threads[0].Root.Text != "session only" {
		t.Fatalf("session board restored %v, want only its own post", threads)
	}
}

// TestDropsAreCountedPerBoard: a session-wide drop count rendered on one
// fleet's board is a claim about that fleet which is false, and it is the sort
// of number somebody acts on.
func TestDropsAreCountedPerBoard(t *testing.T) {
	nc := newTestNATS(t)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)

	// A buffer of one, never drained: everything after the first is dropped.
	sub, err := Subscribe(nc, codecs, 1, nil)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(sub.Close)

	for i := 0; i < 4; i++ {
		if err := msg.SendBoardPost(pub, "epic-07", boardPost("pA", "flood", int64(i))); err != nil {
			t.Fatalf("SendBoardPost: %v", err)
		}
	}
	waitFor(t, func() bool { return sub.Dropped() > 0 })

	if got := sub.DroppedFor("epic-07"); got == 0 {
		t.Fatal("epic-07 dropped nothing, but the subscriber dropped something")
	}
	if got := sub.DroppedFor("epic-08"); got != 0 {
		t.Errorf("epic-08 reports %d drops; it was never posted to", got)
	}
	if got := sub.DroppedFor(""); got != 0 {
		t.Errorf("the session board reports %d drops; it was never posted to", got)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}
