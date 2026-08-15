// SPDX-License-Identifier: Apache-2.0

//go:build livechannels

package channels

// LV1 Discord proof (P-5, design 010).
//
// Split the same way as Telegram, and for a harder reason. discordInboundToMessage
// drops on TWO rules that no automation can get around:
//
//	if botID != "" && m.Author.ID == botID -> "own message"
//	if m.Author.Bot                        -> "bot author"
//
// The second rule rejects a second BOT token and also rejects channel webhooks
// (Discord marks webhook authors bot=true). The only sender the adapter will
// accept is a real user account, and automating a user account is against
// Discord's terms of service. So the inbound half is human-driven by design, not
// by omission — and the outbound half is split out so something meaningful still
// runs unattended.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// liveDiscordAdapter builds a DiscordAdapter scoped to one channel and opens the
// Gateway, registering Stop as cleanup.
func liveDiscordAdapter(t *testing.T, token, channelID string) *DiscordAdapter {
	t.Helper()
	adapter := NewDiscordAdapter(msg.ChannelConfig{
		Enabled:  true,
		BotToken: token,
		Channels: []string{channelID},
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := adapter.Start(ctx); err != nil {
		// Open() fails here on a bad token, and also when a privileged intent is
		// requested but not enabled for the application — the adapter asks for
		// MessageContent, so that misconfiguration surfaces as a Start error.
		t.Fatalf("discord Start (open gateway): %v "+
			"(if this mentions intents, enable the MESSAGE CONTENT INTENT for the bot "+
			"in the Discord Developer Portal → Bot → Privileged Gateway Intents)", err)
	}
	t.Cleanup(func() { _ = adapter.Stop() })
	if st := adapter.Status(); !st.Connected {
		t.Fatalf("discord adapter not connected after Start: %s", st.Error)
	}
	return adapter
}

// TestLiveDiscordOutbound proves the automatable half of the Discord row: the bot
// token authenticates, the Gateway WebSocket completes its handshake and READY
// (Start refuses to run without the bot user id from it), and ChannelMessageSend
// is accepted for a real channel.
//
// There is no inbound assertion here on purpose: the adapter drops its own
// messages, so the bot cannot observe its own post. The proof is that the Gateway
// stayed connected and the REST send returned no error.
//
// Env:
//
//	RYSH_LIVE_DISCORD_BOT_TOKEN    bot token from the Developer Portal. The only
//	                               field Start() requires.
//	RYSH_LIVE_DISCORD_CHANNEL_ID   numeric channel id to post into. Also becomes
//	                               the adapter's allow-list, so unrelated guild
//	                               traffic never reaches InboundCh.
//
// What a human must do first:
//
//  1. Create an application + bot at https://discord.com/developers/applications
//     and copy the bot token.
//  2. Enable the MESSAGE CONTENT INTENT (Bot → Privileged Gateway Intents).
//     Without it the Gateway still connects but every m.Content arrives EMPTY,
//     which makes the round-trip nonce unmatchable — a silent, confusing failure.
//  3. Create a throwaway server, invite the bot with the bot scope and
//     Send Messages + Read Message History permissions.
//  4. Copy the channel id (Discord Settings → Advanced → Developer Mode, then
//     right-click the channel → Copy Channel ID).
func TestLiveDiscordOutbound(t *testing.T) {
	env := requireEnv(t,
		"RYSH_LIVE_DISCORD_BOT_TOKEN",
		"RYSH_LIVE_DISCORD_CHANNEL_ID",
	)
	channelID := env["RYSH_LIVE_DISCORD_CHANNEL_ID"]
	adapter := liveDiscordAdapter(t, env["RYSH_LIVE_DISCORD_BOT_TOKEN"], channelID)
	t.Logf("discord gateway connected: %s", adapter.Status().Details)

	nonce := fmt.Sprintf("rysh-lv1-discord-%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := adapter.Send(ctx, OutboundMessage{
		ThreadID: channelID,
		Content:  "rysh LV1 probe " + nonce,
	}); err != nil {
		t.Fatalf("discord Send to channel %s: %v", channelID, err)
	}
	t.Logf("message sent to channel %s; gateway still connected: %t",
		channelID, adapter.Status().Connected)
}

// TestLiveDiscordRoundTrip closes the loop: the bot posts a nonce-carrying probe
// and a HUMAN in the same channel posts the nonce back, which must arrive on
// InboundCh through the Gateway MessageCreate handler.
//
// Env: everything TestLiveDiscordOutbound needs, plus
//
//	RYSH_LIVE_DISCORD_ECHO_USER_ID   numeric Discord user id of the human who will
//	                                 post. Presence of this variable is the opt-in
//	                                 that says a human is standing by; its value is
//	                                 also asserted, so a passing run cannot be the
//	                                 bot hearing itself or some other member.
//	RYSH_LIVE_DISCORD_WAIT_SECS      seconds to wait for the echo (optional,
//	                                 default 90).
//
// What the human does: watch the channel and, when the probe appears, post any
// message CONTAINING the nonce from the log. Their user id (for the env var) comes
// from right-click → Copy User ID with Developer Mode on. They must be a real user
// account — another bot, or a channel webhook, is dropped by the adapter as
// "bot author".
func TestLiveDiscordRoundTrip(t *testing.T) {
	env := requireEnv(t,
		"RYSH_LIVE_DISCORD_BOT_TOKEN",
		"RYSH_LIVE_DISCORD_CHANNEL_ID",
		"RYSH_LIVE_DISCORD_ECHO_USER_ID",
	)
	channelID := env["RYSH_LIVE_DISCORD_CHANNEL_ID"]
	echoUser := env["RYSH_LIVE_DISCORD_ECHO_USER_ID"]
	adapter := liveDiscordAdapter(t, env["RYSH_LIVE_DISCORD_BOT_TOKEN"], channelID)

	nonce := fmt.Sprintf("rysh-lv1-discord-%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	probe := "rysh LV1 round-trip probe " + nonce + " — post this text back into the channel"
	if err := adapter.Send(ctx, OutboundMessage{ThreadID: channelID, Content: probe}); err != nil {
		t.Fatalf("discord Send to channel %s: %v", channelID, err)
	}
	wait := time.Duration(envInt("RYSH_LIVE_DISCORD_WAIT_SECS", 90)) * time.Second
	t.Logf("probe posted to channel %s; awaiting an echo from user %s within %s (nonce %s)",
		channelID, echoUser, wait, nonce)

	got := awaitInbound(t, adapter.InboundCh(), wait, func(m InboundMessage) bool {
		if m.SenderID == echoUser && m.Content == "" {
			// The single highest-value diagnostic on this platform: the event
			// arrived, the sender is right, and the text is gone — that is
			// exactly what a missing MessageContent intent looks like.
			t.Log("echo user's message arrived with EMPTY content — the MESSAGE CONTENT " +
				"privileged intent is not enabled for this bot in the Developer Portal")
		}
		return strings.Contains(m.Content, nonce)
	})
	if got.SenderID != echoUser {
		t.Fatalf("inbound SenderID = %q, want the echo user %q", got.SenderID, echoUser)
	}
	if got.ThreadID != channelID {
		t.Fatalf("inbound ThreadID = %q, want channel %q", got.ThreadID, channelID)
	}
	t.Logf("round-trip OK: channel=%s sender=%s guild=%s content_len=%d",
		got.ThreadID, got.SenderID, got.Metadata["guild_id"], len(got.Content))
}
