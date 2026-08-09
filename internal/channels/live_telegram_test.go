//go:build livechannels

package channels

// LV1 Telegram proof (P-5, design 010).
//
// Two tests, because exactly one half of Telegram can be automated:
//
//   - TestLiveTelegramOutbound needs only a bot token and a chat. It proves
//     getMe, sendMessage, and — the part that usually breaks in production — that
//     the getUpdates long-poll loop stays healthy rather than dying on a 409
//     "webhook is active" conflict.
//   - TestLiveTelegramRoundTrip additionally needs a HUMAN in the chat, because
//     telegramInboundToMessage drops every message whose sender has is_bot=true.
//     A second bot token would buy nothing: Telegram does not even deliver one
//     bot's group messages to another bot.
//
// Transport is long-poll ("poll" is the adapter default when Mode is empty), so
// no public endpoint and no tunnel is needed. Webhook mode is deliberately not
// exercised here — it would drag in the tunnel dependency that makes the Twilio
// and WhatsApp rows expensive.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// liveTelegramAdapter builds a long-poll TelegramAdapter scoped to one chat and
// starts it, registering Stop as cleanup. Scoping to the chat via Channels is
// what keeps unrelated traffic in a busy group out of the assertion path.
func liveTelegramAdapter(t *testing.T, token, chatID string) *TelegramAdapter {
	t.Helper()
	adapter := NewTelegramAdapter(msg.ChannelConfig{
		Enabled:  true,
		BotToken: token,
		Mode:     "poll",
		Channels: []string{chatID},
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := adapter.Start(ctx); err != nil {
		// Start fails here on a bad token (401) — the single most common setup
		// error, and the one whose message must not leak the token.
		t.Fatalf("telegram Start (getMe): %v", err)
	}
	t.Cleanup(func() { _ = adapter.Stop() })
	return adapter
}

// TestLiveTelegramOutbound proves the automatable half of the Telegram row: the
// bot token authenticates (getMe), sendMessage delivers into a real chat, and
// the getUpdates poll loop survives its first round trip.
//
// The poll-health assertion is the point. Start() flips Connected before the
// first poll returns, so a bot that already has a webhook registered — Telegram
// answers getUpdates with 409 and the loop backs off — still looks connected for
// a moment. Sampling Status() after a full poll cycle is what catches it.
//
// Env:
//
//	RYSH_LIVE_TELEGRAM_BOT_TOKEN   the BotFather token ("123456:AA…").
//	                               The only field Start() requires.
//	RYSH_LIVE_TELEGRAM_CHAT_ID     decimal chat id to post into. Negative for
//	                               groups/supergroups ("-1001234567890"),
//	                               positive for a 1:1 chat with a user.
//
// What a human must do first:
//
//  1. Create a bot with @BotFather (/newbot) and keep the token.
//  2. Create a throwaway group, add the bot to it — or just open a 1:1 chat with
//     the bot and press Start, which is what authorises it to message you.
//  3. Get the chat id: post any message in the chat, then read chat.id from
//     https://api.telegram.org/bot<TOKEN>/getUpdates.
//  4. Make sure no webhook is registered for this bot
//     (https://api.telegram.org/bot<TOKEN>/deleteWebhook) — poll and webhook are
//     mutually exclusive per bot.
func TestLiveTelegramOutbound(t *testing.T) {
	env := requireEnv(t,
		"RYSH_LIVE_TELEGRAM_BOT_TOKEN",
		"RYSH_LIVE_TELEGRAM_CHAT_ID",
	)
	chatID := env["RYSH_LIVE_TELEGRAM_CHAT_ID"]
	adapter := liveTelegramAdapter(t, env["RYSH_LIVE_TELEGRAM_BOT_TOKEN"], chatID)

	// Status().Details carries "@username polling" — the bot's public handle,
	// which is not a secret and is the useful thing to see in a failure.
	t.Logf("telegram adapter started: %s", adapter.Status().Details)

	nonce := fmt.Sprintf("rysh-lv1-telegram-%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := adapter.Send(ctx, OutboundMessage{
		ThreadID: chatID,
		Content:  "rysh LV1 probe " + nonce,
	}); err != nil {
		t.Fatalf("telegram Send to chat %s: %v", chatID, err)
	}
	t.Logf("sendMessage accepted for chat %s", chatID)

	// One full long-poll cycle is 50s; a 409/401 surfaces on the FIRST poll,
	// which returns immediately, so 15s is enough to catch a broken loop while
	// staying well inside the harness's timeout budget.
	time.Sleep(15 * time.Second)
	if st := adapter.Status(); !st.Connected {
		t.Fatalf("getUpdates loop unhealthy after the first poll: %s "+
			"(a 409 here means a webhook is registered for this bot — call deleteWebhook)", st.Error)
	}
	t.Logf("poll loop healthy: %s", adapter.Status().Details)
}

// TestLiveTelegramRoundTrip closes the loop: the bot posts a nonce-carrying
// probe and a HUMAN in the same chat replies to it, which must surface on
// InboundCh through the getUpdates poll loop.
//
// The reply must ADDRESS the bot. Telegram bots run with group privacy mode ON
// by default, meaning they only receive messages that are commands, that
// @-mention them, or that are replies to their own messages. Replying to the
// probe satisfies the third case with no BotFather changes, so that is what the
// runbook asks for. (telegramMentionsBot also treats a reply-to-bot as a mention,
// so this works under reply_mode="mentions" too.)
//
// Env: everything TestLiveTelegramOutbound needs, plus
//
//	RYSH_LIVE_TELEGRAM_ECHO_USER_ID   decimal Telegram user id of the human who
//	                                  will reply. Presence of this variable is the
//	                                  opt-in that says a human is standing by; its
//	                                  value is also asserted, so the proof cannot
//	                                  be satisfied by the bot hearing itself.
//	RYSH_LIVE_TELEGRAM_WAIT_SECS      seconds to wait for the reply (optional,
//	                                  default 90).
//
// What the human does: watch the chat, and when the probe arrives use Telegram's
// Reply on it with any text CONTAINING the nonce from the log. Their numeric user
// id (for the env var) comes from from.id in the same getUpdates output used to
// find the chat id.
func TestLiveTelegramRoundTrip(t *testing.T) {
	env := requireEnv(t,
		"RYSH_LIVE_TELEGRAM_BOT_TOKEN",
		"RYSH_LIVE_TELEGRAM_CHAT_ID",
		"RYSH_LIVE_TELEGRAM_ECHO_USER_ID",
	)
	chatID := env["RYSH_LIVE_TELEGRAM_CHAT_ID"]
	echoUser := env["RYSH_LIVE_TELEGRAM_ECHO_USER_ID"]
	adapter := liveTelegramAdapter(t, env["RYSH_LIVE_TELEGRAM_BOT_TOKEN"], chatID)

	nonce := fmt.Sprintf("rysh-lv1-telegram-%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	probe := "rysh LV1 round-trip probe " + nonce + " — REPLY to this message with the same text"
	if err := adapter.Send(ctx, OutboundMessage{ThreadID: chatID, Content: probe}); err != nil {
		t.Fatalf("telegram Send to chat %s: %v", chatID, err)
	}
	wait := time.Duration(envInt("RYSH_LIVE_TELEGRAM_WAIT_SECS", 90)) * time.Second
	t.Logf("probe posted to chat %s; awaiting a reply from user %s within %s (nonce %s)",
		chatID, echoUser, wait, nonce)

	got := awaitInbound(t, adapter.InboundCh(), wait, func(m InboundMessage) bool {
		if m.SenderID == echoUser && !strings.Contains(m.Content, nonce) {
			// The echo user spoke but the text did not carry the nonce. Almost
			// always a copy/paste slip; say so rather than time out silently.
			t.Logf("echo user posted %d chars without the nonce — resend including %q",
				len(m.Content), nonce)
		}
		return strings.Contains(m.Content, nonce)
	})
	if got.SenderID != echoUser {
		t.Fatalf("inbound SenderID = %q, want the echo user %q", got.SenderID, echoUser)
	}
	if got.ThreadID != chatID {
		t.Fatalf("inbound ThreadID = %q, want chat %q", got.ThreadID, chatID)
	}
	t.Logf("round-trip OK: chat=%s sender=%s chat_type=%s mention=%s content_len=%d",
		got.ThreadID, got.SenderID, got.Metadata["chat_type"],
		got.Metadata["mention"], len(got.Content))
}
