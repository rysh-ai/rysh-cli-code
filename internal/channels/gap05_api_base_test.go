// SPDX-License-Identifier: Apache-2.0

package channels

// gap05 (G13): ChannelConfig.APIBase points an adapter at another API root — a
// self-hosted Telegram Bot API server, or a local fake platform in tests. A bot
// token rides in every request, so a non-empty api_base must be https, or
// plain http only on a loopback host; anything else is refused before any
// request is made.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/channels/fakeplatform"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func TestValidateAPIBase(t *testing.T) {
	for _, tc := range []struct {
		raw string
		ok  bool
	}{
		{"", true},
		{"https://api.telegram.org", true},
		{"https://bots.example.internal:8443/root", true},
		{"http://127.0.0.1:8081", true},
		{"http://localhost:8081", true},
		{"http://[::1]:8081", true},
		{"http://bots.example.com", false},
		{"http://10.0.0.5:8081", false},
		{"ftp://127.0.0.1", false},
		{"127.0.0.1:8081", false},
		{"https://", false},
		{"https://user:pw@example.com", false},
		{"${TG_API_BASE}", false},
		// lead review: loopback look-alikes and userinfo must not pass as loopback.
		{"http://localhost.evil.com", false},
		{"http://127.0.0.1.nip.io:8081", false},
		{"http://127.0.0.1@evil.com", false},
		{"http://127.0.0.1:8081@evil.com", false},
		{"http://[::1]", true},
	} {
		err := ValidateAPIBase(tc.raw)
		if (err == nil) != tc.ok {
			t.Errorf("ValidateAPIBase(%q) err=%v, want ok=%v", tc.raw, err, tc.ok)
		}
	}
}

func TestTelegramUsesConfiguredAPIBase(t *testing.T) {
	fake := fakeplatform.NewTelegram("fake-token-gap05")
	defer fake.Close()

	a, err := NewAdapter("telegram", msg.ChannelConfig{BotToken: "fake-token-gap05", APIBase: fake.URL()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := a.Start(ctx); err != nil {
		t.Fatalf("Start against the fake: %v", err)
	}
	defer func() { _ = a.Stop() }()

	fake.PushPrivate(5550001, "alice", "hello")
	select {
	case im := <-a.InboundCh():
		if im.SenderID != "5550001" || im.Content != "hello" {
			t.Fatalf("inbound = %+v", im)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no inbound from the fake Bot API")
	}
	if err := a.Send(ctx, OutboundMessage{RecipientID: "5550001", ThreadID: "5550001", Content: "hi back"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if s := fake.Sends(); len(s) != 1 || s[0].ChatID != "5550001" || s[0].Text != "hi back" {
		t.Fatalf("fake sends = %+v", s)
	}
}

func TestTelegramRefusesCleartextRemoteAPIBase(t *testing.T) {
	a, err := NewAdapter("telegram", msg.ChannelConfig{BotToken: "fake-token-gap05", APIBase: "http://bots.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	err = a.Start(context.Background())
	if err == nil {
		_ = a.Stop()
		t.Fatal("Start accepted a cleartext non-loopback api_base")
	}
	if !strings.Contains(err.Error(), "api_base") {
		t.Fatalf("error does not name api_base: %v", err)
	}
	if strings.Contains(err.Error(), "fake-token-gap05") {
		t.Fatal("refusal leaked the bot token")
	}
}
