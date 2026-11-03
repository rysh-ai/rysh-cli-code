// SPDX-License-Identifier: Apache-2.0

package channels

// gap05 (G13 c3): the LINE adapter against a local fake Messaging API, with
// signed webhook deliveries to the adapter's loopback webhook.

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/channels/fakeplatform"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

const (
	lineToken  = "fake-line-token-gap05"
	lineSecret = "fake-line-secret-gap05"
)

type lineWebhook interface{ LocalWebhookURL() string }

func startLine(t *testing.T, cc msg.ChannelConfig) (*fakeplatform.Line, ChannelAdapter, string, context.Context) {
	t.Helper()
	fake := fakeplatform.NewLine(lineToken, lineSecret)
	t.Cleanup(fake.Close)
	cc.APIBase = fake.URL()
	cc.BotToken = lineToken
	cc.AppSecret = lineSecret
	cc.WebhookPort = "0"
	a, err := NewAdapter("line", cc)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := a.Start(ctx); err != nil {
		t.Fatalf("Start against the fake LINE API: %v", err)
	}
	t.Cleanup(func() { _ = a.Stop() })
	hook := a.(lineWebhook).LocalWebhookURL()
	return fake, a, hook, ctx
}

func TestLineIsABuiltInChannel(t *testing.T) {
	if !IsValidChannelType("line") {
		t.Fatal("line is not a valid channel type")
	}
}

func TestLineRoundTripAgainstFake(t *testing.T) {
	fake, a, hook, ctx := startLine(t, msg.ChannelConfig{ReplyMode: "mentions"})
	u, err := url.Parse(hook)
	if err != nil || u.Hostname() != "127.0.0.1" {
		t.Fatalf("webhook %q is not on loopback", hook)
	}
	if !a.Status().Connected {
		t.Fatalf("status = %+v", a.Status())
	}

	const alice = "Ualice000000000000000000000000001"
	if code := fake.DeliverDM(hook, alice, "hello"); code != http.StatusOK {
		t.Fatalf("webhook answered %d", code)
	}
	dm := nextInbound(t, a)
	if dm.SenderID != alice || dm.ThreadID != alice || dm.Content != "hello" ||
		dm.Metadata["direct_message"] != "true" || dm.Metadata["observe_only"] == "true" {
		t.Fatalf("DM inbound = %+v", dm)
	}

	const group = "Cgroup00000000000000000000000001"
	fake.DeliverGroup(hook, group, "Ubob0000000000000000000000000001", "lunch?", false)
	if im := nextInbound(t, a); im.ThreadID != group || im.Metadata["observe_only"] != "true" {
		t.Fatalf("un-addressed group message: %+v", im)
	}
	fake.DeliverGroup(hook, group, "Ubob0000000000000000000000000001", "@rysh status?", true)
	if im := nextInbound(t, a); im.Metadata["mention"] != "true" || im.Metadata["observe_only"] == "true" {
		t.Fatalf("mention not addressed: %+v", im)
	}

	// A forged signature is refused and never reaches the humanoid.
	if code := fake.DeliverForged(hook, alice, "forged"); code != http.StatusUnauthorized {
		t.Fatalf("forged webhook answered %d, want 401", code)
	}
	noInbound(t, a)

	if err := a.Send(ctx, OutboundMessage{RecipientID: dm.SenderID, ThreadID: dm.ThreadID, Content: "hi back"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := a.Send(ctx, OutboundMessage{ThreadID: dm.ThreadID, Content: "second"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	p := fake.Pushes()
	if len(p) != 2 || p[0].To != alice || p[0].Text != "hi back" || p[0].RetryKey == "" || p[0].RetryKey == p[1].RetryKey {
		t.Fatalf("pushes = %+v", p)
	}
}

func TestLineGroupAllowList(t *testing.T) {
	fake, a, hook, _ := startLine(t, msg.ChannelConfig{Channels: []string{"Callowed0000000000000000000000001"}})
	fake.DeliverGroup(hook, "Cother00000000000000000000000001", "Ubob0000000000000000000000000001", "hi", true)
	fake.DeliverDM(hook, "Ualice000000000000000000000000001", "hi") // no bypass
	noInbound(t, a)
	fake.DeliverGroup(hook, "Callowed0000000000000000000000001", "Ubob0000000000000000000000000001", "hi", true)
	if im := nextInbound(t, a); im.ThreadID != "Callowed0000000000000000000000001" {
		t.Fatalf("inbound = %+v", im)
	}
}

func TestLineValidateIdentity(t *testing.T) {
	fake := fakeplatform.NewLine(lineToken, lineSecret)
	defer fake.Close()
	a, _ := NewAdapter("line", msg.ChannelConfig{APIBase: fake.URL(), BotToken: lineToken, AppSecret: lineSecret})
	id, err := a.(IdentityValidator).ValidateIdentity(context.Background())
	if err != nil || id.ID != fake.BotID || id.Username != "123fake" {
		t.Fatalf("identity = %+v err=%v", id, err)
	}
	bad, _ := NewAdapter("line", msg.ChannelConfig{APIBase: fake.URL(), BotToken: "wrong-token", AppSecret: lineSecret})
	if _, err := bad.(IdentityValidator).ValidateIdentity(context.Background()); err == nil || strings.Contains(err.Error(), "wrong-token") {
		t.Fatalf("a rejected token: err=%v", err)
	}
}

// Run under sandbox-exec deny-network: each refusal comes before any request
// and names the field at fault.
func TestLineStartRefusesBadConfig(t *testing.T) {
	for _, tc := range []struct {
		cc   msg.ChannelConfig
		want string
	}{
		{msg.ChannelConfig{AppSecret: lineSecret}, "bot_token"},
		{msg.ChannelConfig{BotToken: lineToken}, "app_secret"},
		{msg.ChannelConfig{BotToken: lineToken, AppSecret: lineSecret, APIBase: "http://line-proxy.example.com"}, "api_base"},
	} {
		a, _ := NewAdapter("line", tc.cc)
		err := a.Start(context.Background())
		if err == nil {
			_ = a.Stop()
			t.Fatalf("Start accepted %+v", tc.want)
		}
		if !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), lineToken) || strings.Contains(err.Error(), lineSecret) {
			t.Fatalf("refusal = %v, want it to name %s and quote no secret", err, tc.want)
		}
	}
}
