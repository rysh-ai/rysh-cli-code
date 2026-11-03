// SPDX-License-Identifier: Apache-2.0

package channels

// gap05 (G13): the Discord adapter against a local fake Discord (REST + the
// Gateway WebSocket), selected with ChannelConfig.APIBase.

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/channels/fakeplatform"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func TestDiscordStartsAgainstFakeGateway(t *testing.T) {
	fake := fakeplatform.NewDiscord("fake-discord-token-gap05")
	defer fake.Close()

	a, err := NewAdapter("discord", msg.ChannelConfig{BotToken: "fake-discord-token-gap05", APIBase: fake.URL(), ReplyMode: "mentions"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := a.Start(ctx); err != nil {
		t.Fatalf("Start against the fake Gateway: %v", err)
	}
	defer func() { _ = a.Stop() }()
	if fake.Identifies() != 1 {
		t.Fatalf("IDENTIFY packets = %d, want 1", fake.Identifies())
	}
	if !a.Status().Connected {
		t.Fatalf("status = %+v, want connected", a.Status())
	}

	const user = "100000000000000042"
	fake.PushDM(user, "alice", "hello")
	var im InboundMessage
	select {
	case im = <-a.InboundCh():
	case <-time.After(5 * time.Second):
		t.Fatal("no inbound from the fake Gateway")
	}
	if im.SenderID != user || im.ThreadID != fake.DMChannel(user) || im.Metadata["direct_message"] != "true" {
		t.Fatalf("inbound = %+v", im)
	}

	// The humanoid replies with RecipientID = the author and ThreadID = the
	// channel (routeOutboundToChannel). Discord posts to channels, never to
	// user ids, so the message must go to the channel.
	if err := a.Send(ctx, OutboundMessage{RecipientID: im.SenderID, ThreadID: im.ThreadID, Content: "hi back"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	s := fake.Sends()
	if len(s) != 1 || s[0].ChannelID != fake.DMChannel(user) || s[0].Content != "hi back" {
		t.Fatalf("fake sends = %+v, want one post to the DM channel %s", s, fake.DMChannel(user))
	}
}

func TestDiscordRefusesCleartextRemoteAPIBase(t *testing.T) {
	a, err := NewAdapter("discord", msg.ChannelConfig{BotToken: "fake-discord-token-gap05", APIBase: "http://discord-proxy.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	err = a.Start(context.Background())
	if err == nil {
		_ = a.Stop()
		t.Fatal("Start accepted a cleartext non-loopback api_base")
	}
	if !strings.Contains(err.Error(), "api_base") || strings.Contains(err.Error(), "fake-discord-token-gap05") {
		t.Fatalf("refusal = %v", err)
	}
}

// stubRoundTripper answers every request in-process; no socket is opened.
type stubRoundTripper struct {
	body string
	seen []string
}

func (s *stubRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	s.seen = append(s.seen, r.URL.String())
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(s.body)),
		Request:    r,
	}, nil
}

// The Gateway URL comes from the api_base server's answer, and IDENTIFY
// carries the token, so a cleartext non-loopback Gateway URL is refused.
func TestDiscordAPIBaseTransportRewritesAndGuardsGateway(t *testing.T) {
	stub := &stubRoundTripper{body: `{"url":"ws://gateway.example.com"}`}
	rt, err := newDiscordAPIBaseTransport("http://127.0.0.1:9", stub)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, "https://discord.com/api/v10/gateway", nil)
	if _, err := rt.RoundTrip(req); err == nil || !strings.Contains(err.Error(), "gateway") {
		t.Fatalf("a cleartext remote gateway URL was accepted (err=%v)", err)
	}
	if len(stub.seen) != 1 || stub.seen[0] != "http://127.0.0.1:9/api/v10/gateway" {
		t.Fatalf("request not rewritten to the api_base: %v", stub.seen)
	}

	stub.body = `{"url":"wss://gateway.discord.gg"}`
	req, _ = http.NewRequest(http.MethodGet, "https://discord.com/api/v10/gateway", nil)
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatalf("a wss gateway URL was refused: %v", err)
	}
	stub.body = `{"url":"ws://127.0.0.1:9/gateway"}`
	req, _ = http.NewRequest(http.MethodGet, "https://discord.com/api/v10/gateway", nil)
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatalf("a loopback ws gateway URL was refused: %v", err)
	}

	// A request to any host other than discord.com is never rewritten.
	req, _ = http.NewRequest(http.MethodGet, "https://cdn.discordapp.com/x", nil)
	if _, err := rt.RoundTrip(req); err == nil {
		t.Fatal("a non-discord.com request went out through the api_base transport")
	}
}
