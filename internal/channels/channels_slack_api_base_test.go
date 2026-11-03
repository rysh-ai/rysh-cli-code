// SPDX-License-Identifier: Apache-2.0

package channels

// channels (E33): the Slack adapter honours api_base, guarded like every other
// adapter that reads it (channels.ValidateAPIBase: https, or http on loopback).
// This is what lets the level-E scenario drive the real Slack adapter against a
// local fake platform (gap05's runChannelE2E), as qa found Slack was the one
// channel without one.
//
// Neither test needs a listener or the network, so both run unchanged inside
// sandbox-exec deny-network: the guard refuses before any request, and the
// closed loopback port proves which host the adapter dialed. The tokens are
// synthetic.

import (
	"context"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func channelsSlackCfg(apiBase string) msg.ChannelConfig {
	return msg.ChannelConfig{
		Enabled:  true,
		BotToken: "xoxb-channels-synthetic",
		AppToken: "xapp-channels-synthetic",
		APIBase:  apiBase,
	}
}

// A configured api_base is the host the adapter talks to, not slack.com.
func TestChannelsSlackUsesConfiguredAPIBase(t *testing.T) {
	// Port 1 on loopback is closed: the dial fails at once, and the error
	// names the host that was dialed.
	a := NewSlackAdapter(channelsSlackCfg("https://127.0.0.1:1"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := a.Start(ctx)
	if err == nil {
		_ = a.Stop()
		t.Fatal("Start against a closed port must fail")
	}
	if !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Fatalf("Start did not dial the configured api_base; error: %v", err)
	}
	if strings.Contains(err.Error(), "slack.com") {
		t.Fatalf("Start dialed slack.com despite api_base; error: %v", err)
	}

	// Validate (rysh doctor) must honour it too.
	err = NewSlackAdapter(channelsSlackCfg("https://127.0.0.1:1")).Validate(context.Background())
	if err == nil || !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Fatalf("Validate did not dial the configured api_base; error: %v", err)
	}
}

// A cleartext remote or unresolved api_base is refused before any request:
// the bot and app tokens would otherwise travel to it.
func TestChannelsSlackRefusesUnsafeAPIBase(t *testing.T) {
	for _, base := range []string{
		"http://example.com",
		"http://127.0.0.1.nip.io:8080",
		"${SLACK_API_BASE}",
		"ftp://127.0.0.1/",
	} {
		a := NewSlackAdapter(channelsSlackCfg(base))
		ctx, cancel := context.WithCancel(context.Background())
		err := a.Start(ctx)
		cancel()
		if err == nil {
			_ = a.Stop()
			t.Errorf("api_base %q: Start must refuse", base)
			continue
		}
		if !strings.Contains(err.Error(), "api_base") {
			t.Errorf("api_base %q: Start error should name api_base, got: %v", base, err)
		}
		if err := NewSlackAdapter(channelsSlackCfg(base)).Validate(context.Background()); err == nil ||
			!strings.Contains(err.Error(), "api_base") {
			t.Errorf("api_base %q: Validate should refuse naming api_base, got: %v", base, err)
		}
	}
}
