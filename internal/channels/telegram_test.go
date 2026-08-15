// SPDX-License-Identifier: Apache-2.0

package channels

// Tests for the Telegram adapter (C2, openclaw_roadmap design 001 §4.2 / §6).
// All network interaction is faked with httptest — no live Telegram calls.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// tgDecodeUpdate parses a recorded getUpdates-style update fixture.
func tgDecodeUpdate(t *testing.T, raw string) telegramUpdate {
	t.Helper()
	var u telegramUpdate
	if err := json.Unmarshal([]byte(raw), &u); err != nil {
		t.Fatalf("fixture does not parse: %v", err)
	}
	return u
}

// Recorded getUpdates fixtures (shapes match real Bot API payloads).
const (
	tgFixtureDM = `{"update_id":1001,"message":{"message_id":10,
		"from":{"id":111,"is_bot":false,"first_name":"Alice","username":"alice"},
		"chat":{"id":111,"type":"private"},"date":1700000000,"text":"hello bot"}}`

	tgFixtureGroup = `{"update_id":1002,"message":{"message_id":20,
		"from":{"id":222,"is_bot":false,"first_name":"Bob"},
		"chat":{"id":-100200,"type":"supergroup"},"date":1700000001,"text":"group msg"}}`

	tgFixtureForumTopic = `{"update_id":1003,"message":{"message_id":30,"message_thread_id":42,
		"from":{"id":333,"is_bot":false,"first_name":"Cara","username":"cara"},
		"chat":{"id":-1001234567890,"type":"supergroup"},"date":1700000002,"text":"topic msg"}}`

	tgFixtureBotSender = `{"update_id":1004,"message":{"message_id":40,
		"from":{"id":999,"is_bot":true,"first_name":"SomeBot","username":"somebot"},
		"chat":{"id":111,"type":"private"},"date":1700000003,"text":"from a bot"}}`

	tgFixtureNoText = `{"update_id":1005,"message":{"message_id":50,
		"from":{"id":111,"is_bot":false,"first_name":"Alice","username":"alice"},
		"chat":{"id":111,"type":"private"},"date":1700000004}}`

	// A non-message update (e.g. edited_message) arrives with message absent.
	tgFixtureNoMessage = `{"update_id":1006}`

	// Mention via entity, with an emoji before it so the entity offset is only
	// correct when interpreted as UTF-16 code units (the Bot API contract).
	tgFixtureEntityMention = `{"update_id":1007,"message":{"message_id":60,
		"from":{"id":222,"is_bot":false,"first_name":"Bob"},
		"chat":{"id":-100200,"type":"supergroup"},"date":1700000005,
		"text":"🚀 @ryshbot deploy please",
		"entities":[{"type":"mention","offset":3,"length":8}]}}`

	// Reply to one of the bot's own messages counts as a mention.
	tgFixtureReplyToBot = `{"update_id":1008,"message":{"message_id":70,
		"from":{"id":222,"is_bot":false,"first_name":"Bob"},
		"chat":{"id":-100200,"type":"supergroup"},"date":1700000006,"text":"yes do it",
		"reply_to_message":{"message_id":65,
			"from":{"id":424242,"is_bot":true,"first_name":"rysh","username":"ryshbot"},
			"chat":{"id":-100200,"type":"supergroup"},"text":"shall I deploy?"}}}`
)

func TestTelegramInboundToMessage(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		wantOK bool
		check  func(t *testing.T, im InboundMessage)
	}{
		{
			name: "basic DM", raw: tgFixtureDM, wantOK: true,
			check: func(t *testing.T, im InboundMessage) {
				if im.SenderID != "111" {
					t.Errorf("SenderID = %q, want 111", im.SenderID)
				}
				if im.SenderName != "alice" {
					t.Errorf("SenderName = %q, want alice (username preferred)", im.SenderName)
				}
				if im.Content != "hello bot" {
					t.Errorf("Content = %q", im.Content)
				}
				if im.ThreadID != "111" {
					t.Errorf("ThreadID = %q, want chat id 111", im.ThreadID)
				}
				if im.Metadata["chat_id"] != "111" || im.Metadata["message_id"] != "10" || im.Metadata["chat_type"] != "private" {
					t.Errorf("metadata = %v", im.Metadata)
				}
				// Critical: no "channel" key — the humanoid would treat it as
				// the outbound recipient and misroute the reply.
				if _, ok := im.Metadata["channel"]; ok {
					t.Error("metadata must not contain a 'channel' key")
				}
			},
		},
		{
			name: "group message, first_name fallback", raw: tgFixtureGroup, wantOK: true,
			check: func(t *testing.T, im InboundMessage) {
				if im.SenderName != "Bob" {
					t.Errorf("SenderName = %q, want first_name Bob when username absent", im.SenderName)
				}
				if im.ThreadID != "-100200" {
					t.Errorf("ThreadID = %q, want -100200", im.ThreadID)
				}
				if im.Metadata["chat_type"] != "supergroup" {
					t.Errorf("chat_type = %q", im.Metadata["chat_type"])
				}
			},
		},
		{
			name: "forum topic composes thread id", raw: tgFixtureForumTopic, wantOK: true,
			check: func(t *testing.T, im InboundMessage) {
				if im.ThreadID != "-1001234567890:42" {
					t.Errorf("ThreadID = %q, want -1001234567890:42", im.ThreadID)
				}
				if im.Metadata["message_thread_id"] != "42" {
					t.Errorf("message_thread_id metadata = %q, want 42", im.Metadata["message_thread_id"])
				}
			},
		},
		{name: "bot sender skipped", raw: tgFixtureBotSender, wantOK: false},
		{name: "non-text message skipped", raw: tgFixtureNoText, wantOK: false},
		{name: "update without message skipped", raw: tgFixtureNoMessage, wantOK: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			im, ok := telegramInboundToMessage(tgDecodeUpdate(t, tc.raw))
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && tc.check != nil {
				tc.check(t, im)
			}
		})
	}
}

// tgRecv drains one message from the adapter's inbound channel, if any.
func tgRecv(a *TelegramAdapter) (InboundMessage, bool) {
	select {
	case im := <-a.InboundCh():
		return im, true
	default:
		return InboundMessage{}, false
	}
}

func TestTelegramProcessUpdateAllowList(t *testing.T) {
	// Allow-list configured: only chat -100200 passes.
	a := NewTelegramAdapter(msg.ChannelConfig{Channels: []string{"-100200"}})

	a.processUpdate(tgDecodeUpdate(t, tgFixtureDM)) // chat 111 — disallowed
	if _, ok := tgRecv(a); ok {
		t.Fatal("message from disallowed chat must be dropped")
	}

	a.processUpdate(tgDecodeUpdate(t, tgFixtureGroup)) // chat -100200 — allowed
	if _, ok := tgRecv(a); !ok {
		t.Fatal("message from allowed chat must be enqueued")
	}

	// Empty allow-list means every chat is allowed (mirrors slack.go).
	open := NewTelegramAdapter(msg.ChannelConfig{})
	open.processUpdate(tgDecodeUpdate(t, tgFixtureDM))
	if _, ok := tgRecv(open); !ok {
		t.Fatal("empty allow-list must admit all chats")
	}
}

func TestTelegramReplyModeMentions(t *testing.T) {
	newBot := func() *TelegramAdapter {
		a := NewTelegramAdapter(msg.ChannelConfig{ReplyMode: "mentions"})
		a.botID = 424242
		a.botUsername = "ryshbot"
		return a
	}

	// Entity mention (UTF-16 offset past an emoji) → mention, not observe_only.
	a := newBot()
	a.processUpdate(tgDecodeUpdate(t, tgFixtureEntityMention))
	im, ok := tgRecv(a)
	if !ok {
		t.Fatal("mention must be enqueued")
	}
	if im.Metadata["mention"] != "true" {
		t.Error("entity mention must set metadata mention=true")
	}
	if im.Metadata["observe_only"] == "true" {
		t.Error("mention must not be observe_only in mentions mode")
	}

	// Reply-to-bot counts as a mention.
	a = newBot()
	a.processUpdate(tgDecodeUpdate(t, tgFixtureReplyToBot))
	im, ok = tgRecv(a)
	if !ok {
		t.Fatal("reply-to-bot must be enqueued")
	}
	if im.Metadata["mention"] != "true" {
		t.Error("reply to the bot's message must count as a mention")
	}

	// Plain group message in mentions mode → observe_only.
	a = newBot()
	a.processUpdate(tgDecodeUpdate(t, tgFixtureGroup))
	im, ok = tgRecv(a)
	if !ok {
		t.Fatal("non-mention must still be forwarded (observe_only)")
	}
	if im.Metadata["observe_only"] != "true" {
		t.Error("non-mention in mentions mode must be observe_only")
	}

	// Same message in default "messages" mode → processed normally.
	b := NewTelegramAdapter(msg.ChannelConfig{})
	b.botID = 424242
	b.botUsername = "ryshbot"
	b.processUpdate(tgDecodeUpdate(t, tgFixtureGroup))
	im, ok = tgRecv(b)
	if !ok {
		t.Fatal("message must be enqueued in messages mode")
	}
	if im.Metadata["observe_only"] == "true" {
		t.Error("messages mode must not mark observe_only")
	}
}

func TestTelegramMentionsBot(t *testing.T) {
	mk := func(text string) *telegramMessage { return &telegramMessage{Text: text} }

	if !telegramMentionsBot(mk("hey @ryshbot do it"), 1, "ryshbot") {
		t.Error("substring mention not detected")
	}
	if !telegramMentionsBot(mk("hey @RyshBot do it"), 1, "ryshbot") {
		t.Error("mention matching must be case-insensitive (Telegram usernames are)")
	}
	if telegramMentionsBot(mk("hey @ryshbot2 do it"), 1, "ryshbot") {
		t.Error("@ryshbot2 must not match bot ryshbot (word boundary)")
	}
	if telegramMentionsBot(mk("no mention here"), 1, "ryshbot") {
		t.Error("false positive mention")
	}
	if telegramMentionsBot(nil, 1, "ryshbot") {
		t.Error("nil message must not be a mention")
	}
	// Entity extraction honors UTF-16 offsets (emoji = 2 code units).
	m := &telegramMessage{
		Text:     "🚀 @ryshbot go",
		Entities: []telegramEntity{{Type: "mention", Offset: 3, Length: 8}},
	}
	if got := telegramEntityText(m.Text, 3, 8); got != "@ryshbot" {
		t.Errorf("telegramEntityText = %q, want @ryshbot", got)
	}
	if !telegramMentionsBot(m, 1, "ryshbot") {
		t.Error("entity mention with emoji offset not detected")
	}
	// Out-of-range entity must not panic and yields no text.
	if got := telegramEntityText("short", 10, 5); got != "" {
		t.Errorf("out-of-range entity text = %q, want empty", got)
	}
}

func TestParseTelegramThreadIDRoundTrip(t *testing.T) {
	cases := []struct {
		in         string
		wantChat   string
		wantThread int64
	}{
		{"111", "111", 0},
		{"-100200", "-100200", 0},
		{"-1001234567890:42", "-1001234567890", 42},
		{"123:7", "123", 7},
		{" 123 ", "123", 0},
		{"", "", 0},
	}
	for _, tc := range cases {
		chat, thread := parseTelegramThreadID(tc.in)
		if chat != tc.wantChat || thread != tc.wantThread {
			t.Errorf("parseTelegramThreadID(%q) = (%q, %d), want (%q, %d)",
				tc.in, chat, thread, tc.wantChat, tc.wantThread)
		}
	}

	// Round trip: the ThreadID composed by inbound mapping parses back into
	// the same chat + topic for outbound routing.
	im, ok := telegramInboundToMessage(tgDecodeUpdate(t, tgFixtureForumTopic))
	if !ok {
		t.Fatal("forum fixture must map")
	}
	chat, thread := parseTelegramThreadID(im.ThreadID)
	if chat != "-1001234567890" || thread != 42 {
		t.Errorf("round trip = (%q, %d), want (-1001234567890, 42)", chat, thread)
	}
}

func TestSplitTelegramMessage(t *testing.T) {
	if got := splitTelegramMessage("short"); len(got) != 1 || got[0] != "short" {
		t.Fatalf("short message should be a single chunk, got %v", got)
	}

	// Build content over two split lengths with newlines to break on.
	var b strings.Builder
	for b.Len() < 2*telegramSplitLen+100 {
		b.WriteString(strings.Repeat("x", 80) + "\n")
	}
	long := b.String()
	chunks := splitTelegramMessage(long)
	if len(chunks) < 3 {
		t.Errorf("expected >=3 chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if len(c) > telegramMaxMessageLen {
			t.Errorf("chunk %d length %d exceeds Telegram cap %d", i, len(c), telegramMaxMessageLen)
		}
	}
	if strings.Join(chunks, "") != long {
		t.Error("chunks must reassemble to the original content")
	}
}

func TestRenderTelegramStep(t *testing.T) {
	got := renderTelegramStep("run migrate_db: a<b & c>d")
	want := "<i>run migrate_db: a&lt;b &amp; c&gt;d</i>"
	if got != want {
		t.Errorf("renderTelegramStep = %q, want %q", got, want)
	}
	// Underscores must survive untouched — that is the point of choosing HTML
	// over Markdown parse mode.
	if !strings.Contains(got, "migrate_db") {
		t.Error("underscores must not be escaped or dropped")
	}
}

func TestTelegramInboundBufferDropsWhenFull(t *testing.T) {
	a := NewTelegramAdapter(msg.ChannelConfig{})
	u := tgDecodeUpdate(t, tgFixtureDM)
	// The inbound buffer holds 100; the 101st must be dropped, not block.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 101; i++ {
			a.processUpdate(u)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("processUpdate blocked on a full inbound buffer")
	}
	if n := len(a.inbound); n != 100 {
		t.Errorf("inbound buffered %d messages, want 100", n)
	}
}

// newFakeTelegramAPI builds an httptest server that speaks just enough Bot API
// for the poll + send round trip, recording what the adapter asked for.
type fakeTelegramAPI struct {
	mu           sync.Mutex
	offsets      []int64
	sends        []tgSendMessageParams
	sendFailures int // number of leading sendMessage calls answered with 429
	updateServed bool
}

func (f *fakeTelegramAPI) handler(t *testing.T, token string) http.HandlerFunc {
	prefix := "/bot" + token + "/"
	return func(rw http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, prefix) {
			t.Errorf("request path %q missing token prefix %q", r.URL.Path, prefix)
			http.NotFound(rw, r)
			return
		}
		switch strings.TrimPrefix(r.URL.Path, prefix) {
		case "getMe":
			_, _ = io.WriteString(rw, `{"ok":true,"result":{"id":424242,"is_bot":true,"first_name":"rysh","username":"ryshbot"}}`)
		case "getUpdates":
			var req tgGetUpdatesParams
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.mu.Lock()
			f.offsets = append(f.offsets, req.Offset)
			first := !f.updateServed
			f.updateServed = true
			f.mu.Unlock()
			if first {
				_, _ = io.WriteString(rw, `{"ok":true,"result":[`+tgFixtureDM+`]}`)
			} else {
				// Empty result; a tiny delay keeps the poll loop from
				// hammering the test server in a tight loop.
				time.Sleep(10 * time.Millisecond)
				_, _ = io.WriteString(rw, `{"ok":true,"result":[]}`)
			}
		case "sendMessage":
			var p tgSendMessageParams
			_ = json.NewDecoder(r.Body).Decode(&p)
			f.mu.Lock()
			fail := f.sendFailures > 0
			if fail {
				f.sendFailures--
			} else {
				f.sends = append(f.sends, p)
			}
			f.mu.Unlock()
			if fail {
				rw.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(rw, `{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":0}}`)
				return
			}
			_, _ = io.WriteString(rw, `{"ok":true,"result":{"message_id":900}}`)
		default:
			t.Errorf("unexpected API method: %s", r.URL.Path)
			http.NotFound(rw, r)
		}
	}
}

func TestTelegramPollAndSendRoundTrip(t *testing.T) {
	fake := &fakeTelegramAPI{}
	srv := httptest.NewServer(fake.handler(t, "TOKEN"))
	defer srv.Close()

	a := NewTelegramAdapter(msg.ChannelConfig{BotToken: "TOKEN"})
	a.apiBase = srv.URL

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := a.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = a.Stop() }()

	// getMe result must be reflected in Status details.
	st := a.Status()
	if !st.Connected || !strings.Contains(st.Details, "@ryshbot") || !strings.Contains(st.Details, "polling") {
		t.Errorf("Status = %+v, want connected with '@ryshbot ... polling' details", st)
	}

	// The poll loop must deliver the fixture DM.
	var im InboundMessage
	select {
	case im = <-a.InboundCh():
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for inbound message from poll loop")
	}
	if im.Content != "hello bot" || im.ThreadID != "111" {
		t.Errorf("inbound = %+v", im)
	}

	// Offset must advance to update_id+1 on the next poll so the update is
	// confirmed and never redelivered.
	deadline := time.Now().Add(3 * time.Second)
	for {
		fake.mu.Lock()
		advanced := len(fake.offsets) > 0 && fake.offsets[len(fake.offsets)-1] == 1002
		fake.mu.Unlock()
		if advanced {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("poll offset never advanced past the delivered update")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Reply into the inbound thread.
	if err := a.Send(ctx, OutboundMessage{RecipientID: im.ThreadID, ThreadID: im.ThreadID, Content: "pong"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	// A step to a different chat (avoids the 1s per-chat pacer) with a forum
	// topic thread — must render italic HTML and carry message_thread_id.
	if err := a.Send(ctx, OutboundMessage{ThreadID: "-1001234567890:42", Content: "compile_step", Kind: OutboundKindStep}); err != nil {
		t.Fatalf("Send step: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.sends) != 2 {
		t.Fatalf("got %d sends, want 2", len(fake.sends))
	}
	if p := fake.sends[0]; p.ChatID != "111" || p.Text != "pong" || p.ParseMode != "" || p.MessageThreadID != 0 {
		t.Errorf("reply send = %+v", p)
	}
	if p := fake.sends[1]; p.ChatID != "-1001234567890" || p.MessageThreadID != 42 ||
		p.ParseMode != "HTML" || p.Text != "<i>compile_step</i>" {
		t.Errorf("step send = %+v", p)
	}
}

func TestTelegramSend429Retry(t *testing.T) {
	fake := &fakeTelegramAPI{sendFailures: 1}
	srv := httptest.NewServer(fake.handler(t, "TOKEN"))
	defer srv.Close()

	a := NewTelegramAdapter(msg.ChannelConfig{BotToken: "TOKEN"})
	a.apiBase = srv.URL
	a.connected = true // skip Start; Send only needs connected state

	if err := a.Send(context.Background(), OutboundMessage{RecipientID: "111", Content: "retry me"}); err != nil {
		t.Fatalf("Send should succeed after a 429 retry: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.sends) != 1 || fake.sends[0].Text != "retry me" {
		t.Errorf("sends after retry = %+v", fake.sends)
	}
}

func TestTelegramSendNotConnected(t *testing.T) {
	a := NewTelegramAdapter(msg.ChannelConfig{BotToken: "TOKEN"})
	if err := a.Send(context.Background(), OutboundMessage{RecipientID: "1", Content: "x"}); err == nil {
		t.Fatal("expected error when not connected")
	}
}

func TestTelegramStartRequiresToken(t *testing.T) {
	a := NewTelegramAdapter(msg.ChannelConfig{})
	if err := a.Start(context.Background()); err == nil {
		t.Fatal("Start without bot_token must fail")
	}
	bad := NewTelegramAdapter(msg.ChannelConfig{BotToken: "T", Mode: "carrier-pigeon"})
	if err := bad.Start(context.Background()); err == nil {
		t.Fatal("Start with an invalid mode must fail")
	}
}

func TestTelegramWebhookMode(t *testing.T) {
	var (
		hookMu        sync.Mutex
		setURL        string
		deleteWebhook bool
	)
	api := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			_, _ = io.WriteString(rw, `{"ok":true,"result":{"id":424242,"is_bot":true,"first_name":"rysh","username":"ryshbot"}}`)
		case strings.HasSuffix(r.URL.Path, "/setWebhook"):
			var p struct {
				URL string `json:"url"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			hookMu.Lock()
			setURL = p.URL
			hookMu.Unlock()
			_, _ = io.WriteString(rw, `{"ok":true,"result":true}`)
		case strings.HasSuffix(r.URL.Path, "/deleteWebhook"):
			hookMu.Lock()
			deleteWebhook = true
			hookMu.Unlock()
			_, _ = io.WriteString(rw, `{"ok":true,"result":true}`)
		default:
			http.NotFound(rw, r)
		}
	}))
	defer api.Close()

	// Grab a free loopback port for the adapter's webhook listener.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	a := NewTelegramAdapter(msg.ChannelConfig{
		BotToken:    "TOKEN",
		Mode:        "webhook",
		WebhookPort: strconv.Itoa(port),
		WebhookURL:  "https://example.com/tg-hook",
	})
	a.apiBase = api.URL

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := a.Start(ctx); err != nil {
		t.Fatalf("Start (webhook): %v", err)
	}

	hookMu.Lock()
	gotURL := setURL
	hookMu.Unlock()
	if gotURL != "https://example.com/tg-hook" {
		t.Errorf("setWebhook url = %q", gotURL)
	}
	if st := a.Status(); !strings.Contains(st.Details, fmt.Sprintf("webhook :%d", port)) {
		t.Errorf("Status details = %q, want webhook :%d", st.Details, port)
	}

	// Deliver an update to the loopback webhook and expect it inbound.
	resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/", port), "application/json",
		strings.NewReader(tgFixtureDM))
	if err != nil {
		t.Fatalf("POST to webhook: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("webhook POST status = %d, want 200", resp.StatusCode)
	}
	select {
	case im := <-a.InboundCh():
		if im.Content != "hello bot" {
			t.Errorf("inbound content = %q", im.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for webhook inbound")
	}

	// Stop must deregister the webhook.
	_ = a.Stop()
	deadline := time.Now().Add(2 * time.Second)
	for {
		hookMu.Lock()
		done := deleteWebhook
		hookMu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("deleteWebhook was never called on Stop")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
