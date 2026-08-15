// SPDX-License-Identifier: Apache-2.0

package board

import (
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Two realistic pane uuids. They matter: the board's identity key is the FULL
// pane uuid, never the persona and never the 8-char truncation the fleet
// envelope carries.
const (
	paneA = "aaaa1111-f8bb-45fb-9d02-2657b16706ae"
	paneB = "bbbb2222-ff1c-4435-84bc-ab2eed4aa435"
	paneC = "cccc3333-26c7-434e-aea3-f90241e3dffe"
)

func post(paneID, persona, kind, text, threadID string, ts int64) *msg.MsgBoardPost {
	p := msg.NewBoardPost(paneID, persona, kind, text, ts)
	p.ThreadID = threadID
	return p
}

// TestThreadingRoundTrip: two panes open roots, one replies to the other's
// root. The reply must land UNDER that root, not become a third root.
//
// It also pins the wave-1 finding that changed the schema: given-names are
// unique per LANE, not per session (TabActor.IsGivenNameTakenInLane), so two
// panes may legally share one. Both panes here are called "epic01" and they
// must stay two agents.
func TestThreadingRoundTrip(t *testing.T) {
	s := New(0)

	threadA := msg.MintThreadID(paneA, 1)
	threadB := msg.MintThreadID(paneB, 1)

	if !s.Apply(post(paneA, "epic01", msg.BoardKindMilestone, "A opens", threadA, 100)) {
		t.Fatal("root A rejected")
	}
	if !s.Apply(post(paneB, "epic01", msg.BoardKindMilestone, "B opens", threadB, 101)) {
		t.Fatal("root B rejected")
	}
	if !s.Apply(post(paneB, "epic01", msg.BoardKindReply, "B replies to A", threadA, 102)) {
		t.Fatal("reply rejected")
	}

	got := s.Threads()
	if len(got) != 2 {
		t.Fatalf("want 2 threads, got %d — a reply became a root", len(got))
	}
	if got[0].Key != threadA || got[1].Key != threadB {
		t.Fatalf("threads out of arrival order: %q then %q", got[0].Key, got[1].Key)
	}
	if got[0].Provisional || got[1].Provisional {
		t.Fatalf("no thread should be provisional: %+v", got)
	}
	if len(got[0].Replies) != 1 {
		t.Fatalf("thread A: want 1 reply, got %d", len(got[0].Replies))
	}
	if len(got[1].Replies) != 0 {
		t.Fatalf("thread B: want 0 replies, got %d", len(got[1].Replies))
	}
	if got[0].Replies[0].Text != "B replies to A" {
		t.Fatalf("wrong reply under A: %q", got[0].Replies[0].Text)
	}

	// Identity is the pane uuid, not the shared display name.
	if got[0].Root.PaneID != paneA {
		t.Fatalf("root A attributed to %q", got[0].Root.PaneID)
	}
	if got[0].Replies[0].PaneID != paneB {
		t.Fatalf("reply attributed to %q, want %q — two agents sharing a "+
			"given-name were merged into one", got[0].Replies[0].PaneID, paneB)
	}
	if got[0].Root.Persona != got[0].Replies[0].Persona {
		t.Fatal("test setup broken: both panes must share a persona for this to prove anything")
	}
}

// TestOrphanReparenting is the hard one. Thread ids are minted agent-side with
// no round trip (design 025 §4.3), so a reply can arrive BEFORE its root.
//
// Before the root: the orphan renders as a provisional root.
// After the root: exactly ONE thread, not two, with the reply under the root —
// and the thread sits where the ROOT's arrival puts it, not where the early
// orphan landed.
func TestOrphanReparenting(t *testing.T) {
	s := New(0)
	threadA := msg.MintThreadID(paneA, 7)

	// 1. The reply arrives first.
	s.Apply(post(paneB, "wkr-01", msg.BoardKindReply, "done, boss", threadA, 200))

	got := s.Threads()
	if len(got) != 1 {
		t.Fatalf("want 1 provisional thread, got %d", len(got))
	}
	if !got[0].Provisional {
		t.Fatal("an orphan reply must render as a PROVISIONAL root")
	}
	if got[0].Root != nil {
		t.Fatal("provisional thread must have no root")
	}
	if len(got[0].Replies) != 1 {
		t.Fatalf("orphan lost: want 1 reply, got %d", len(got[0].Replies))
	}

	// 2. An unrelated post lands in between, so the root's arrival position is
	//    observable rather than coincidental.
	s.Apply(post(paneC, "ceo", msg.BoardKindMilestone, "unrelated", "", 201))

	// 3. The real root finally arrives.
	s.Apply(post(paneA, "mgr-01", msg.BoardKindMilestone, "start the thing", threadA, 202))

	got = s.Threads()
	if len(got) != 2 {
		t.Fatalf("want 2 threads (the re-parented one + the unrelated post), got %d — "+
			"the root was duplicated instead of adopting its orphan", len(got))
	}
	// Arrival order: the unrelated post (seq 2) precedes the re-parented thread,
	// whose position is now its ROOT's arrival (seq 3).
	if got[0].Root == nil || got[0].Root.PaneID != paneC {
		t.Fatalf("want the unrelated post first, got %+v", got[0])
	}
	th := got[1]
	if th.Key != threadA {
		t.Fatalf("re-parented thread has key %q, want %q", th.Key, threadA)
	}
	if th.Provisional {
		t.Fatal("thread is still provisional after its root arrived")
	}
	if th.Root == nil || th.Root.Text != "start the thing" {
		t.Fatalf("root not adopted: %+v", th.Root)
	}
	if len(th.Replies) != 1 || th.Replies[0].Text != "done, boss" {
		t.Fatalf("orphan not re-parented under its root: %+v", th.Replies)
	}

	st := s.Stats()
	if st.Provisional != 0 {
		t.Fatalf("want 0 provisional threads after re-parenting, got %d", st.Provisional)
	}
	if st.Posts != 3 {
		t.Fatalf("want 3 posts held, got %d", st.Posts)
	}
}

// TestThreadOpenerRepliesToOwnThread: a second post from the thread's opener
// under its own key is a reply, not a second root. A thread has exactly one.
func TestThreadOpenerRepliesToOwnThread(t *testing.T) {
	s := New(0)
	k := msg.MintThreadID(paneA, 1)
	s.Apply(post(paneA, "mgr-01", msg.BoardKindMilestone, "root", k, 300))
	s.Apply(post(paneA, "mgr-01", msg.BoardKindReply, "and another thing", k, 301))

	got := s.Threads()
	if len(got) != 1 {
		t.Fatalf("want 1 thread, got %d", len(got))
	}
	if got[0].Root == nil {
		t.Fatalf("the opener's own post was not taken as the root: %+v", got[0])
	}
	if got[0].Root.Text != "root" {
		t.Fatalf("root replaced: %q", got[0].Root.Text)
	}
	if len(got[0].Replies) != 1 {
		t.Fatalf("want 1 reply, got %d", len(got[0].Replies))
	}
}

// TestStandalonePostIsItsOwnRoot: a post with no ThreadID is a root that can
// never be replied to, and two of them never collapse into one thread.
func TestStandalonePostIsItsOwnRoot(t *testing.T) {
	s := New(0)
	s.Apply(post(paneA, "a", msg.BoardKindMilestone, "one", "", 400))
	s.Apply(post(paneA, "a", msg.BoardKindMilestone, "two", "", 401))

	got := s.Threads()
	if len(got) != 2 {
		t.Fatalf("want 2 standalone threads, got %d", len(got))
	}
	if got[0].Key == got[1].Key {
		t.Fatalf("standalone posts share a key %q", got[0].Key)
	}
	for i, th := range got {
		if th.Provisional || th.Root == nil {
			t.Fatalf("standalone thread %d must be a real root: %+v", i, th)
		}
	}
}

// TestIdempotency: the same post delivered twice is one entry. Agents retry.
func TestIdempotency(t *testing.T) {
	s := New(0)
	p := post(paneA, "a", msg.BoardKindMilestone, "shipped", "", 500)

	if !s.Apply(p) {
		t.Fatal("first delivery rejected")
	}
	if s.Apply(p) {
		t.Fatal("second delivery of the SAME post was accepted — not idempotent")
	}
	// A separately constructed but identical message is the same message on the
	// wire; it must dedup too.
	if s.Apply(post(paneA, "a", msg.BoardKindMilestone, "shipped", "", 500)) {
		t.Fatal("an identical re-marshalled post was accepted twice")
	}

	if got := len(s.Threads()); got != 1 {
		t.Fatalf("want 1 thread, got %d", got)
	}
	if st := s.Stats(); st.Posts != 1 || st.Duplicates != 2 {
		t.Fatalf("want 1 post / 2 duplicates, got %d / %d", st.Posts, st.Duplicates)
	}
}

// TestIdempotencyLimitIsRealAndDocumented pins the limit stated in dedupKey's
// comment: a producer that REBUILDS the message on retry with a fresh clock
// defeats dedup. This test exists so the limit cannot be quietly "fixed" into a
// claim the store does not actually make.
func TestIdempotencyLimitIsRealAndDocumented(t *testing.T) {
	s := New(0)
	s.Apply(post(paneA, "a", msg.BoardKindMilestone, "shipped", "", 600))
	s.Apply(post(paneA, "a", msg.BoardKindMilestone, "shipped", "", 601)) // fresh clock

	if got := s.Stats().Posts; got != 2 {
		t.Fatalf("want 2 posts (dedup is by identity, NOT by content), got %d", got)
	}
}

// TestUnknownVersionIsKept: a post from a newer agent is rendered, counted, and
// never dropped.
func TestUnknownVersionIsKept(t *testing.T) {
	s := New(0)
	p := post(paneA, "a", "some-future-kind", "hello from v99", "", 700)
	p.V = 99
	if !s.Apply(p) {
		t.Fatal("a post with an unknown schema version was dropped")
	}
	st := s.Stats()
	if st.Posts != 1 {
		t.Fatalf("want 1 post, got %d", st.Posts)
	}
	if st.UnknownVersion != 1 {
		t.Fatalf("want UnknownVersion 1, got %d", st.UnknownVersion)
	}
	if vc := s.VersionCounts(); vc[99] != 1 {
		t.Fatalf("want VersionCounts[99] == 1, got %v", vc)
	}
	if got := s.Threads()[0].Root.Kind; got != "some-future-kind" {
		t.Fatalf("unknown Kind was not rendered as-is: %q", got)
	}
}

// TestRegistrationIsAdvisory: a post from a pane that never registered is still
// rendered. Registration only fills the roster.
func TestRegistrationIsAdvisory(t *testing.T) {
	s := New(0)
	s.Apply(post(paneA, "unregistered", msg.BoardKindMilestone, "still shown", "", 800))
	if got := len(s.Threads()); got != 1 {
		t.Fatalf("post from an unregistered pane was dropped: %d threads", got)
	}
	if got := len(s.Roster()); got != 0 {
		t.Fatalf("posting must not populate the roster: %d entries", got)
	}

	s.Register(&msg.MsgBoardRegister{
		V: msg.BoardSchemaVersion, PaneID: paneB, Persona: "wkr-01", TS: 801,
	})
	r := s.Roster()
	if len(r) != 1 || r[0].PaneID != paneB || r[0].Persona != "wkr-01" {
		t.Fatalf("roster wrong: %+v", r)
	}

	// Last announcement wins — a renamed pane re-announces.
	s.Register(&msg.MsgBoardRegister{
		V: msg.BoardSchemaVersion, PaneID: paneB, Persona: "wkr-01-renamed", TS: 802,
	})
	if r = s.Roster(); len(r) != 1 || r[0].Persona != "wkr-01-renamed" {
		t.Fatalf("re-registration did not replace: %+v", r)
	}
}

// TestPersonaSanitisedInStore: approval panes overload GivenName with
// "requestID\x1FresponseSubject" (internal/actors/approval_pane.go). A producer
// that reads a given-name naively can put that on the wire; the store must not
// hand a NATS subject to a view as somebody's name.
func TestPersonaSanitisedInStore(t *testing.T) {
	s := New(0)
	s.Apply(post(paneA, "req-42\x1frysh.approval.reply", msg.BoardKindMilestone, "x", "", 900))
	s.Apply(post(paneB, "", msg.BoardKindMilestone, "y", "", 901))

	got := s.Threads()
	if want := "pane-" + paneA[:8]; got[0].Root.Persona != want {
		t.Fatalf("unit-separator persona survived: %q, want %q", got[0].Root.Persona, want)
	}
	if want := "pane-" + paneB[:8]; got[1].Root.Persona != want {
		t.Fatalf("empty persona not filled: %q, want %q", got[1].Root.Persona, want)
	}
}

// TestEvictionIsWholeThreadsOldestFirst: the store is bounded, and it evicts by
// whole thread so a surviving reply never becomes an orphan of a root that was
// silently removed.
func TestEvictionIsWholeThreadsOldestFirst(t *testing.T) {
	s := New(2)
	k := msg.MintThreadID(paneA, 1)
	s.Apply(post(paneA, "a", msg.BoardKindMilestone, "old root", k, 1000))
	s.Apply(post(paneB, "b", msg.BoardKindReply, "old reply", k, 1001))
	// The thread now holds 2 posts, exactly the cap.
	if st := s.Stats(); st.Posts != 2 || st.Evicted != 0 {
		t.Fatalf("premature eviction: %+v", st)
	}

	s.Apply(post(paneC, "c", msg.BoardKindMilestone, "new", "", 1002))

	got := s.Threads()
	if len(got) != 1 {
		t.Fatalf("want 1 surviving thread, got %d", len(got))
	}
	if got[0].Root.Text != "new" {
		t.Fatalf("evicted the wrong (newest) thread: %q survived", got[0].Root.Text)
	}
	st := s.Stats()
	if st.Evicted != 2 {
		t.Fatalf("want 2 evicted posts (the whole old thread), got %d", st.Evicted)
	}
	if st.Posts != 1 {
		t.Fatalf("want 1 post held, got %d", st.Posts)
	}

	// An evicted post's dedup key is forgotten with it, so the same post can be
	// re-filed later rather than being silently swallowed forever.
	if !s.Apply(post(paneA, "a", msg.BoardKindMilestone, "old root", k, 1000)) {
		t.Fatal("dedup key outlived its evicted thread — the dedup set is unbounded")
	}
}

func TestOwnsThread(t *testing.T) {
	cases := []struct {
		pane, thread string
		want         bool
	}{
		{paneA, msg.MintThreadID(paneA, 1), true},
		{paneA, msg.MintThreadID(paneA, 42), true},
		{paneA, msg.MintThreadID(paneB, 1), false},
		{paneA, paneA, false},      // no separator, no ordinal
		{paneA, paneA + "/", false}, // separator but no ordinal
		{"", "anything/1", false},
		{paneA, "", false},
	}
	for _, c := range cases {
		if got := ownsThread(c.pane, c.thread); got != c.want {
			t.Errorf("ownsThread(%q, %q) = %v, want %v", c.pane, c.thread, got, c.want)
		}
	}
}

func TestApplyNilIsSafe(t *testing.T) {
	s := New(0)
	if s.Apply(nil) {
		t.Fatal("nil post accepted")
	}
	s.Register(nil)
	s.ApplyEvent(Event{})
	if st := s.Stats(); st.Posts != 0 || st.Threads != 0 {
		t.Fatalf("nil input mutated the store: %+v", st)
	}
}
