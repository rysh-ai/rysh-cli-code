// SPDX-License-Identifier: Apache-2.0

package channels

// gap05 (G13 c1): the Mattermost adapter against a local fake Mattermost
// (REST v4 + the websocket event stream).

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/channels/fakeplatform"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

const mmToken = "fake-mattermost-token-gap05"

func startMattermost(t *testing.T, cc msg.ChannelConfig) (*fakeplatform.Mattermost, ChannelAdapter, context.Context) {
	t.Helper()
	fake := fakeplatform.NewMattermost(mmToken)
	t.Cleanup(fake.Close)
	cc.ServerURL = fake.URL()
	cc.BotToken = mmToken
	a, err := NewAdapter("mattermost", cc)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := a.Start(ctx); err != nil {
		t.Fatalf("Start against the fake Mattermost: %v", err)
	}
	t.Cleanup(func() { _ = a.Stop() })
	return fake, a, ctx
}

func nextInbound(t *testing.T, a ChannelAdapter) InboundMessage {
	t.Helper()
	select {
	case im := <-a.InboundCh():
		return im
	case <-time.After(5 * time.Second):
		t.Fatal("no inbound from the fake platform")
	}
	return InboundMessage{}
}

func noInbound(t *testing.T, a ChannelAdapter) {
	t.Helper()
	select {
	case im := <-a.InboundCh():
		t.Fatalf("unexpected inbound: %+v", im)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestMattermostIsABuiltInChannel(t *testing.T) {
	if !IsValidChannelType("mattermost") {
		t.Fatal("mattermost is not a valid channel type")
	}
}

func TestMattermostRoundTripAgainstFake(t *testing.T) {
	fake, a, ctx := startMattermost(t, msg.ChannelConfig{ReplyMode: "mentions"})
	if ok, bad := fake.Authentications(); ok != 1 || bad != 0 {
		t.Fatalf("websocket authentications ok=%d refused=%d, want 1/0", ok, bad)
	}
	if !a.Status().Connected {
		t.Fatalf("status = %+v", a.Status())
	}

	// A DM is addressed even in mentions mode (the DM rule).
	fake.PushDM("useralice", "alice", "hello")
	dm := nextInbound(t, a)
	if im := dm; im.SenderID != "useralice" || im.SenderName != "alice" || im.ThreadID != fake.DMChannel("useralice") ||
		im.Content != "hello" || im.Metadata["direct_message"] != "true" || im.Metadata["observe_only"] == "true" {
		t.Fatalf("DM inbound = %+v", im)
	}

	// An open-channel message without a mention is observe-only.
	fake.PushChannel("townsquare", "userbob", "bob", "lunch?")
	if im := nextInbound(t, a); im.Metadata["observe_only"] != "true" {
		t.Fatalf("un-addressed channel message not observe-only: %+v", im)
	}
	// With @mention it is addressed.
	fake.PushChannel("townsquare", "userbob", "bob", "@ryshbot status?")
	if im := nextInbound(t, a); im.Metadata["mention"] != "true" || im.Metadata["observe_only"] == "true" {
		t.Fatalf("mention not addressed: %+v", im)
	}
	// The bot's own post is dropped.
	fake.PushChannel("townsquare", fake.BotID, "ryshbot", "my own words")
	noInbound(t, a)

	// Reply into the DM's channel (RecipientID is the author, as the
	// humanoid sets it).
	if err := a.Send(ctx, OutboundMessage{RecipientID: dm.SenderID, ThreadID: dm.ThreadID, Content: "hi back"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	p := fake.Posts()
	if len(p) != 1 || p[0].ChannelID != fake.DMChannel("useralice") || p[0].Message != "hi back" {
		t.Fatalf("posts = %+v", p)
	}
}

func TestMattermostChannelAllowList(t *testing.T) {
	fake, a, _ := startMattermost(t, msg.ChannelConfig{Channels: []string{"townsquare"}})
	fake.PushChannel("offtopic", "userbob", "bob", "hi")
	noInbound(t, a)
	// A DM does not bypass the channel allow-list (E33 Q6 pin).
	fake.PushDM("useralice", "alice", "hi")
	noInbound(t, a)
	fake.PushChannel("townsquare", "userbob", "bob", "hi")
	if im := nextInbound(t, a); im.ThreadID != "townsquare" {
		t.Fatalf("inbound = %+v", im)
	}
}

func TestMattermostValidateIdentity(t *testing.T) {
	fake := fakeplatform.NewMattermost(mmToken)
	defer fake.Close()
	a, err := NewAdapter("mattermost", msg.ChannelConfig{ServerURL: fake.URL(), BotToken: mmToken})
	if err != nil {
		t.Fatal(err)
	}
	iv, ok := a.(IdentityValidator)
	if !ok {
		t.Fatal("mattermost adapter has no identity validator")
	}
	id, err := iv.ValidateIdentity(context.Background())
	if err != nil || id.Username != "ryshbot" || id.ID != fake.BotID {
		t.Fatalf("identity = %+v err=%v", id, err)
	}

	bad, _ := NewAdapter("mattermost", msg.ChannelConfig{ServerURL: fake.URL(), BotToken: "wrong-token"})
	if _, err := bad.(IdentityValidator).ValidateIdentity(context.Background()); err == nil || strings.Contains(err.Error(), "wrong-token") {
		t.Fatalf("a rejected token: err=%v", err)
	}
}

func TestMattermostStartRequiresServerAndToken(t *testing.T) {
	// Run under sandbox-exec deny-network; each refusal must name the field,
	// so a request that merely failed does not pass.
	for _, tc := range []struct {
		cc   msg.ChannelConfig
		want string
	}{
		{msg.ChannelConfig{BotToken: mmToken}, "server_url"},
		{msg.ChannelConfig{ServerURL: "https://chat.example.com"}, "bot_token"},
	} {
		a, _ := NewAdapter("mattermost", tc.cc)
		err := a.Start(context.Background())
		if err == nil {
			_ = a.Stop()
			t.Fatalf("Start accepted a config without %s", tc.want)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("refusal = %v, want it to name %s", err, tc.want)
		}
	}
}

// Run under sandbox-exec deny-network: the refusal comes before any request.
func TestMattermostRefusesCleartextRemoteServer(t *testing.T) {
	a, _ := NewAdapter("mattermost", msg.ChannelConfig{ServerURL: "http://chat.example.com", BotToken: mmToken})
	err := a.Start(context.Background())
	if err == nil {
		_ = a.Stop()
		t.Fatal("Start accepted a cleartext non-loopback server_url")
	}
	if !strings.Contains(err.Error(), "server_url") || strings.Contains(err.Error(), mmToken) {
		t.Fatalf("refusal = %v", err)
	}
}
