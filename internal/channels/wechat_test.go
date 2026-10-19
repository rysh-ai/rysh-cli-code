// SPDX-License-Identifier: Apache-2.0

package channels

// Fixture tests for the WeChat adapter. Everything here runs offline: the
// signature/XML/window logic is pure, and the API calls are pointed at an
// httptest server via the adapter's apiBase field, so no test ever reaches
// api.weixin.qq.com. The live round-trip lives in live_wechat_test.go behind
// the `livechannels` build tag.

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// weChatSign computes a valid signature the way WeChat does, so tests sign
// their own requests rather than hardcoding digests.
func weChatSign(token, timestamp, nonce string) string {
	parts := []string{token, timestamp, nonce}
	sort.Strings(parts)
	sum := sha1.Sum([]byte(strings.Join(parts, "")))
	return hex.EncodeToString(sum[:])
}

func TestWeChatSignatureValid(t *testing.T) {
	const token, ts, nonce = "rysh-wc-token", "1735689600", "abc123"
	good := weChatSign(token, ts, nonce)

	tests := []struct {
		name                  string
		token, ts, nonce, sig string
		want                  bool
	}{
		{"valid", token, ts, nonce, good, true},
		{"uppercase signature accepted", token, ts, nonce, strings.ToUpper(good), true},
		{"wrong token", "other-token", ts, nonce, good, false},
		{"tampered timestamp", token, "1735689601", nonce, good, false},
		{"tampered nonce", token, ts, "zzz", good, false},
		{"empty signature", token, ts, nonce, "", false},
		// The important one: an unconfigured token must never yield an endpoint
		// that accepts everything.
		{"empty token never validates", "", ts, nonce, weChatSign("", ts, nonce), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := weChatSignatureValid(tc.token, tc.ts, tc.nonce, tc.sig); got != tc.want {
				t.Errorf("weChatSignatureValid = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseWeChatInbound(t *testing.T) {
	textXML := []byte(`<xml>
  <ToUserName><![CDATA[gh_officialaccount]]></ToUserName>
  <FromUserName><![CDATA[oUser_openid_123]]></FromUserName>
  <CreateTime>1735689600</CreateTime>
  <MsgType><![CDATA[text]]></MsgType>
  <Content><![CDATA[你好 rysh]]></Content>
  <MsgId>24016741900000001</MsgId>
</xml>`)

	got := parseWeChatInbound(textXML)
	if len(got) != 1 {
		t.Fatalf("expected 1 message, got %d", len(got))
	}
	m := got[0]
	if m.SenderID != "oUser_openid_123" {
		t.Errorf("SenderID = %q", m.SenderID)
	}
	if m.Content != "你好 rysh" {
		t.Errorf("Content = %q", m.Content)
	}
	// Official Account chats are 1:1, so the sender IS the conversation key.
	if m.ThreadID != m.SenderID {
		t.Errorf("ThreadID = %q, want it to equal SenderID", m.ThreadID)
	}
	if m.Metadata["message_id"] != "24016741900000001" {
		t.Errorf("message_id = %q", m.Metadata["message_id"])
	}
}

func TestParseWeChatInboundIgnoresNonText(t *testing.T) {
	cases := map[string][]byte{
		"image event": []byte(`<xml><FromUserName><![CDATA[o1]]></FromUserName>` +
			`<MsgType><![CDATA[image]]></MsgType><MsgId>1</MsgId></xml>`),
		"subscribe event": []byte(`<xml><FromUserName><![CDATA[o1]]></FromUserName>` +
			`<MsgType><![CDATA[event]]></MsgType><Event><![CDATA[subscribe]]></Event></xml>`),
		"text with empty body": []byte(`<xml><FromUserName><![CDATA[o1]]></FromUserName>` +
			`<MsgType><![CDATA[text]]></MsgType><Content><![CDATA[   ]]></Content></xml>`),
		"garbage": []byte(`not xml at all`),
		"empty":   []byte(``),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if got := parseWeChatInbound(body); len(got) != 0 {
				t.Errorf("expected no messages, got %d", len(got))
			}
		})
	}
}

func TestWeChatMarkHandledDedupes(t *testing.T) {
	w := NewWeChatAdapter(msg.ChannelConfig{})

	if !w.markHandled("msg-1") {
		t.Error("first delivery should be new")
	}
	if w.markHandled("msg-1") {
		t.Error("second delivery of the same MsgId should be a duplicate")
	}
	// An empty id carries nothing to compare, so it must not be swallowed.
	// Called twice: a second empty id must not look like a duplicate either.
	first, second := w.markHandled(""), w.markHandled("")
	if !first || !second {
		t.Error("empty ids should always pass through")
	}
}

func TestWeChatHandledEviction(t *testing.T) {
	w := NewWeChatAdapter(msg.ChannelConfig{})
	for i := 0; i < weChatHandledCap+10; i++ {
		w.markHandled(fmt.Sprintf("m%d", i))
	}
	if len(w.handled) > weChatHandledCap {
		t.Errorf("handled set grew to %d, cap is %d", len(w.handled), weChatHandledCap)
	}
	// The newest must still be remembered; the oldest must have been evicted.
	if w.markHandled(fmt.Sprintf("m%d", weChatHandledCap+9)) {
		t.Error("newest id should still be recognised as handled")
	}
	if !w.markHandled("m0") {
		t.Error("oldest id should have been evicted")
	}
}

// The reply window is the rule most likely to be got wrong, because WeChat
// bounds replies by time AND count and a time-only check silently passes.
func TestWeChatReplyWindow(t *testing.T) {
	w := NewWeChatAdapter(msg.ChannelConfig{})
	const user = "oUser_1"

	if ok, why := w.replyAllowed(user); ok {
		t.Error("a user who never messaged us must not be repliable")
	} else if !strings.Contains(why, "48h") {
		t.Errorf("reason should explain the window, got %q", why)
	}

	w.noteInbound(user)
	for i := 0; i < weChatRepliesPerInbound; i++ {
		if ok, why := w.replyAllowed(user); !ok {
			t.Fatalf("reply %d should be allowed, refused: %s", i+1, why)
		}
		w.noteReply(user)
	}
	ok, why := w.replyAllowed(user)
	if ok {
		t.Errorf("reply %d should be refused — WeChat allows %d",
			weChatRepliesPerInbound+1, weChatRepliesPerInbound)
	}
	if !strings.Contains(why, "5") {
		t.Errorf("reason should name the cap, got %q", why)
	}

	// A fresh inbound message resets both halves of the budget.
	w.noteInbound(user)
	if ok, why := w.replyAllowed(user); !ok {
		t.Errorf("a new inbound should reset the budget, refused: %s", why)
	}
}

func TestWeChatWindowExpires(t *testing.T) {
	w := NewWeChatAdapter(msg.ChannelConfig{})
	const user = "oUser_2"
	w.mu.Lock()
	w.windows[user] = &weChatWindow{lastInbound: time.Now().Add(-49 * time.Hour)}
	w.mu.Unlock()

	ok, why := w.replyAllowed(user)
	if ok {
		t.Error("a 49h-old window must be closed")
	}
	if !strings.Contains(why, "closed") {
		t.Errorf("reason should say the window closed, got %q", why)
	}
}

// newWeChatTestAdapter points an adapter at a stub API server.
func newWeChatTestAdapter(t *testing.T, handler http.HandlerFunc) *WeChatAdapter {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	w := NewWeChatAdapter(msg.ChannelConfig{AppID: "wxappid", AppSecret: "secret"})
	w.apiBase = srv.URL
	return w
}

func TestWeChatAccessTokenCaches(t *testing.T) {
	var fetches int
	w := newWeChatTestAdapter(t, func(rw http.ResponseWriter, r *http.Request) {
		fetches++
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"access_token": "tok-abc", "expires_in": 7200,
		})
	})

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		tok, err := w.accessToken(ctx)
		if err != nil {
			t.Fatalf("accessToken: %v", err)
		}
		if tok != "tok-abc" {
			t.Fatalf("token = %q", tok)
		}
	}
	// The cache is the whole point: WeChat caps token issuance, so three sends
	// must not mean three fetches.
	if fetches != 1 {
		t.Errorf("fetched the token %d times, want 1 (cache not working)", fetches)
	}
}

func TestWeChatAccessTokenRefreshesNearExpiry(t *testing.T) {
	var fetches int
	w := newWeChatTestAdapter(t, func(rw http.ResponseWriter, r *http.Request) {
		fetches++
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"access_token": fmt.Sprintf("tok-%d", fetches), "expires_in": 7200,
		})
	})

	ctx := context.Background()
	if _, err := w.accessToken(ctx); err != nil {
		t.Fatal(err)
	}
	// Push expiry inside the refresh margin: the next call must refetch rather
	// than hand back a token that dies mid-send.
	w.mu.Lock()
	w.tokenExp = time.Now().Add(weChatTokenRefreshMargin / 2)
	w.mu.Unlock()

	tok, err := w.accessToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if tok != "tok-2" || fetches != 2 {
		t.Errorf("expected a refresh inside the margin; token=%q fetches=%d", tok, fetches)
	}
}

func TestWeChatAccessTokenError(t *testing.T) {
	w := newWeChatTestAdapter(t, func(rw http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"errcode": weChatErrSecretFrozen, "errmsg": "appsecret frozen",
		})
	})
	_, err := w.accessToken(context.Background())
	if err == nil {
		t.Fatal("expected an error for a frozen secret")
	}
	// The hint is what makes the failure actionable in a chat app where the
	// operator sees one line and no stack.
	if !strings.Contains(err.Error(), "frozen in the WeChat console") {
		t.Errorf("error should carry the operator hint, got %q", err)
	}
}

func TestWeChatSendRespectsWindow(t *testing.T) {
	var sends int
	w := newWeChatTestAdapter(t, func(rw http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/cgi-bin/token") {
			_ = json.NewEncoder(rw).Encode(map[string]any{"access_token": "t", "expires_in": 7200})
			return
		}
		sends++
		_ = json.NewEncoder(rw).Encode(map[string]any{"errcode": 0, "errmsg": "ok"})
	})

	ctx := context.Background()
	err := w.Send(ctx, OutboundMessage{RecipientID: "oUser_9", Content: "hi"})
	if err == nil {
		t.Error("sending with no open window should fail")
	}
	if sends != 0 {
		t.Errorf("no HTTP send should have been attempted, got %d", sends)
	}

	w.noteInbound("oUser_9")
	if err := w.Send(ctx, OutboundMessage{RecipientID: "oUser_9", Content: "hi"}); err != nil {
		t.Fatalf("send inside the window failed: %v", err)
	}
	if sends != 1 {
		t.Errorf("sends = %d, want 1", sends)
	}
}

func TestWeChatSendNoRecipient(t *testing.T) {
	w := NewWeChatAdapter(msg.ChannelConfig{})
	if err := w.Send(context.Background(), OutboundMessage{Content: "orphan"}); err == nil {
		t.Error("a send with neither recipient_id nor thread_id must fail")
	}
}

func TestWeChatStartRequiresCredentials(t *testing.T) {
	tests := []struct {
		name string
		cfg  msg.ChannelConfig
		want string
	}{
		{"no app_id", msg.ChannelConfig{}, "app_id is required"},
		{"no app_secret", msg.ChannelConfig{AppID: "x"}, "app_secret is required"},
		{"no token", msg.ChannelConfig{AppID: "x", AppSecret: "y"}, "token is required"},
		// A configured AES key must be refused, not silently ignored: safe mode
		// means the bodies are encrypted, and pretending otherwise would parse
		// ciphertext as plaintext.
		{"aes key unsupported", msg.ChannelConfig{
			AppID: "x", AppSecret: "y", VerifyToken: "z", EncodingAESKey: "k",
		}, "not implemented yet"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := NewWeChatAdapter(tc.cfg)
			err := w.Start(context.Background())
			if err == nil {
				_ = w.Stop()
				t.Fatal("expected Start to fail")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestWeChatFactoryAndValidType(t *testing.T) {
	if !IsValidChannelType("wechat") {
		t.Error("wechat should be a valid channel type")
	}
	a, err := NewAdapter("wechat", msg.ChannelConfig{AppID: "x"})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	if a.Type() != "wechat" {
		t.Errorf("Type() = %q", a.Type())
	}
}

func TestWeChatStoreAndLookup(t *testing.T) {
	w := NewWeChatAdapter(msg.ChannelConfig{})
	w.storeReceived(InboundMessage{
		SenderID: "oUser_1", SenderName: "oUser_1", Content: "first",
		Metadata: map[string]string{"message_id": "wc-1"},
	})
	recent := w.RecentMessages(10)
	if len(recent) != 1 {
		t.Fatalf("expected 1 stored message, got %d", len(recent))
	}
	// Both handles must resolve, and case-insensitively — an ID typed into a
	// prompt should not miss because of capitalisation.
	if _, ok := w.GetMessage(strings.ToUpper(recent[0].ID)); !ok {
		t.Error("short ID lookup should be case-insensitive")
	}
	if _, ok := w.GetMessage("wc-1"); !ok {
		t.Error("MsgId lookup should work")
	}
	if _, ok := w.GetMessage("nope"); ok {
		t.Error("unknown ID should not resolve")
	}
}
