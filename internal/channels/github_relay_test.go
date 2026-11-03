// SPDX-License-Identifier: Apache-2.0

package channels

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// E-108 relay mode (design 073 "Later": the rysh-server relay). rysh-server
// holds the webhook secret and the App key, verifies every delivery and posts
// replies; the session subscribes to ws.{W}.github.{C}.inbound and asks the
// server to post on …outbound. Every design-073 guard stays in the cli and
// applies to a relayed event exactly as to a webhook. No network: an embedded
// nats-server, and a REST client that fails closed and counts.

// githubFixtureDir is the package directory, captured before any test
// changes into a temp directory for its channel state.
var githubFixtureDir, _ = os.Getwd()

// rfixture is fixture() with an absolute path, for tests that t.Chdir.
func rfixture(t *testing.T, name string, edits map[string]any) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(githubFixtureDir, "testdata", "github", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for path, v := range edits {
		parts := strings.Split(path, ".")
		node := doc
		for _, p := range parts[:len(parts)-1] {
			node = node[p].(map[string]any)
		}
		node[parts[len(parts)-1]] = v
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func githubRelayCfg() msg.ChannelConfig {
	return msg.ChannelConfig{
		Enabled:           true,
		Relay:             true,
		RelayURL:          "https://dev.rysh.ai",
		RelayAPIKey:       "relay-key",
		RelayWorkspaceID:  "ws-1",
		RelayConnectionID: "conn-1",
		BotLogin:          "example_bot",
		Channels:          []string{"acme/widgets"},
		PairingPolicy:     "drop",
	}
}

// noRESTTransport fails every request closed and counts it: relay mode must
// never call the GitHub REST API itself.
type noRESTTransport struct{ n atomic.Int32 }

func (c *noRESTTransport) RoundTrip(*http.Request) (*http.Response, error) {
	c.n.Add(1)
	return nil, errors.New("relay mode must not call GitHub directly")
}

func newGitHubRelayAdapter(t *testing.T) (*GitHubAdapter, *noRESTTransport) {
	t.Helper()
	t.Chdir(t.TempDir())
	g := NewGitHubAdapter(githubRelayCfg())
	ct := &noRESTTransport{}
	g.client = &http.Client{Transport: ct}
	return g, ct
}

// githubRelayMsg is the server's channel.InboundChannelMessage for one
// verified webhook delivery.
func githubRelayMsg(t *testing.T, event string, body []byte, delivery string, ts int64) []byte {
	t.Helper()
	var ev struct {
		Action     string `json:"action"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Sender struct {
			Login string `json:"login"`
		} `json:"sender"`
	}
	_ = json.Unmarshal(body, &ev)
	out, err := json.Marshal(relayInbound{
		ChannelType:  "github",
		ConnectionID: "conn-1",
		WorkspaceID:  "ws-1",
		SenderID:     ev.Sender.Login,
		SenderName:   ev.Sender.Login,
		MessageID:    delivery,
		Timestamp:    ts,
		Metadata: map[string]string{
			"github_event":           event,
			"github_action":          ev.Action,
			"github_repo":            ev.Repository.FullName,
			"github_installation_id": "77",
			"github_payload":         string(body),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func githubInbound(g *GitHubAdapter) (InboundMessage, bool) {
	select {
	case m := <-g.inbound:
		return m, true
	case <-time.After(300 * time.Millisecond):
		return InboundMessage{}, false
	}
}

func TestGitHubRelayInboundMentionBecomesInbound(t *testing.T) {
	g, ct := newGitHubRelayAdapter(t)
	g.handleRelayInbound(githubRelayMsg(t, "issue_comment", rfixture(t, "issue_comment_created", nil), "d-1", time.Now().Unix()), false)
	m, ok := githubInbound(g)
	if !ok {
		t.Fatal("a relayed owner mention produced no inbound message")
	}
	if m.SenderID != "alice" || m.ThreadID != "acme/widgets#42" {
		t.Errorf("sender/thread = %q/%q", m.SenderID, m.ThreadID)
	}
	if m.Content != "please find why the parser drops the last token" {
		t.Errorf("Content = %q, want the comment without the mention", m.Content)
	}
	md := m.Metadata
	if md["repo"] != "acme/widgets" || md["number"] != "42" || md["kind"] != "issue" || md["comment_id"] != "9001" || md["delivery"] != "d-1" {
		t.Errorf("metadata = %+v", md)
	}
	if _, ok := md["title"]; ok {
		t.Errorf("metadata carries the issue title: %+v", md)
	}
	if _, ok := md["github_payload"]; ok {
		t.Errorf("the raw payload must not ride toward the prompt: %+v", md)
	}
	if ct.n.Load() != 0 {
		t.Errorf("relay mode called the GitHub REST API %d times", ct.n.Load())
	}
	if _, again := githubInbound(g); again {
		t.Errorf("one delivery produced two inbound messages")
	}
}

func TestGitHubRelayIgnoresWhatItMustIgnore(t *testing.T) {
	now := time.Now().Unix()
	cases := []struct {
		name    string
		event   string
		fixture string
		edits   map[string]any
		mutate  func(map[string]any)
	}{
		{"no mention", "issue_comment", "issue_comment_created", map[string]any{"comment.body": "looks fine"}, nil},
		{"longer handle", "issue_comment", "issue_comment_created", map[string]any{"comment.body": "@example_bot-ci fix it"}, nil},
		{"edited comment", "issue_comment", "issue_comment_created", map[string]any{"action": "edited"}, nil},
		{"unlisted repo", "issue_comment", "issue_comment_created", map[string]any{"repository.full_name": "evil/fork"}, nil},
		{"non-member", "issue_comment", "issue_comment_created", map[string]any{"comment.author_association": "CONTRIBUTOR"}, nil},
		{"own login", "issue_comment", "issue_comment_created", map[string]any{"comment.user": map[string]any{"login": "example_bot", "type": "User"}, "sender": map[string]any{"login": "example_bot", "type": "User"}}, nil},
		{"own app bot", "issue_comment", "issue_comment_created", map[string]any{"comment.user": map[string]any{"login": "example_bot[bot]", "type": "Bot"}, "sender": map[string]any{"login": "example_bot[bot]", "type": "Bot"}}, nil},
		{"any bot", "issue_comment", "issue_comment_created", map[string]any{"sender": map[string]any{"login": "dependabot[bot]", "type": "Bot"}}, nil},
		{"issue edited", "issues", "issues_opened", map[string]any{"action": "edited"}, nil},
		{"unhandled event", "push", "issue_comment_created", nil, nil},
		{"repo metadata disagrees with payload", "issue_comment", "issue_comment_created", nil,
			func(m map[string]any) { m["metadata"].(map[string]any)["github_repo"] = "evil/other" }},
		{"another channel's message", "issue_comment", "issue_comment_created", nil,
			func(m map[string]any) { m["channel_type"] = "whatsapp" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, ct := newGitHubRelayAdapter(t)
			data := githubRelayMsg(t, tc.event, rfixture(t, tc.fixture, tc.edits), "d-"+tc.name, now)
			if tc.mutate != nil {
				var m map[string]any
				_ = json.Unmarshal(data, &m)
				tc.mutate(m)
				data, _ = json.Marshal(m)
			}
			g.handleRelayInbound(data, false)
			if m, ok := githubInbound(g); ok {
				t.Fatalf("%s produced an inbound message: %+v", tc.name, m)
			}
			if ct.n.Load() != 0 {
				t.Errorf("an ignored relay message called GitHub %d times", ct.n.Load())
			}
		})
	}
}

// A delivery repeated, or the same comment under a new delivery id, or a
// replayed copy, is answered once — and that survives a restart in the same
// directory, because replay re-sends from a one-second watermark.
func TestGitHubRelayAnswersEachRequestOnce(t *testing.T) {
	g, _ := newGitHubRelayAdapter(t)
	body := rfixture(t, "issue_comment_created", nil)
	now := time.Now().Unix()
	g.handleRelayInbound(githubRelayMsg(t, "issue_comment", body, "d-1", now), false)
	if _, ok := githubInbound(g); !ok {
		t.Fatal("first delivery produced nothing")
	}
	g.handleRelayInbound(githubRelayMsg(t, "issue_comment", body, "d-1", now), false)
	g.handleRelayInbound(githubRelayMsg(t, "issue_comment", body, "d-2", now), false) // same comment, new delivery
	g.handleRelayInbound(githubRelayMsg(t, "issue_comment", body, "d-1", now), true)  // replayed copy
	if m, ok := githubInbound(g); ok {
		t.Fatalf("a repeat was answered again: %+v", m)
	}
	restarted := NewGitHubAdapter(githubRelayCfg())
	restarted.handleRelayInbound(githubRelayMsg(t, "issue_comment", body, "d-1", now), true)
	if m, ok := githubInbound(restarted); ok {
		t.Fatalf("a restarted session answered an already-handled delivery: %+v", m)
	}
}

// A replayed mention old enough that the thread has moved on is not
// answered; a recent one is, marked as replayed.
func TestGitHubRelayReplayAge(t *testing.T) {
	g, _ := newGitHubRelayAdapter(t)
	old := time.Now().Add(-2 * relayReplyWindow).Unix()
	g.handleRelayInbound(githubRelayMsg(t, "issue_comment", rfixture(t, "issue_comment_created", nil), "d-old", old), true)
	if m, ok := githubInbound(g); ok {
		t.Fatalf("a stale replayed mention was answered: %+v", m)
	}
	recent := rfixture(t, "issue_comment_created", map[string]any{"comment.id": 9002})
	g.handleRelayInbound(githubRelayMsg(t, "issue_comment", recent, "d-new", time.Now().Unix()), true)
	m, ok := githubInbound(g)
	if !ok || m.Metadata["replayed"] != "true" {
		t.Fatalf("a recent replayed mention: ok=%v metadata=%+v", ok, m.Metadata)
	}
}

func TestGitHubRelayModeConfig(t *testing.T) {
	if err := NewGitHubAdapter(githubRelayCfg()).checkConfig(); err != nil {
		t.Fatalf("a complete relay config is refused: %v", err)
	}
	cases := []struct {
		name, want string
		mutate     func(*msg.ChannelConfig)
	}{
		{"app_secret set", "app_secret", func(c *msg.ChannelConfig) { c.AppSecret = "x" }},
		{"bot_token set", "bot_token", func(c *msg.ChannelConfig) { c.BotToken = "x" }},
		{"no workspace id", "workspace", func(c *msg.ChannelConfig) { c.RelayWorkspaceID = "" }},
		{"no connection id", "connection", func(c *msg.ChannelConfig) { c.RelayConnectionID = "" }},
		{"no upstream url", "upstream", func(c *msg.ChannelConfig) { c.RelayURL = "" }},
		{"no upstream key", "upstream", func(c *msg.ChannelConfig) { c.RelayAPIKey = "" }},
		{"no bot_login", "bot_login", func(c *msg.ChannelConfig) { c.BotLogin = "" }},
		{"no repos", "channels", func(c *msg.ChannelConfig) { c.Channels = nil }},
		{"pairing on a public thread", "pairing_policy", func(c *msg.ChannelConfig) { c.PairingPolicy = "" }},
	}
	for _, tc := range cases {
		cfg := githubRelayCfg()
		tc.mutate(&cfg)
		err := NewGitHubAdapter(cfg).checkConfig()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want a refusal naming %q", tc.name, err, tc.want)
		}
	}
}

// Start in relay mode dials the upstream and binds no webhook listener.
func TestGitHubRelayStartBindsNoListener(t *testing.T) {
	t.Chdir(t.TempDir())
	ns, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats not ready")
	}
	t.Cleanup(ns.Shutdown)
	shrinkRelayTimeout(t)
	cfg := githubRelayCfg()
	cfg.RelayURL = ns.ClientURL()
	cfg.WebhookPort = freeGitHubPort(t)
	g := NewGitHubAdapter(cfg)
	ct := &noRESTTransport{}
	g.client = &http.Client{Transport: ct}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := g.Start(ctx); err != nil {
		t.Fatalf("relay Start: %v", err)
	}
	defer g.Stop()
	if addr := g.listenAddr(); addr != "" {
		t.Errorf("relay mode bound a webhook listener on %s", addr)
	}
	if !g.Status().Connected {
		t.Errorf("relay Start left the channel disconnected: %+v", g.Status())
	}
	if ct.n.Load() != 0 {
		t.Errorf("relay Start called GitHub %d times (no token in relay mode)", ct.n.Load())
	}
}

// relayRecorder answers every request on subject with reply and records it.
type relayRecorder struct {
	mu   sync.Mutex
	reqs []map[string]any
}

func recordRelay(t *testing.T, nc *nats.Conn, subject, reply string) *relayRecorder {
	t.Helper()
	r := &relayRecorder{}
	sub, err := nc.Subscribe(subject, func(m *nats.Msg) {
		var body map[string]any
		_ = json.Unmarshal(m.Data, &body)
		r.mu.Lock()
		r.reqs = append(r.reqs, body)
		r.mu.Unlock()
		_ = m.Respond([]byte(reply))
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *relayRecorder) all() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]map[string]any(nil), r.reqs...)
}

func TestGitHubRelaySendAsksTheServerToComment(t *testing.T) {
	nc := newOutboundTestConn(t)
	g, ct := newGitHubRelayAdapter(t)
	g.relayConn = nc
	rec := recordRelay(t, nc, "ws.ws-1.github.conn-1.outbound", `{"status":"accepted"}`)

	long := strings.Repeat("a", githubMaxBodyLen+10)
	if err := g.Send(context.Background(), OutboundMessage{ThreadID: "acme/widgets#42", Content: long}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	reqs := rec.all()
	if len(reqs) != 2 {
		t.Fatalf("%d outbound requests, want 2 (split over %d chars)", len(reqs), githubMaxBodyLen)
	}
	ids := map[string]bool{}
	for i, r := range reqs {
		if r["recipient_id"] != "acme/widgets#42" || r["connection_id"] != "conn-1" || r["workspace_id"] != "ws-1" {
			t.Errorf("request %d = %+v", i, r)
		}
		id, _ := r["message_id"].(string)
		if id == "" || ids[id] {
			t.Errorf("request %d message_id %q: want a distinct idempotency key", i, id)
		}
		ids[id] = true
		if c, _ := r["content"].(string); len(c) > githubMaxBodyLen {
			t.Errorf("request %d content is %d chars, over %d", i, len(c), githubMaxBodyLen)
		}
	}
	if ct.n.Load() != 0 {
		t.Errorf("relay Send called GitHub directly %d times", ct.n.Load())
	}
	if err := g.Send(context.Background(), OutboundMessage{ThreadID: "evil/other#1", Content: "x"}); err == nil {
		t.Errorf("Send to an unlisted repository was not refused")
	}
}

func TestGitHubRelaySendReturnsTheServerError(t *testing.T) {
	nc := newOutboundTestConn(t)
	g, _ := newGitHubRelayAdapter(t)
	g.relayConn = nc
	recordRelay(t, nc, "ws.ws-1.github.conn-1.outbound", `{"error":"repository not linked to this connection"}`)
	err := g.Send(context.Background(), OutboundMessage{ThreadID: "acme/widgets#42", Content: "hi"})
	if err == nil || !strings.Contains(err.Error(), "repository not linked") {
		t.Fatalf("Send err = %v, want the server's refusal", err)
	}
}

// On pickup the session asks the server for the 👀 reaction on the comment.
func TestGitHubRelayPickupAsksForTheEyesReaction(t *testing.T) {
	for _, tc := range []struct{ event, fixture, thread string }{
		{"issue_comment", "issue_comment_created", "issue_comment:9001"},
		{"pull_request_review_comment", "pull_request_review_comment_created", "pull_request_review_comment:"},
	} {
		t.Run(tc.event, func(t *testing.T) {
			nc := newOutboundTestConn(t)
			g, ct := newGitHubRelayAdapter(t)
			g.relayConn = nc
			rec := recordRelay(t, nc, "ws.ws-1.github.conn-1.outbound", `{"status":"accepted"}`)
			g.handleRelayInbound(githubRelayMsg(t, tc.event, rfixture(t, tc.fixture, nil), "d-1", time.Now().Unix()), false)
			if _, ok := githubInbound(g); !ok {
				t.Fatal("no inbound")
			}
			if !waitFor(t, 2*time.Second, func() bool { return len(rec.all()) == 1 }) {
				t.Fatalf("%d reaction requests, want 1", len(rec.all()))
			}
			r := rec.all()[0]
			th, _ := r["thread_id"].(string)
			if r["type"] != "reaction" || r["recipient_id"] != "acme/widgets" || r["content"] != "eyes" || !strings.HasPrefix(th, tc.thread) {
				t.Errorf("reaction request = %+v", r)
			}
			if ct.n.Load() != 0 {
				t.Errorf("the reaction went to GitHub directly")
			}
		})
	}
}

// After (re)connecting, the session asks for what arrived since its
// watermark and handles it as replayed.
func TestGitHubRelayReplayAsksSinceTheWatermark(t *testing.T) {
	t.Chdir(t.TempDir())
	shrinkRelayTimeout(t)
	ns := startPermServer(t, -1, nil, nil)
	t.Cleanup(ns.Shutdown)
	admin, err := nats.Connect(ns.ClientURL(), nats.UserInfo("admin", "pw"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)

	since := time.Now().Add(-time.Minute).Unix()
	missed := githubRelayMsg(t, "issue_comment", rfixture(t, "issue_comment_created", nil), "d-missed", time.Now().Unix())
	var asked atomic.Int64
	asked.Store(-1)
	if _, err := admin.Subscribe("ws.ws-1.github.conn-1.replay", func(m *nats.Msg) {
		var req relayReplayRequest
		_ = json.Unmarshal(m.Data, &req)
		asked.Store(req.SinceTimestamp)
		resp, _ := json.Marshal(relayReplayResponse{Messages: []json.RawMessage{missed}})
		_ = m.Respond(resp)
	}); err != nil {
		t.Fatal(err)
	}
	_ = admin.Flush()

	g := NewGitHubAdapter(githubRelayCfg())
	g.client = &http.Client{Transport: &noRESTTransport{}}
	// Stop waits for the relay's background replay and reaction; registered
	// before nc.Close, so it runs after the connection is closed and before
	// shrinkRelayTimeout restores the timeout they read.
	t.Cleanup(func() { _ = g.Stop() })
	g.noteRelaySeen(since)
	nc, err := nats.Connect(ns.ClientURL(), append([]nats.Option{nats.UserInfo("relay", "pw")}, g.relayEventHandlers()...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	if err := g.attachRelay(nc); err != nil {
		t.Fatal(err)
	}
	m, ok := func() (InboundMessage, bool) {
		select {
		case m := <-g.inbound:
			return m, true
		case <-time.After(3 * time.Second):
			return InboundMessage{}, false
		}
	}()
	if got := asked.Load(); got != since {
		t.Errorf("replay asked since %d, want the watermark %d", got, since)
	}
	if !ok || m.Metadata["replayed"] != "true" || m.Metadata["delivery"] != "d-missed" {
		t.Fatalf("missed mention not replayed: ok=%v %+v", ok, m.Metadata)
	}
}

// Another session holding the connection (D-21) gives the plain-words holder
// status, as WhatsApp's relay does, and nothing is answered.
func TestGitHubRelayRefusedInboundSetsHolderStatus(t *testing.T) {
	t.Chdir(t.TempDir())
	shrinkRelayTimeout(t)
	ns := startPermServer(t, -1, []string{"ws.ws-1.github.conn-1.inbound"}, nil)
	t.Cleanup(ns.Shutdown)
	g := NewGitHubAdapter(githubRelayCfg())
	g.client = &http.Client{Transport: &noRESTTransport{}}
	t.Cleanup(func() { _ = g.Stop() }) // waits for background relay work (see above)
	nc, err := nats.Connect(ns.ClientURL(), append([]nats.Option{nats.UserInfo("relay", "pw"), nats.MaxReconnects(-1), nats.ReconnectWait(50 * time.Millisecond)}, g.relayEventHandlers()...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	if err := g.attachRelay(nc); err != nil {
		t.Fatal(err)
	}
	if !waitFor(t, 3*time.Second, func() bool { return g.Status().Error == githubRelayHolderInUseError }) {
		t.Fatalf("status error = %q, want %q", g.Status().Error, githubRelayHolderInUseError)
	}
	g.handleRelayInbound(githubRelayMsg(t, "issue_comment", rfixture(t, "issue_comment_created", nil), "d-x", time.Now().Unix()), false)
	if m, ok := githubInbound(g); ok {
		t.Errorf("a session that does not hold the connection answered: %+v", m)
	}
}
