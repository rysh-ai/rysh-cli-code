// SPDX-License-Identifier: Apache-2.0

package board

import (
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// newTestNATS starts an in-process NATS server with JetStream and returns a
// connected client. Modelled on internal/tools/rysh_command_test.go:20-38; the
// JetStream options are the addition, because the board persists (gate 2).
//
// This is a REAL server and a REAL connection — the end-to-end tests below
// publish through msg.SendBoardPost exactly as an agent would. Nothing is faked.
func newTestNATS(t *testing.T) *nats.Conn {
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

func newTestKV(t *testing.T, nc *nats.Conn) nats.KeyValue {
	t.Helper()
	js, err := nc.JetStream()
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	kv, err := js.CreateKeyValue(BucketConfig("test-" + t.Name()))
	if err != nil {
		t.Fatalf("CreateKeyValue: %v", err)
	}
	return kv
}

func waitEvent(t *testing.T, s *Subscriber) Event {
	t.Helper()
	select {
	case ev := <-s.Events():
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no board event within 5s")
		return Event{}
	}
}

// TestSubscriberEndToEnd publishes through the real publisher onto a real NATS
// connection and asserts the store received it, threaded. No fakes: this is the
// wire the agents will actually use.
func TestSubscriberEndToEnd(t *testing.T) {
	nc := newTestNATS(t)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)

	sub, err := Subscribe(nc, codecs, 64, nil)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	store := New(0)
	thread := msg.MintThreadID(paneA, 1)

	root := msg.NewBoardPost(paneA, "mgr-01", msg.BoardKindMilestone, "wave 1 done", 1)
	root.ThreadID = thread
	if err := msg.SendBoardPost(pub, "", root); err != nil {
		t.Fatalf("SendBoardPost: %v", err)
	}
	store.ApplyEvent(waitEvent(t, sub))

	reply := msg.NewBoardPost(paneB, "wkr-01", msg.BoardKindReply, "confirmed", 2)
	reply.ThreadID = thread
	if err := msg.SendBoardPost(pub, "", reply); err != nil {
		t.Fatalf("SendBoardPost: %v", err)
	}
	store.ApplyEvent(waitEvent(t, sub))

	if err := msg.SendBoardRegister(pub, "", &msg.MsgBoardRegister{
		V: msg.BoardSchemaVersion, PaneID: paneB, Persona: "wkr-01", TS: 3,
	}); err != nil {
		t.Fatalf("SendBoardRegister: %v", err)
	}
	store.ApplyEvent(waitEvent(t, sub))

	got := store.Threads()
	if len(got) != 1 {
		t.Fatalf("want 1 thread over the wire, got %d", len(got))
	}
	if got[0].Root == nil || got[0].Root.Text != "wave 1 done" {
		t.Fatalf("root did not survive the wire: %+v", got[0].Root)
	}
	if len(got[0].Replies) != 1 || got[0].Replies[0].Text != "confirmed" {
		t.Fatalf("reply did not land under the root: %+v", got[0].Replies)
	}
	if r := store.Roster(); len(r) != 1 || r[0].Persona != "wkr-01" {
		t.Fatalf("registration did not survive the wire: %+v", r)
	}
	if d := sub.Dropped(); d != 0 {
		t.Fatalf("dropped %d events with a 64-slot buffer", d)
	}
}

// TestSubscriberDropsAreCountedNotHidden: a full buffer must never block the
// NATS callback, so it drops — but it must never drop silently.
func TestSubscriberDropsAreCountedNotHidden(t *testing.T) {
	nc := newTestNATS(t)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)

	sub, err := Subscribe(nc, codecs, 1, nil) // buffer of one, never drained
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	const n = 50
	for i := 0; i < n; i++ {
		p := msg.NewBoardPost(paneA, "a", msg.BoardKindMilestone, "burst", int64(i))
		if err := msg.SendBoardPost(pub, "", p); err != nil {
			t.Fatalf("SendBoardPost: %v", err)
		}
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && sub.Dropped() < uint64(n-1) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := sub.Dropped(); got != uint64(n-1) {
		t.Fatalf("want %d counted drops (buffer of 1, %d published), got %d",
			n-1, n, got)
	}
}

// TestPersistenceSurvivesRestart is founder gate 2: the board survives a daemon
// restart. A fresh Store, restored from the same KV, must rebuild the same
// threads in the same order.
func TestPersistenceSurvivesRestart(t *testing.T) {
	nc := newTestNATS(t)
	kv := newTestKV(t, nc)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)

	sub, err := Subscribe(nc, codecs, 64, SingleBoardPersistence("", NewPersistence(kv, "")))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	live := New(0)
	thread := msg.MintThreadID(paneA, 1)

	root := msg.NewBoardPost(paneA, "mgr-01", msg.BoardKindMilestone, "root text", 10)
	root.ThreadID = thread
	_ = msg.SendBoardPost(pub, "", root)
	live.ApplyEvent(waitEvent(t, sub))

	reply := msg.NewBoardPost(paneB, "wkr-01", msg.BoardKindReply, "reply text", 11)
	reply.ThreadID = thread
	_ = msg.SendBoardPost(pub, "", reply)
	live.ApplyEvent(waitEvent(t, sub))

	_ = msg.SendBoardRegister(pub, "", &msg.MsgBoardRegister{
		V: msg.BoardSchemaVersion, PaneID: paneA, Persona: "mgr-01", TS: 12,
	})
	live.ApplyEvent(waitEvent(t, sub))

	if got := sub.WriteErrors(); got != 0 {
		t.Fatalf("%d KV write errors — the board is not actually persisting", got)
	}
	sub.Close() // the "daemon" stops

	// The "daemon" restarts: brand-new store, same bucket.
	restored := New(0)
	posts, regs, err := NewPersistence(kv, "").Restore(restored)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if posts != 2 || regs != 1 {
		t.Fatalf("want 2 posts / 1 registration restored, got %d / %d", posts, regs)
	}

	got := restored.Threads()
	if len(got) != 1 {
		t.Fatalf("want 1 rebuilt thread, got %d", len(got))
	}
	if got[0].Provisional {
		t.Fatal("restored thread is provisional — replay lost the root or its order")
	}
	if got[0].Root.Text != "root text" {
		t.Fatalf("restored root wrong: %q", got[0].Root.Text)
	}
	if len(got[0].Replies) != 1 || got[0].Replies[0].Text != "reply text" {
		t.Fatalf("restored replies wrong: %+v", got[0].Replies)
	}
	if r := restored.Roster(); len(r) != 1 || r[0].Persona != "mgr-01" {
		t.Fatalf("restored roster wrong: %+v", r)
	}
}

// TestPersistenceOutlivesARenderDrop is the precise claim persist.go makes: a
// post dropped by a full render buffer is NOT lost, because the KV write is
// UNCONDITIONAL. If the write is ever made conditional on the hand-off
// succeeding — the natural-looking "only persist what we enqueued" — this goes
// red. (Merely reordering the two statements does not, and must not: push()
// does not return early, so both orders persist everything.)
func TestPersistenceOutlivesARenderDrop(t *testing.T) {
	nc := newTestNATS(t)
	kv := newTestKV(t, nc)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)

	// Buffer of 1, never drained: every post after the first is dropped live.
	sub, err := Subscribe(nc, codecs, 1, SingleBoardPersistence("", NewPersistence(kv, "")))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	const n = 10
	for i := 0; i < n; i++ {
		p := msg.NewBoardPost(paneA, "a", msg.BoardKindMilestone, "post", int64(i))
		_ = msg.SendBoardPost(pub, "", p)
	}
	_ = nc.Flush()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && sub.Dropped() < uint64(n-1) {
		time.Sleep(10 * time.Millisecond)
	}
	if sub.Dropped() == 0 {
		t.Fatal("test setup broken: nothing was dropped, so nothing is being proved")
	}

	restored := New(0)
	posts, _, err := NewPersistence(kv, "").Restore(restored)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if posts != n {
		t.Fatalf("want all %d posts durable despite %d render drops, got %d — "+
			"the KV write is no longer unconditional",
			n, sub.Dropped(), posts)
	}
}

// TestRestoreOnEmptyBucketIsNotAnError: a first run has no history, and that is
// the normal case, not a failure to start.
func TestRestoreOnEmptyBucketIsNotAnError(t *testing.T) {
	nc := newTestNATS(t)
	kv := newTestKV(t, nc)

	s := New(0)
	posts, regs, err := NewPersistence(kv, "").Restore(s)
	if err != nil {
		t.Fatalf("Restore on an empty bucket: %v", err)
	}
	if posts != 0 || regs != 0 {
		t.Fatalf("want nothing restored, got %d / %d", posts, regs)
	}
}

// TestNilPersistenceIsLiveOnly: no JetStream means a live-only board, not a
// crash and not a refusal to start.
func TestNilPersistenceIsLiveOnly(t *testing.T) {
	p := NewPersistence(nil, "")
	if p != nil {
		t.Fatal("NewPersistence(nil, ...) should yield a nil Persistence")
	}
	if err := p.SavePost(msg.NewBoardPost(paneA, "a", "", "x", 1)); err != nil {
		t.Fatalf("nil Persistence SavePost: %v", err)
	}
	if err := p.SaveRegister(&msg.MsgBoardRegister{PaneID: paneA}); err != nil {
		t.Fatalf("nil Persistence SaveRegister: %v", err)
	}
	posts, regs, err := p.Restore(New(0))
	if err != nil || posts != 0 || regs != 0 {
		t.Fatalf("nil Persistence Restore: %d/%d %v", posts, regs, err)
	}
}

// TestRestoreContinuesTheOrdinal: a restart must not rewrite keys that are still
// live in the bucket, or the second session would overwrite the first's history.
func TestRestoreContinuesTheOrdinal(t *testing.T) {
	nc := newTestNATS(t)
	kv := newTestKV(t, nc)

	first := NewPersistence(kv, "")
	for i := 0; i < 3; i++ {
		if err := first.SavePost(msg.NewBoardPost(paneA, "a", "", "old", int64(i))); err != nil {
			t.Fatalf("SavePost: %v", err)
		}
	}

	second := NewPersistence(kv, "")
	if _, _, err := second.Restore(New(0)); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if err := second.SavePost(msg.NewBoardPost(paneB, "b", "", "new", 99)); err != nil {
		t.Fatalf("SavePost after restore: %v", err)
	}

	final := New(0)
	posts, _, err := NewPersistence(kv, "").Restore(final)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if posts != 4 {
		t.Fatalf("want 4 posts (3 old + 1 new), got %d — the restart reused an "+
			"ordinal and overwrote history", posts)
	}
	th := final.Threads()
	if th[len(th)-1].Root.Text != "new" {
		t.Fatalf("the post written after the restart is not last: %q",
			th[len(th)-1].Root.Text)
	}
}
