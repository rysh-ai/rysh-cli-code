// SPDX-License-Identifier: Apache-2.0

package peerfeed

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/peer"
)

// N3 (design 044 amendment 2026-10-20, root-order-051): a local board the
// user explicitly published is sent to the peer board — forward-only, text
// only, idempotent, revocable at once, on exact subjects.

func inProcessNATS(t *testing.T) *nats.Conn {
	t.Helper()
	ns, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true, DontListen: true})
	if err != nil {
		t.Fatal(err)
	}
	ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats not ready")
	}
	t.Cleanup(ns.Shutdown)
	nc, err := nats.Connect("", nats.InProcessServer(ns))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	return nc
}

// peerSink records the posts the publisher sends; down makes it refuse.
type peerSink struct {
	mu    sync.Mutex
	posts []peer.Post
	down  bool
}

func (s *peerSink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer rysh_pub_test_key" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if s.down {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/peer-boards/team/messages") {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	var p peer.Post
	json.NewDecoder(r.Body).Decode(&p)
	s.posts = append(s.posts, p)
	w.Header().Set("Content-Type", "application/json")
	// Answer like the server: the client only trusts a receipt that names
	// its workspace, board and message id.
	ws := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/workspaces/"), "/peer-boards/team/messages")
	json.NewEncoder(w).Encode(peer.Receipt{Message: peer.Message{WorkspaceID: ws, Board: "team", ID: p.ID, ThreadID: p.ID, Sequence: int64(len(s.posts)), Text: p.Text}})
}

func (s *peerSink) sent() []peer.Post {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]peer.Post(nil), s.posts...)
}

type pubFixture struct {
	t    *testing.T
	nc   *nats.Conn
	sink *peerSink
	p    *Publisher
	root string
}

func newPublisher(t *testing.T) *pubFixture {
	t.Helper()
	nc := inProcessNATS(t)
	sink := &peerSink{}
	server := httptest.NewServer(sink)
	t.Cleanup(server.Close)
	root := t.TempDir()
	if err := peer.Save(root, "pbw-a", peer.Connection{Enabled: true, URL: server.URL, Workspace: uuid.NewString(), Board: "team", KeySecret: "PUB_KEY", SessionID: mySession}); err != nil {
		t.Fatal(err)
	}
	p := &Publisher{Root: root, Session: "pbw-a", NC: nc, Codecs: msg.DefaultCodecRegistry(), Max: 16,
		Key: func(string) string { return "rysh_pub_test_key" }}
	t.Cleanup(p.Close)
	return &pubFixture{t: t, nc: nc, sink: sink, p: p, root: root}
}

func (f *pubFixture) publishSetting(boards map[string]peer.Publication) {
	f.t.Helper()
	if err := peer.SavePublications(f.root, "pbw-a", peer.Publications{Boards: boards}); err != nil {
		f.t.Fatal(err)
	}
	if err := f.p.Reconcile(); err != nil {
		f.t.Fatal(err)
	}
}

// post publishes a local board post and waits until the publisher has it
// queued (or until it is clear nothing will be queued).
func (f *pubFixture) post(board string, p *msg.MsgBoardPost, wantQueued bool) {
	f.t.Helper()
	before := f.p.Pending()
	data, _ := json.Marshal(p)
	env, _ := json.Marshal(msg.NATSEnvelope{TypeTag: msg.TagBoardPost, Payload: data})
	if err := f.nc.Publish(msg.BoardPostSubject(board), env); err != nil {
		f.t.Fatal(err)
	}
	f.nc.Flush()
	deadline := time.Now().Add(2 * time.Second)
	for wantQueued && f.p.Pending() == before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !wantQueued {
		time.Sleep(100 * time.Millisecond)
	}
}

func boardPost(kind, text string, ts int64) *msg.MsgBoardPost {
	return &msg.MsgBoardPost{V: 1, PaneID: "pane-worker-1", Persona: "wkr-01", Kind: kind, Text: text, TS: ts, File: "/home/someone/private/notes.md"}
}

func TestPublisherSendsOnlyPublishedBoardsFromNowOn(t *testing.T) {
	f := newPublisher(t)
	ctx := context.Background()
	since := time.Now().UnixMilli()

	// Nothing is published: a post goes nowhere.
	f.publishSetting(nil)
	f.post("default", boardPost(msg.BoardKindMilestone, "unpublished", since+1), false)
	f.p.Flush(ctx)
	if n := len(f.sink.sent()); n != 0 {
		t.Fatalf("an unpublished board sent %d posts", n)
	}

	f.publishSetting(map[string]peer.Publication{"default": {Board: "default", Since: since}})
	for _, s := range f.p.Subjects() {
		if strings.ContainsAny(s, "*>") {
			t.Fatalf("wildcard subscription %q", s)
		}
	}
	if subs := f.p.Subjects(); len(subs) != 1 || subs[0] != msg.BoardPostSubject("default") {
		t.Fatalf("subjects %v, want exactly the published board's post subject", subs)
	}
	f.post("default", boardPost(msg.BoardKindMilestone, "codec wiring done", since+10), true)
	f.post("default", boardPost(msg.BoardKindMilestone, "older than the switch", since-10), false)
	f.post("default", boardPost(msg.BoardKindCmd, "do X now", since+20), false)
	f.post("epic-07", boardPost(msg.BoardKindMilestone, "other board", since+30), false)
	f.p.Flush(ctx)
	sent := f.sink.sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d posts, want 1: %+v", len(sent), sent)
	}
	got := sent[0]
	if !strings.Contains(got.Text, "codec wiring done") || !strings.Contains(got.Text, "default") || !strings.Contains(got.Text, msg.BoardKindMilestone) {
		t.Errorf("published text %q", got.Text)
	}
	if strings.Contains(got.Text, "notes.md") || strings.Contains(got.Text, "/home/") {
		t.Errorf("an attachment path left the session: %q", got.Text)
	}
	if got.AgentName != "wkr-01" || got.AgentID != "pane-worker-1" || got.SessionID != mySession || got.SessionName != "pbw-a" {
		t.Errorf("attribution %+v", got)
	}
	if got.ID != PublishedMessageID(mySession, "default", boardPost(msg.BoardKindMilestone, "codec wiring done", since+10)) || got.ID == "" {
		t.Errorf("message id %q is not the deterministic one", got.ID)
	}
}

func TestPublisherAllIncludesCommandsTextOnly(t *testing.T) {
	f := newPublisher(t)
	since := time.Now().UnixMilli()
	f.publishSetting(map[string]peer.Publication{"epic-07": {Board: "epic-07", Since: since, All: true}})
	f.post("epic-07", boardPost(msg.BoardKindCmd, "rebase onto macmini", since+1), true)
	f.p.Flush(context.Background())
	if sent := f.sink.sent(); len(sent) != 1 || !strings.Contains(sent[0].Text, "rebase onto macmini") {
		t.Fatalf("--all did not send the command post: %+v", sent)
	}
}

func TestPublisherUnpublishStopsAtOnce(t *testing.T) {
	f := newPublisher(t)
	since := time.Now().UnixMilli()
	f.publishSetting(map[string]peer.Publication{"default": {Board: "default", Since: since}})
	// Control: while published, a post is sent (so the checks below cannot
	// pass merely because nothing is ever sent).
	f.post("default", boardPost(msg.BoardKindMilestone, "sent while published", since+1), true)
	f.p.Flush(context.Background())
	if n := len(f.sink.sent()); n != 1 {
		t.Fatalf("control: %d posts sent while published, want 1", n)
	}
	f.post("default", boardPost(msg.BoardKindMilestone, "queued before unpublish", since+2), true)
	// Unpublished before the queue is flushed: the queued post is dropped.
	if err := peer.SavePublications(f.root, "pbw-a", peer.Publications{}); err != nil {
		t.Fatal(err)
	}
	f.p.Flush(context.Background())
	if n := len(f.sink.sent()); n != 1 {
		t.Fatalf("%d posts sent after unpublish", n-1)
	}
	if err := f.p.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if subs := f.p.Subjects(); len(subs) != 0 {
		t.Fatalf("still subscribed after unpublish: %v", subs)
	}
	f.post("default", boardPost(msg.BoardKindMilestone, "after unpublish", since+3), false)
	f.p.Flush(context.Background())
	if n := len(f.sink.sent()); n != 1 {
		t.Fatalf("%d posts sent after unpublish", n-1)
	}
}

func TestPublisherRetriesWithTheSameIDAndIsBounded(t *testing.T) {
	f := newPublisher(t)
	since := time.Now().UnixMilli()
	f.publishSetting(map[string]peer.Publication{"default": {Board: "default", Since: since}})
	f.sink.mu.Lock()
	f.sink.down = true
	f.sink.mu.Unlock()
	f.post("default", boardPost(msg.BoardKindBlocked, "waiting on review", since+1), true)
	f.p.Flush(context.Background())
	if f.p.Pending() != 1 {
		t.Fatalf("a failed send was not kept: pending %d", f.p.Pending())
	}
	f.sink.mu.Lock()
	f.sink.down = false
	f.sink.mu.Unlock()
	f.p.Flush(context.Background())
	sent := f.sink.sent()
	if len(sent) != 1 || sent[0].ID != PublishedMessageID(mySession, "default", boardPost(msg.BoardKindBlocked, "waiting on review", since+1)) {
		t.Fatalf("retry: %+v", sent)
	}
	if f.p.Pending() != 0 {
		t.Fatalf("pending after success: %d", f.p.Pending())
	}
	// Bounded: an outage cannot grow the queue without limit.
	f.sink.mu.Lock()
	f.sink.down = true
	f.sink.mu.Unlock()
	for i := 0; i < 40; i++ {
		f.post("default", boardPost(msg.BoardKindMilestone, "burst", since+int64(10+i)), false)
	}
	if f.p.Pending() > 16 {
		t.Fatalf("pending %d exceeds the bound 16", f.p.Pending())
	}
}

func TestPublishedMessageIDIsStable(t *testing.T) {
	a := boardPost(msg.BoardKindMilestone, "x", 5)
	b := boardPost(msg.BoardKindMilestone, "x", 5)
	if PublishedMessageID(mySession, "default", a) != PublishedMessageID(mySession, "default", b) {
		t.Fatal("the same post got two ids")
	}
	if _, err := uuid.Parse(PublishedMessageID(mySession, "default", a)); err != nil {
		t.Fatalf("id is not a uuid: %v", err)
	}
	for _, other := range []string{PublishedMessageID(otherSession, "default", a), PublishedMessageID(mySession, "epic-07", a), PublishedMessageID(mySession, "default", boardPost(msg.BoardKindMilestone, "y", 5))} {
		if other == PublishedMessageID(mySession, "default", a) {
			t.Fatal("different posts share an id")
		}
	}
}

// Every board view must be able to show that a board is published: the feed
// carries the list.
func TestPollerReportsPublishedBoards(t *testing.T) {
	b := &fakeBoard{t: t, ws: uuid.NewString()}
	b.add(post(0, otherSession, "pbw-b", "hello"))
	p, feeds, _, root := newPoller(t, b)
	if err := peer.SavePublications(root, "pbw-a", peer.Publications{Boards: map[string]peer.Publication{"epic-07": {Board: "epic-07", Since: 1}}}); err != nil {
		t.Fatal(err)
	}
	p.Tick(context.Background())
	if f := mustLast(t, *feeds); len(f.Published) != 1 || f.Published[0] != "epic-07" {
		t.Fatalf("published boards in the feed: %v", f.Published)
	}
}
