package actors

import (
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Tests for ABLA, the always-on board listener.
//
// The headline is TestBoardRecordsWithNoTUIAttached: before this actor it was
// impossible, because the only subscriber and the only KV writer lived in the
// TUI. Every test here runs WITHOUT a terminal — that is deliberate, and it is
// the proof that collection no longer depends on anything being on screen.

const (
	ablaPaneA = "aaaa1111-f8bb-45fb-9d02-2657b16706ae"
	ablaPaneB = "bbbb2222-ff1c-4435-84bc-ab2eed4aa435"
)

func newABLATestNATS(t *testing.T) *nats.Conn {
	t.Helper()
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
	return nc
}

func newABLATestKV(t *testing.T, nc *nats.Conn) nats.KeyValue {
	t.Helper()
	js, err := nc.JetStream()
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	kv, err := js.CreateKeyValue(board.BucketConfig("abla-" + t.Name()))
	if err != nil {
		t.Fatalf("CreateKeyValue: %v", err)
	}
	return kv
}

// waitFor polls until cond holds or the deadline passes. The board is a
// push pipeline with a goroutine in it, so "has it arrived yet" is inherently a
// poll; the alternative is a sleep long enough to be slow and short enough to
// be flaky.
func ablaWaitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func post(paneID, persona, text, threadID string, ts int64) *msg.MsgBoardPost {
	p := msg.NewBoardPost(paneID, persona, msg.BoardKindMilestone, text, ts)
	p.ThreadID = threadID
	return p
}

// TestBoardRecordsWithNoTUIAttached is the acceptance criterion for this order.
//
// There is no TUI in this test and no terminal anywhere near it. An agent posts
// a milestone; the board must have heard it and written it down. Before ABLA
// this was impossible by construction: internal/tui/board_view.go held the only
// subscription and the only persister, so with no board pane open the post went
// nowhere and left no trace.
func TestBoardRecordsWithNoTUIAttached(t *testing.T) {
	nc := newABLATestNATS(t)
	kv := newABLATestKV(t, nc)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)

	abla := NewAgentBoardListenerActor(nc, codecs, kv)
	abla.Start()
	t.Cleanup(abla.Stop)
	if !abla.Listening() {
		t.Fatal("ABLA is not listening after Start")
	}

	thread := msg.MintThreadID(ablaPaneA, 1)
	if err := msg.SendBoardPost(pub, post(ablaPaneA, "mgr-01", "wave 1 done", thread, 100)); err != nil {
		t.Fatalf("SendBoardPost: %v", err)
	}
	if err := msg.SendBoardPost(pub, post(ablaPaneB, "wkr-01", "confirmed", thread, 101)); err != nil {
		t.Fatalf("SendBoardPost: %v", err)
	}

	// 1. It was heard: the in-memory board has it, with no view involved.
	ablaWaitFor(t, "both posts in ABLA's store", func() bool {
		return abla.Store().Stats().Posts == 2
	})
	th := abla.Store().Threads()
	if len(th) != 1 {
		t.Fatalf("want 1 thread, got %d", len(th))
	}
	if th[0].Root == nil || th[0].Root.Text != "wave 1 done" {
		t.Fatalf("root wrong: %+v", th[0].Root)
	}
	if len(th[0].Replies) != 1 || th[0].Replies[0].Text != "confirmed" {
		t.Fatalf("reply did not land under the root: %+v", th[0].Replies)
	}
	if e := abla.WriteErrors(); e != 0 {
		t.Fatalf("%d KV write errors — the board is not actually recording", e)
	}

	// 2. It was written down: a cold reader, with ABLA stopped, sees it all.
	abla.Stop()
	cold := board.New(0)
	posts, _, err := board.NewPersistence(kv).Restore(cold)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if posts != 2 {
		t.Fatalf("want 2 posts durable with no TUI ever attached, got %d", posts)
	}
	cth := cold.Threads()
	if len(cth) != 1 || cth[0].Root == nil || cth[0].Root.Text != "wave 1 done" {
		t.Fatalf("cold restore lost the thread: %+v", cth)
	}
	if len(cth[0].Replies) != 1 {
		t.Fatalf("cold restore lost the reply: %+v", cth[0].Replies)
	}
}

// TestRegistrationHeardBeforeAnyBoardExists is the exact live failure that
// motivated ABLA: two panes announce their personas, and the board showed one
// agent because every announcement was published before a board existed.
//
// With a session-scoped listener the announcements land in the roster whether
// or not anything is watching.
func TestRegistrationHeardBeforeAnyBoardExists(t *testing.T) {
	nc := newABLATestNATS(t)
	kv := newABLATestKV(t, nc)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)

	abla := NewAgentBoardListenerActor(nc, codecs, kv)
	abla.Start()
	t.Cleanup(abla.Stop)

	for _, r := range []*msg.MsgBoardRegister{
		{V: msg.BoardSchemaVersion, PaneID: ablaPaneA, Persona: "mgr-01", TS: 1},
		{V: msg.BoardSchemaVersion, PaneID: ablaPaneB, Persona: "wkr-01", TS: 2},
	} {
		if err := msg.SendBoardRegister(pub, r); err != nil {
			t.Fatalf("SendBoardRegister: %v", err)
		}
	}

	ablaWaitFor(t, "both personas in the roster", func() bool {
		return len(abla.Store().Roster()) == 2
	})

	// And they survive the process: a cold restore rebuilds both.
	abla.Stop()
	cold := board.New(0)
	if _, regs, err := board.NewPersistence(kv).Restore(cold); err != nil || regs != 2 {
		t.Fatalf("want 2 registrations restored, got %d (err %v)", regs, err)
	}
}

// TestStartIsIdempotent: a supervisor restart or a double Start must not open a
// second subscription, or every post is recorded twice.
func TestStartIsIdempotent(t *testing.T) {
	nc := newABLATestNATS(t)
	kv := newABLATestKV(t, nc)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)

	abla := NewAgentBoardListenerActor(nc, codecs, kv)
	abla.Start()
	abla.Start()
	abla.Start()
	t.Cleanup(abla.Stop)

	if err := msg.SendBoardPost(pub, post(ablaPaneA, "mgr-01", "once", "", 200)); err != nil {
		t.Fatalf("SendBoardPost: %v", err)
	}
	ablaWaitFor(t, "the post to arrive", func() bool {
		return abla.Store().Stats().Posts == 1
	})

	// Give any duplicate subscription time to prove itself.
	time.Sleep(200 * time.Millisecond)
	if got := abla.Store().Stats().Posts; got != 1 {
		t.Fatalf("want 1 post after 3 Starts, got %d — Start opened extra subscriptions", got)
	}

	// KV entries, not restored posts: dedup-on-restore would report 1 either
	// way, so only the entry count can see a second subscription writing.
	n, rewrites := postEntries(t, kv)
	if n != 1 || rewrites != 0 {
		t.Fatalf("want 1 KV entry written exactly once after 3 Starts, got %d entries "+
			"with %d rewrite(s) — Start opened a second subscription", n, rewrites)
	}
}

// postEntries reports how many post entries exist, and how many times a key in
// this bucket was written MORE THAN ONCE.
//
// REWRITES ARE THE DETECTOR, and the reason is worth stating because the obvious
// measurements do not work. Two writers recording the same session do NOT
// produce two entries: each board.Persistence keeps its own ordinal, so both
// write post-...0001, and the second Put OVERWRITES the first. A second writer
// is therefore invisible to an entry count, invisible to a restore (Store.Apply
// would dedup identical posts anyway), and invisible to the rendered board —
// while silently destroying history. See TestASecondWriterDestroysHistory.
//
// THE MEASUREMENT IS BUCKET-WIDE, NOT PER KEY, and that is the whole subtlety.
// A JetStream KV revision counts operations on the BUCKET, so under a single
// writer the Nth post carries revision N. An earlier version of this comment
// claimed "a revision above 1 means some key was written more than once, which
// under one writer can never happen" — that is FALSE, and the three tests using
// it passed only because each writes one post. It was one added post away from
// failing against correct code.
//
// What is true: the highest revision in the bucket equals the total number of
// writes to it, so with every key written exactly once it equals the number of
// keys. The excess is the number of rewrites, and a rewrite of a post key is a
// destroyed post.
//
// Precondition, and it is why liveness was taken off this bucket rather than
// moved to another one (design 026 §5.4): NOTHING ELSE WRITES HERE. A caller
// that deleted or purged keys would bump revisions without adding writes and
// would need a different measurement.
func postEntries(t *testing.T, kv nats.KeyValue) (posts int, rewrites uint64) {
	t.Helper()
	keys, err := kv.Keys()
	if err == nats.ErrNoKeysFound {
		return 0, 0
	}
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	var maxRevision uint64
	for _, k := range keys {
		if strings.HasPrefix(k, "post-") {
			posts++
		}
		e, gerr := kv.Get(k)
		if gerr != nil {
			t.Fatalf("Get(%s): %v", k, gerr)
		}
		if r := e.Revision(); r > maxRevision {
			maxRevision = r
		}
	}
	if maxRevision < uint64(len(keys)) {
		t.Fatalf("max revision %d is below the key count %d — the bucket-wide "+
			"revision assumption this helper rests on no longer holds", maxRevision, len(keys))
	}
	return posts, maxRevision - uint64(len(keys))
}

// TestTUISubscriptionDoesNotPersist is the separation itself, from the reader's
// side: a second subscriber standing in for a TUI is created with a NIL
// persister, so only ABLA writes. Both see the post; only one records it.
//
// This is what stops the KV being written twice whenever someone opens a board
// pane, and it is why setupBoardSubscriptions passes nil.
func TestTUISubscriptionDoesNotPersist(t *testing.T) {
	nc := newABLATestNATS(t)
	kv := newABLATestKV(t, nc)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)

	abla := NewAgentBoardListenerActor(nc, codecs, kv)
	abla.Start()
	t.Cleanup(abla.Stop)

	// Exactly what internal/tui/board_view.go now builds: read-only.
	reader, err := board.Subscribe(nc, codecs, 64, nil)
	if err != nil {
		t.Fatalf("Subscribe (reader): %v", err)
	}
	defer reader.Close()
	readerStore := board.New(0)
	go func() {
		for ev := range reader.Events() {
			readerStore.ApplyEvent(ev)
		}
	}()

	if err := msg.SendBoardPost(pub, post(ablaPaneA, "mgr-01", "one post", "", 300)); err != nil {
		t.Fatalf("SendBoardPost: %v", err)
	}

	ablaWaitFor(t, "ABLA to record", func() bool { return abla.Store().Stats().Posts == 1 })
	ablaWaitFor(t, "the reader to render", func() bool { return readerStore.Stats().Posts == 1 })

	// Let a duplicate write show itself if there is one.
	time.Sleep(200 * time.Millisecond)

	// Count KV ENTRIES, not restored posts: a restore would dedup the duplicate
	// away and report 1 either way. See postEntries.
	n, rewrites := postEntries(t, kv)
	if n != 1 || rewrites != 0 {
		t.Fatalf("want 1 KV entry written exactly once, got %d entries with %d rewrite(s) — "+
			"the reader is persisting too, and a second writer overwrites history", n, rewrites)
	}
}

// TestRestoreBeforeSubscribeOrdering: history must be in the store before the
// live tail joins it, or yesterday's milestones interleave with today's.
func TestRestoreBeforeSubscribeOrdering(t *testing.T) {
	nc := newABLATestNATS(t)
	kv := newABLATestKV(t, nc)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)

	// A previous session wrote two posts.
	prior := board.NewPersistence(kv)
	for i, text := range []string{"yesterday one", "yesterday two"} {
		if err := prior.SavePost(post(ablaPaneA, "mgr-01", text, "", int64(i))); err != nil {
			t.Fatalf("SavePost: %v", err)
		}
	}

	// The daemon restarts.
	abla := NewAgentBoardListenerActor(nc, codecs, kv)
	abla.Start()
	t.Cleanup(abla.Stop)

	ablaWaitFor(t, "history to load", func() bool { return abla.Store().Stats().Posts == 2 })

	if err := msg.SendBoardPost(pub, post(ablaPaneB, "wkr-01", "today", "", 500)); err != nil {
		t.Fatalf("SendBoardPost: %v", err)
	}
	ablaWaitFor(t, "the live post", func() bool { return abla.Store().Stats().Posts == 3 })

	th := abla.Store().Threads()
	if len(th) != 3 {
		t.Fatalf("want 3 threads, got %d", len(th))
	}
	want := []string{"yesterday one", "yesterday two", "today"}
	for i, w := range want {
		if th[i].Root == nil || th[i].Root.Text != w {
			t.Fatalf("position %d is %+v, want %q — history and the live tail interleaved",
				i, th[i].Root, w)
		}
	}
}

// TestASecondWriterDestroysHistory is why ABLA must be a singleton, and it is
// NOT the tidiness concern it looks like.
//
// Two board.Persistence instances each keep their own arrival ordinal, so both
// write post-...0001. The second Put does not add an entry — it OVERWRITES the
// first at revision 2. Writer A's post is then gone: not duplicated, not
// merged, GONE, with nothing anywhere reporting a loss.
//
// This is the real cost of a second writer, and it is worse than the "wasted
// disk" it would be if duplicates simply piled up. It is why the TUI passes a
// nil persister and why cmd/rysh spawns ABLA with SpawnNamed rather than Spawn.
func TestASecondWriterDestroysHistory(t *testing.T) {
	nc := newABLATestNATS(t)
	kv := newABLATestKV(t, nc)

	a := board.NewPersistence(kv)
	b := board.NewPersistence(kv)
	if err := a.SavePost(post(ablaPaneA, "A", "post from writer A", "", 1)); err != nil {
		t.Fatalf("SavePost: %v", err)
	}
	if err := b.SavePost(post(ablaPaneB, "B", "post from writer B", "", 2)); err != nil {
		t.Fatalf("SavePost: %v", err)
	}

	count, rewrites := postEntries(t, kv)
	if count != 1 || rewrites != 1 {
		t.Fatalf("expected the clobber to show as 1 entry rewritten once, got %d entries "+
			"with %d rewrite(s) — if this changed, the key scheme changed and the hazard "+
			"note in abla.go needs revisiting", count, rewrites)
	}

	store := board.New(0)
	posts, _, err := board.NewPersistence(kv).Restore(store)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if posts != 1 {
		t.Fatalf("want 1 surviving post (the loss), got %d", posts)
	}
	if got := store.Threads()[0].Root.Text; got != "post from writer B" {
		t.Fatalf("survivor is %q; the point of this test is that A was destroyed", got)
	}
}

// TestASingleWriterAtSeveralPostsIsNotAClobber is the arrangement the previous
// detector could not survive, and it is why this test exists rather than only a
// corrected comment.
//
// One writer, three posts: the entries carry revisions 1, 2 and 3, because a
// JetStream KV revision is BUCKET-WIDE and not per key. The old rule — "a
// revision above 1 means some key was written more than once, which under one
// writer can never happen" — reads that as a second writer and fails against
// entirely correct code. The three tests that used it passed only because each
// of them writes exactly one post, so the bug was one added post away.
//
// A test that passes is not evidence that the reason written above it is true;
// this one attacks the reason.
func TestASingleWriterAtSeveralPostsIsNotAClobber(t *testing.T) {
	nc := newABLATestNATS(t)
	kv := newABLATestKV(t, nc)

	p := board.NewPersistence(kv)
	for i, text := range []string{"one", "two", "three"} {
		if err := p.SavePost(post(ablaPaneA, "A", text, "", int64(i+1))); err != nil {
			t.Fatalf("SavePost(%q): %v", text, err)
		}
	}

	count, rewrites := postEntries(t, kv)
	if count != 3 || rewrites != 0 {
		t.Fatalf("one writer at three posts: want 3 entries and 0 rewrites, got %d entries "+
			"with %d rewrite(s) — a busy single writer must not read as a second one",
			count, rewrites)
	}

	store := board.New(0)
	posts, _, err := board.NewPersistence(kv).Restore(store)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if posts != 3 {
		t.Fatalf("want all 3 posts to survive a single writer, got %d", posts)
	}
}

// TestNilConnDegradesQuietly: no NATS means no feed, not a panic and not a
// session that refuses to start.
func TestNilConnDegradesQuietly(t *testing.T) {
	abla := NewAgentBoardListenerActor(nil, msg.DefaultCodecRegistry(), nil)
	abla.Start()
	if abla.Listening() {
		t.Fatal("claims to be listening with no connection")
	}
	if abla.Store() == nil {
		t.Fatal("Store must be usable even with no connection")
	}
	if abla.Dropped() != 0 || abla.WriteErrors() != 0 {
		t.Fatal("counters should be zero with no subscription")
	}
	abla.Stop()
	abla.Stop() // idempotent
}

// TestStopIsIdempotentAndStopsRecording.
func TestStopIsIdempotentAndStopsRecording(t *testing.T) {
	nc := newABLATestNATS(t)
	kv := newABLATestKV(t, nc)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)

	abla := NewAgentBoardListenerActor(nc, codecs, kv)
	abla.Start()
	_ = msg.SendBoardPost(pub, post(ablaPaneA, "mgr-01", "before stop", "", 700))
	ablaWaitFor(t, "the first post", func() bool { return abla.Store().Stats().Posts == 1 })

	abla.Stop()
	abla.Stop()
	if abla.Listening() {
		t.Fatal("still listening after Stop")
	}

	_ = msg.SendBoardPost(pub, post(ablaPaneA, "mgr-01", "after stop", "", 701))
	_ = nc.Flush()
	time.Sleep(200 * time.Millisecond)

	cold := board.New(0)
	posts, _, err := board.NewPersistence(kv).Restore(cold)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if posts != 1 {
		t.Fatalf("want 1 recorded post (the one before Stop), got %d", posts)
	}
}

// TestABLAAnswersLivenessWhileRecording proves the question is answered by the
// RECORDER rather than inferred from a trace it left, and that a stopped
// recorder goes quiet instead of continuing to claim it records.
//
// This replaced a KV heartbeat. The heartbeat needed a staleness threshold that
// could be wrong under load, and it wrote a key into the board's bucket — whose
// single-writer detector above depends on nothing else writing there. Taking
// liveness off the bucket restored that precondition rather than working around
// it.
func TestABLAAnswersLivenessWhileRecording(t *testing.T) {
	nc := newABLATestNATS(t)
	kv := newABLATestKV(t, nc)
	a := NewAgentBoardListenerActor(nc, msg.DefaultCodecRegistry(), kv)

	// Before Start, nobody is recording and nobody must answer.
	if _, err := nc.Request(msg.BoardAliveSubject(), nil, 300*time.Millisecond); err == nil {
		t.Fatal("an unstarted recorder answered a liveness request — a view would " +
			"render a confident empty board on the strength of it")
	}

	a.Start()
	t.Cleanup(a.Stop)

	reply, err := nc.Request(msg.BoardAliveSubject(), nil, 2*time.Second)
	if err != nil {
		t.Fatalf("a live recorder did not answer: %v", err)
	}
	if got := string(reply.Data); got != msg.BoardAliveReply {
		t.Errorf("reply = %q, want %q", got, msg.BoardAliveReply)
	}

	// A stopped recorder must go quiet. Timing out is the honest answer.
	a.Stop()
	if _, err := nc.Request(msg.BoardAliveSubject(), nil, 300*time.Millisecond); err == nil {
		t.Fatal("a STOPPED recorder still answered — that is the confident receipt " +
			"failure in its purest form: a claim of health from something that is dead")
	}
}
