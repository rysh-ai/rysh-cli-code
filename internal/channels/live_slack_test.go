//go:build livechannels

package channels

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// TestLiveSlackRoundTrip is P-1, LV1's headline proof (design 010 §3.2): a real
// Slack workspace, the adapter up on Socket Mode, a SECOND identity posting into
// the test channel, and the message surfacing on InboundCh on the right thread —
// then the adapter replying in that thread and the reply being observable in
// Slack's own history.
//
// Reply mode is "messages" (not "mentions") deliberately. In "messages" mode
// every non-bot, non-subtype message in an allow-listed channel is emitted on
// InboundCh unconditionally (slack.go handleEventsAPI), so the proof depends on
// nothing but delivery. "mentions" would make it depend on two extra variables:
// the probe text having to carry the bot's resolved "<@Uxxxx>" tag, and the
// AppMentionEvent/MessageEvent race that mentionDedup resolves by dropping
// whichever of the two arrives second. Neither is what P-1 is trying to prove.
//
// The second identity MUST be a USER token, not a second bot. slack.go drops any
// event with ev.BotID != "" to avoid bot-answers-bot loops, and chat.postMessage
// with a bot token always sets bot_id — a second bot's probe would be discarded
// silently and this test would time out with a healthy-looking connection.
//
// Env:
//
//	RYSH_LIVE_SLACK_BOT_TOKEN     xoxb-… bot token of the rysh Slack app
//	RYSH_LIVE_SLACK_APP_TOKEN     xapp-… app-level token, scope connections:write
//	RYSH_LIVE_SLACK_CHANNEL_ID    Cxxxxxxxx — the channel ID, not the name (a name
//	                              would force a conversations.info lookup and thus
//	                              an extra scope; the ID matches directly)
//	RYSH_LIVE_SLACK_USER_TOKEN    xoxp-… user token of a HUMAN member of that
//	                              channel, scope chat:write — this is the second
//	                              identity
//	RYSH_LIVE_SLACK_TIMEOUT_SEC   inbound wait, seconds (optional; default 60)
//
// A human must, once: create a free workspace; create a Slack app; enable Socket
// Mode; give the bot the scopes chat:write and channels:history (channels:history
// is what the message.channels event subscription requires, and it is also what
// lets this test read the thread back); subscribe to the message.channels bot
// event; install the app; invite the bot to the test channel with /invite; and
// install the same app with the USER scope chat:write to mint the xoxp- token.
func TestLiveSlackRoundTrip(t *testing.T) {
	env := requireEnv(t,
		"RYSH_LIVE_SLACK_BOT_TOKEN",
		"RYSH_LIVE_SLACK_APP_TOKEN",
		"RYSH_LIVE_SLACK_CHANNEL_ID",
		"RYSH_LIVE_SLACK_USER_TOKEN",
	)
	channelID := strings.TrimSpace(env["RYSH_LIVE_SLACK_CHANNEL_ID"])
	inboundWait := time.Duration(envInt("RYSH_LIVE_SLACK_TIMEOUT_SEC", 60)) * time.Second

	adapter := NewSlackAdapter(msg.ChannelConfig{
		Enabled:   true,
		BotToken:  env["RYSH_LIVE_SLACK_BOT_TOKEN"],
		AppToken:  env["RYSH_LIVE_SLACK_APP_TOKEN"],
		Channels:  []string{channelID},
		ReplyMode: "messages",
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Start blocks until the WebSocket is genuinely up, so a nil here already
	// means auth.test passed and Socket Mode connected.
	if err := adapter.Start(ctx); err != nil {
		t.Fatalf("slack Start (auth.test + socket mode): %v", err)
	}
	defer func() { _ = adapter.Stop() }()

	if st := adapter.Status(); !st.Connected {
		t.Fatalf("slack Status after Start: connected=false, error=%q", st.Error)
	}
	botUserID := slackBotUserID(adapter)
	t.Logf("socket mode up: bot_user=%s channel=%s", botUserID, channelID)

	// Second identity posts the probe. A per-run nonce keeps the predicate from
	// matching ordinary workspace chatter.
	second := slack.New(env["RYSH_LIVE_SLACK_USER_TOKEN"])
	nonce := fmt.Sprintf("rysh-lv1-slack-%d", time.Now().UnixNano())
	_, probeTS, err := second.PostMessage(channelID,
		slack.MsgOptionText("LV1 probe "+nonce, false))
	if err != nil {
		t.Fatalf("second identity chat.postMessage into %s: %v "+
			"(needs a USER token with chat:write whose owner is a member of the channel)",
			channelID, err)
	}
	t.Logf("second identity posted probe ts=%s; awaiting InboundCh (up to %s)…", probeTS, inboundWait)

	// Inbound proof.
	got := awaitInbound(t, adapter.InboundCh(), inboundWait, func(m InboundMessage) bool {
		return strings.Contains(m.Content, nonce)
	})
	if got.Metadata["channel"] != channelID {
		t.Fatalf("inbound metadata[channel] = %q, want %q", got.Metadata["channel"], channelID)
	}
	// A top-level message threads on itself: slack.go falls back to ev.TimeStamp
	// when ThreadTimeStamp is empty, so the thread root must be the probe's ts.
	if got.ThreadID != probeTS {
		t.Fatalf("inbound ThreadID = %q, want the probe's ts %q", got.ThreadID, probeTS)
	}
	if got.SenderID == "" || got.SenderID == botUserID {
		t.Fatalf("inbound SenderID = %q — the probe must arrive as the second identity, not as the bot (%s)",
			got.SenderID, botUserID)
	}
	if got.Metadata["observe_only"] == "true" {
		t.Fatalf("inbound was tagged observe_only in reply_mode=messages — the humanoid would never answer it")
	}
	t.Logf("inbound OK: sender=%s (%s) thread=%s content_len=%d",
		got.SenderID, got.SenderName, got.ThreadID, len(got.Content))

	// Outbound proof: reply in-thread through the adapter, then observe it in
	// Slack's own history rather than trusting PostMessage's error being nil.
	bot := slack.New(env["RYSH_LIVE_SLACK_BOT_TOKEN"])
	pong := "LV1 pong " + nonce
	if err := adapter.Send(ctx, OutboundMessage{
		RecipientID: channelID,
		ThreadID:    got.ThreadID,
		Content:     pong,
	}); err != nil {
		t.Fatalf("adapter Send (in-thread reply): %v", err)
	}
	reply := slackAwaitThreadReply(t, bot, channelID, got.ThreadID, pong, 30*time.Second)
	if reply.User != botUserID && reply.BotID == "" {
		t.Fatalf("reply ts=%s in thread %s was not posted by the bot (user=%q, bot_id set=%v)",
			reply.Timestamp, got.ThreadID, reply.User, reply.BotID != "")
	}
	if reply.ThreadTimestamp != got.ThreadID {
		t.Fatalf("reply ts=%s has thread_ts %q, want %q — it did not land in the probe's thread",
			reply.Timestamp, reply.ThreadTimestamp, got.ThreadID)
	}
	t.Logf("round-trip OK: bot reply ts=%s landed in thread %s", reply.Timestamp, got.ThreadID)
}

// TestLiveSlackOutbound is the cheaper half of P-1: it needs only the rysh app's
// own credentials — no second identity — and proves that Socket Mode comes up and
// that both outbound renderings the adapter produces are accepted by Slack and
// visible in the channel.
//
// It is worth having separately because the user token is the single hardest
// credential in the Slack set (it needs a second OAuth install with user scopes),
// and everything except the inbound leg can be proven without it.
//
// Env:
//
//	RYSH_LIVE_SLACK_BOT_TOKEN     xoxb-… (scopes chat:write, channels:history)
//	RYSH_LIVE_SLACK_APP_TOKEN     xapp-… (scope connections:write)
//	RYSH_LIVE_SLACK_CHANNEL_ID    Cxxxxxxxx, with the bot invited
func TestLiveSlackOutbound(t *testing.T) {
	env := requireEnv(t,
		"RYSH_LIVE_SLACK_BOT_TOKEN",
		"RYSH_LIVE_SLACK_APP_TOKEN",
		"RYSH_LIVE_SLACK_CHANNEL_ID",
	)
	channelID := strings.TrimSpace(env["RYSH_LIVE_SLACK_CHANNEL_ID"])

	adapter := NewSlackAdapter(msg.ChannelConfig{
		Enabled:   true,
		BotToken:  env["RYSH_LIVE_SLACK_BOT_TOKEN"],
		AppToken:  env["RYSH_LIVE_SLACK_APP_TOKEN"],
		Channels:  []string{channelID},
		ReplyMode: "messages",
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := adapter.Start(ctx); err != nil {
		t.Fatalf("slack Start (auth.test + socket mode): %v", err)
	}
	defer func() { _ = adapter.Stop() }()

	bot := slack.New(env["RYSH_LIVE_SLACK_BOT_TOKEN"])
	nonce := fmt.Sprintf("rysh-lv1-slack-out-%d", time.Now().UnixNano())

	// A normal message.
	if err := adapter.Send(ctx, OutboundMessage{
		RecipientID: channelID,
		Content:     "LV1 outbound " + nonce,
	}); err != nil {
		t.Fatalf("adapter Send (message): %v", err)
	}
	posted := slackAwaitChannelMessage(t, bot, channelID, nonce, 30*time.Second)
	t.Logf("outbound message OK: ts=%s", posted.Timestamp)

	// A progress step. This renders as a context block (slack.go Send,
	// OutboundKindStep) — a different, block-validated API path that a plain
	// message never exercises, and one that fails with invalid_blocks rather than
	// degrading. MsgOptionText doubles as the notification fallback, which is what
	// makes the nonce findable in history.
	stepNonce := nonce + "-step"
	if err := adapter.Send(ctx, OutboundMessage{
		RecipientID: channelID,
		ThreadID:    posted.Timestamp,
		Content:     "🔧 LV1 step " + stepNonce,
		Kind:        OutboundKindStep,
	}); err != nil {
		t.Fatalf("adapter Send (step / context block): %v", err)
	}
	step := slackAwaitThreadReply(t, bot, channelID, posted.Timestamp, stepNonce, 30*time.Second)
	t.Logf("outbound step OK: ts=%s in thread %s", step.Timestamp, posted.Timestamp)
}

// slackAwaitThreadReply polls conversations.replies until a message in threadTS
// contains marker, or timeout expires. Polling (not events) because the adapter
// deliberately never surfaces its own posts on InboundCh.
func slackAwaitThreadReply(t *testing.T, api *slack.Client, channelID, threadTS, marker string, timeout time.Duration) slack.Message {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		msgs, _, _, err := api.GetConversationReplies(&slack.GetConversationRepliesParameters{
			ChannelID: channelID,
			Timestamp: threadTS,
			Limit:     100,
		})
		if err != nil {
			lastErr = err
		} else {
			for _, m := range msgs {
				if strings.Contains(m.Text, marker) {
					return m
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("no message containing the run marker appeared in thread %s within %s%s",
		threadTS, timeout, slackReadHint(lastErr))
	return slack.Message{}
}

// slackAwaitChannelMessage polls conversations.history for a message containing
// marker.
func slackAwaitChannelMessage(t *testing.T, api *slack.Client, channelID, marker string, timeout time.Duration) slack.Message {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := api.GetConversationHistory(&slack.GetConversationHistoryParameters{
			ChannelID: channelID,
			Limit:     50,
		})
		if err != nil {
			lastErr = err
		} else {
			for _, m := range resp.Messages {
				if strings.Contains(m.Text, marker) {
					return m
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("no message containing the run marker appeared in %s within %s%s",
		channelID, timeout, slackReadHint(lastErr))
	return slack.Message{}
}

// slackReadHint turns a history/replies read error into the scope advice it
// almost always means. The error text is Slack's own code ("missing_scope",
// "not_in_channel"); no token material passes through it.
func slackReadHint(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("; last read error: %v (the bot token needs channels:history — "+
		"the same scope its message.channels subscription requires — and the bot must be "+
		"a member of the channel)", err)
}

// slackBotUserID reads the user id auth.test resolved, under the adapter's lock.
// Same package, so no production accessor has to be added for the test.
func slackBotUserID(a *SlackAdapter) string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.botUserID
}
