// SPDX-License-Identifier: Apache-2.0

package peerfeed

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	pc "github.com/rysh-ai/rysh-cli-shared/peercoord"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/peer"
)

const (
	mySession    = "11111111-1111-4111-8111-111111111111"
	otherSession = "22222222-2222-4222-8222-222222222222"
)

var me = Identity{SessionID: mySession, SessionName: "pbw-a", Agents: map[string]bool{"agent-mine": true}}

func post(seq int64, session, name, text string) peer.Message {
	id := uuid.NewString()
	return peer.Message{Sequence: seq, ID: id, ThreadID: id, SessionID: session, SessionName: name, AgentName: "agent", Text: text, CreatedAt: time.Unix(1700000000+seq, 0)}
}

func reply(seq int64, root peer.Message, session, name, text string) peer.Message {
	m := post(seq, session, name, text)
	m.ThreadID = root.ID
	return m
}

func TestClassifyReasons(t *testing.T) {
	root := post(1, mySession, "pbw-a", "my root")
	threads := map[string]bool{root.ID: true}
	addressedMsg := post(5, otherSession, "pbw-b", "[message] pbw-b/worker: which section?")
	addressed := map[string]bool{addressedMsg.ID: true}
	for _, c := range []struct {
		name string
		m    peer.Message
		want string
	}{
		{"own post never alerts", post(2, mySession, "pbw-a", "@pbw-a note to self"), ""},
		{"own reply never alerts", reply(3, root, mySession, "pbw-a", "more"), ""},
		{"addressed beats everything", addressedMsg, msg.PeerAlertAddressed},
		{"reply in my thread", reply(6, root, otherSession, "pbw-b", "thanks"), msg.PeerAlertReply},
		{"mention", post(7, otherSession, "pbw-b", "ping @PBW-A, please look"), msg.PeerAlertMention},
		{"mention is a whole token", post(8, otherSession, "pbw-b", "@pbw-ab is someone else"), msg.PeerAlertMessage},
		{"other message", post(9, otherSession, "pbw-b", "build is green"), msg.PeerAlertMessage},
	} {
		if got := Classify(me, c.m, threads, addressed); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestFeedHistoryNeverAlertsThenNewMessagesDo(t *testing.T) {
	f := NewFeed(10)
	if f.Cursor() != nil {
		t.Fatal("a new feed has a cursor")
	}
	root := post(1, mySession, "pbw-a", "my root")
	history := []peer.Message{root, post(2, otherSession, "pbw-b", "old news @pbw-a")}
	if alerts := f.Apply(me, history, nil); len(alerts) != 0 {
		t.Fatalf("history raised alerts: %+v", alerts)
	}
	if c := f.Cursor(); c == nil || *c != 2 {
		t.Fatalf("cursor after history: %v", c)
	}
	if got := f.Messages(); len(got) != 2 || !got[0].Mine || got[1].Mine {
		t.Fatalf("history messages: %+v", got)
	}
	fresh := []peer.Message{reply(3, root, otherSession, "pbw-b", "re"), post(4, mySession, "pbw-a", "mine"), post(5, otherSession, "pbw-b", "news")}
	alerts := f.Apply(me, fresh, nil)
	if len(alerts) != 2 || alerts[0].Alert != msg.PeerAlertReply || alerts[1].Alert != msg.PeerAlertMessage {
		t.Fatalf("alerts: %+v", alerts)
	}
	// A page that overlaps what the feed holds is not counted twice.
	if again := f.Apply(me, fresh, nil); len(again) != 0 || len(f.Messages()) != 5 {
		t.Fatalf("duplicate page: alerts %d, messages %d", len(again), len(f.Messages()))
	}
}

func TestFeedIsBoundedAndResets(t *testing.T) {
	f := NewFeed(3)
	var page []peer.Message
	for i := int64(1); i <= 5; i++ {
		page = append(page, post(i, otherSession, "pbw-b", fmt.Sprint("m", i)))
	}
	f.Apply(me, page, nil)
	got := f.Messages()
	if len(got) != 3 || got[0].Sequence != 3 || got[2].Sequence != 5 {
		t.Fatalf("bounded feed: %+v", got)
	}
	f.Reset()
	if f.Cursor() != nil || len(f.Messages()) != 0 {
		t.Fatal("reset kept state")
	}
	if alerts := f.Apply(me, page[:1], nil); len(alerts) != 0 {
		t.Fatal("first page after a reset alerted")
	}
}

// fakeBoard is a peer server: messages, coordination events and state.
type fakeBoard struct {
	mu       sync.Mutex
	t        *testing.T
	ws       string
	messages []peer.Message
	events   []pc.Event
	state    pc.State
	down     bool
	gets     int
	posts    int
}

func (b *fakeBoard) add(m peer.Message) {
	b.mu.Lock()
	defer b.mu.Unlock()
	m.Sequence = int64(len(b.messages) + 1)
	b.messages = append(b.messages, m)
}

func (b *fakeBoard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer rysh_feed_test_key" {
		b.t.Errorf("request without the key: %s", r.URL.Path)
	}
	if r.Method != http.MethodGet {
		b.posts++
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if b.down {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	base := "/api/workspaces/" + b.ws + "/peer-boards/team"
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case base + "/messages":
		b.gets++
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		page := peer.Page{Messages: []peer.Message{}}
		if a := r.URL.Query().Get("after"); a != "" {
			after, _ := strconv.ParseInt(a, 10, 64)
			page.Next = after
			for _, m := range b.messages {
				if m.Sequence > after && len(page.Messages) < limit {
					page.Messages = append(page.Messages, m)
				}
			}
			page.HasMore = len(b.messages) > 0 && len(page.Messages) > 0 && page.Messages[len(page.Messages)-1].Sequence < b.messages[len(b.messages)-1].Sequence
		} else {
			start := max(0, len(b.messages)-limit)
			page.Messages = append(page.Messages, b.messages[start:]...)
		}
		if n := len(page.Messages); n > 0 {
			page.Next = page.Messages[n-1].Sequence
		}
		json.NewEncoder(w).Encode(page)
	case base + "/coordination":
		json.NewEncoder(w).Encode(b.state)
	case base + "/coordination/events":
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		page := pc.Page{Events: []pc.Event{}, Next: after}
		for _, e := range b.events {
			if e.Sequence > after {
				page.Events = append(page.Events, e)
				page.Next = e.Sequence
			}
		}
		json.NewEncoder(w).Encode(page)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func newPoller(t *testing.T, b *fakeBoard) (*Poller, *[]*msg.MsgPeerFeed, *[]msg.PeerFeedMessage, string) {
	t.Helper()
	server := httptest.NewServer(b)
	t.Cleanup(server.Close)
	root := t.TempDir()
	if err := peer.Save(root, "pbw-a", peer.Connection{Enabled: true, URL: server.URL, Workspace: b.ws, Board: "team", KeySecret: "FEED_KEY", SessionID: mySession, Pane: "pane-connect"}); err != nil {
		t.Fatal(err)
	}
	var feeds []*msg.MsgPeerFeed
	var alerts []msg.PeerFeedMessage
	p := &Poller{
		Root: root, Session: "pbw-a",
		Key: func(name string) string {
			if name != "FEED_KEY" {
				t.Errorf("key looked up by %q", name)
			}
			return "rysh_feed_test_key"
		},
		Publish: func(f *msg.MsgPeerFeed) { feeds = append(feeds, f) },
		Alert: func(c peer.Connection, m msg.PeerFeedMessage) {
			if c.Pane != "pane-connect" {
				t.Errorf("alert connection pane %q", c.Pane)
			}
			alerts = append(alerts, m)
		},
		Max: 50,
	}
	return p, &feeds, &alerts, root
}

func last(feeds []*msg.MsgPeerFeed) *msg.MsgPeerFeed {
	if len(feeds) == 0 {
		return nil
	}
	return feeds[len(feeds)-1]
}

// mustLast is last for assertions that read fields: no feed is a failure, not a panic.
func mustLast(t *testing.T, feeds []*msg.MsgPeerFeed) *msg.MsgPeerFeed {
	t.Helper()
	f := last(feeds)
	if f == nil {
		t.Fatal("nothing was published")
	}
	return f
}

func TestPollerFollowsBoardAlertsAndRecovers(t *testing.T) {
	b := &fakeBoard{t: t, ws: uuid.NewString()}
	root := post(0, mySession, "pbw-a", "my root")
	b.add(root)
	b.add(post(0, otherSession, "pbw-b", "history"))
	p, feeds, alerts, _ := newPoller(t, b)
	ctx := context.Background()

	p.Tick(ctx)
	f := last(*feeds)
	if f == nil || !f.Connected || f.Board != "team" || len(f.Messages) != 2 || f.Error != "" {
		t.Fatalf("first feed: %+v", f)
	}
	if len(*alerts) != 0 {
		t.Fatalf("history alerted: %+v", *alerts)
	}

	b.add(reply(0, root, otherSession, "pbw-b", "re: your root"))
	b.add(post(0, otherSession, "pbw-b", "plain news"))
	p.Tick(ctx)
	if f = mustLast(t, *feeds); len(f.Messages) != 4 {
		t.Fatalf("follow: %+v", f)
	}
	if len(*alerts) != 2 || (*alerts)[0].Alert != msg.PeerAlertReply || (*alerts)[1].Alert != msg.PeerAlertMessage {
		t.Fatalf("alerts: %+v", *alerts)
	}

	// A server outage keeps what the feed has and says why; nothing repeats
	// once it is back.
	b.mu.Lock()
	b.down = true
	b.mu.Unlock()
	p.Tick(ctx)
	if f = last(*feeds); f.Error == "" || len(f.Messages) != 4 || !f.Connected {
		t.Fatalf("outage feed: %+v", f)
	}
	b.mu.Lock()
	b.down = false
	b.mu.Unlock()
	b.add(post(0, otherSession, "pbw-b", "after the outage"))
	p.Tick(ctx)
	if f = last(*feeds); f.Error != "" || len(f.Messages) != 5 {
		t.Fatalf("recovered feed: %+v", f)
	}
	if len(*alerts) != 3 {
		t.Fatalf("alerts after recovery: %d", len(*alerts))
	}
	seen := map[int64]bool{}
	for _, m := range f.Messages {
		if seen[m.Sequence] {
			t.Fatalf("duplicate sequence %d", m.Sequence)
		}
		seen[m.Sequence] = true
	}
	if b.posts != 0 {
		t.Fatalf("the feed wrote to the peer board %d times", b.posts)
	}
}

func TestPollerAddressedCoordinationMessages(t *testing.T) {
	b := &fakeBoard{t: t, ws: uuid.NewString()}
	b.add(post(0, otherSession, "pbw-b", "history"))
	b.state = pc.State{Peers: map[string]*pc.Peer{
		"peer-a": {ID: "peer-a", SessionID: mySession},
		"peer-b": {ID: "peer-b", SessionID: otherSession},
	}, Agents: map[string]*pc.Agent{
		"agent-mine":  {ID: "agent-mine", PeerID: "peer-a", Label: "pbw-a/leader"},
		"agent-other": {ID: "agent-other", PeerID: "peer-b", Label: "pbw-b/worker"},
	}}
	p, _, alerts, _ := newPoller(t, b)
	ctx := context.Background()
	p.Tick(ctx)

	toMe := post(0, otherSession, "pbw-b", "[message] pbw-b/worker: which README section?")
	toOther := post(0, otherSession, "pbw-b", "[message] pbw-b/worker: note to the other worker")
	b.add(toMe)
	b.add(toOther)
	b.mu.Lock()
	b.events = []pc.Event{
		{ID: toMe.ID, Sequence: 2, Kind: "message", SenderID: "agent-other", RecipientID: "agent-mine", Text: toMe.Text},
		{ID: toOther.ID, Sequence: 3, Kind: "message", SenderID: "agent-other", RecipientID: "agent-other", Text: toOther.Text},
	}
	b.mu.Unlock()
	p.Tick(ctx)
	if len(*alerts) != 2 || (*alerts)[0].Alert != msg.PeerAlertAddressed || (*alerts)[1].Alert != msg.PeerAlertMessage {
		t.Fatalf("alerts: %+v", *alerts)
	}
}

func TestPollerDisconnectedAndBoardChange(t *testing.T) {
	b := &fakeBoard{t: t, ws: uuid.NewString()}
	b.add(post(0, otherSession, "pbw-b", "history"))
	p, feeds, alerts, root := newPoller(t, b)
	ctx := context.Background()
	p.Tick(ctx)

	c, _ := peer.Load(root, "pbw-a")
	c.Enabled = false
	peer.Save(root, "pbw-a", c)
	gets := b.gets
	p.Tick(ctx)
	if f := mustLast(t, *feeds); f.Connected || len(f.Messages) != 0 {
		t.Fatalf("disconnected feed: %+v", f)
	}
	if b.gets != gets {
		t.Fatal("a disconnected feed read the board")
	}

	// Reconnecting to another board starts over: its history does not alert.
	b.add(post(0, otherSession, "pbw-b", "while away"))
	c.Enabled, c.Board = true, "team"
	c.SessionID = uuid.NewString()
	peer.Save(root, "pbw-a", c)
	p.Tick(ctx)
	if f := mustLast(t, *feeds); !f.Connected || len(f.Messages) != 2 {
		t.Fatalf("reconnected feed: %+v", f)
	}
	if len(*alerts) != 0 {
		t.Fatalf("reconnect history alerted: %+v", *alerts)
	}
}

func TestPollerNeverPublishesTheKey(t *testing.T) {
	b := &fakeBoard{t: t, ws: uuid.NewString()}
	b.add(post(0, otherSession, "pbw-b", "hello"))
	p, feeds, _, _ := newPoller(t, b)
	p.Tick(context.Background())
	data, _ := json.Marshal(last(*feeds))
	if len(*feeds) == 0 || strings.Contains(string(data), "rysh_feed_test_key") || strings.Contains(string(data), "FEED_KEY") {
		t.Fatal("feed carries the key or its name (or nothing was published)")
	}
}
