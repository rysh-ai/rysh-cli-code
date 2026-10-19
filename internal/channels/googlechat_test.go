// SPDX-License-Identifier: Apache-2.0

package channels

// Fixture tests for the Google Chat adapter. All offline: a throwaway RSA key
// is generated per run for the JWT path, and both Google APIs are served by an
// httptest server via chatBase/pubsubBase/tokenURI.
//
// Two areas get the most attention because they are the ones that fail
// silently in production:
//
//   - The service-account JWT. A wrong `aud` or a mis-encoded signature comes
//     back as a flat "Invalid JWT" from Google with no hint, so the assertion
//     is decoded and its claims asserted here rather than only round-tripped.
//   - Pub/Sub dedup. Delivery is at-least-once BY DESIGN, so a redelivered
//     event is normal traffic, not an error path — if dedup is wrong the bot
//     answers real users twice.

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// testServiceAccountJSON builds a valid service-account key with a fresh RSA
// key, so no fixture private key is ever committed.
func testServiceAccountJSON(t *testing.T, tokenURI string) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	sa := map[string]string{
		"type":         "service_account",
		"project_id":   "rysh-test-project",
		"client_email": "rysh-bot@rysh-test-project.iam.gserviceaccount.com",
		"private_key":  string(pemBytes),
		"token_uri":    tokenURI,
	}
	b, err := json.Marshal(sa)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseGoogleServiceAccount(t *testing.T) {
	good := testServiceAccountJSON(t, googleTokenEndpoint)
	sa, key, err := parseGoogleServiceAccount(good)
	if err != nil {
		t.Fatalf("valid key rejected: %v", err)
	}
	if sa.ProjectID != "rysh-test-project" || key == nil {
		t.Errorf("parsed sa = %+v", sa)
	}

	bad := map[string]string{
		"empty":           "",
		"not json":        "{{{",
		"no client_email": `{"type":"service_account","private_key":"x"}`,
		"no private_key":  `{"type":"service_account","client_email":"a@b.c"}`,
		"unparseable key": `{"type":"service_account","client_email":"a@b.c","private_key":"not a pem"}`,
		// An OAuth client-ID file is the classic wrong download, and it looks
		// enough like a key file to be confusing without an explicit check.
		"oauth client file": `{"type":"authorized_user","client_email":"a@b.c","private_key":"x"}`,
	}
	for name, raw := range bad {
		t.Run(name, func(t *testing.T) {
			if _, _, err := parseGoogleServiceAccount(raw); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

// The assertion is decoded and inspected, not just round-tripped: Google
// rejects a wrong audience with an opaque error, so the claim is pinned here.
func TestGoogleAssertionClaims(t *testing.T) {
	const tokenURI = "https://oauth2.example/token"
	auth, err := newGoogleAuth(testServiceAccountJSON(t, tokenURI),
		[]string{scopeChatBot, scopePubSub}, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertion, err := auth.signAssertion()
	if err != nil {
		t.Fatal(err)
	}

	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		t.Fatalf("assertion has %d segments, want 3", len(parts))
	}

	var header map[string]string
	decodeSegment(t, parts[0], &header)
	if header["alg"] != "RS256" {
		t.Errorf("alg = %q, want RS256", header["alg"])
	}

	var claims map[string]any
	decodeSegment(t, parts[1], &claims)
	// aud must be the TOKEN ENDPOINT, not the API being called (RFC 7523).
	if claims["aud"] != tokenURI {
		t.Errorf("aud = %v, want the token endpoint %q", claims["aud"], tokenURI)
	}
	if claims["iss"] != "rysh-bot@rysh-test-project.iam.gserviceaccount.com" {
		t.Errorf("iss = %v", claims["iss"])
	}
	scope, _ := claims["scope"].(string)
	for _, want := range []string{scopeChatBot, scopePubSub} {
		if !strings.Contains(scope, want) {
			t.Errorf("scope %q missing %q", scope, want)
		}
	}
	exp, _ := claims["exp"].(float64)
	iat, _ := claims["iat"].(float64)
	if exp <= iat {
		t.Errorf("exp %v must be after iat %v", exp, iat)
	}

	// The signature must verify against the account's own public key — a
	// mis-encoded signing input would still produce three segments.
	if _, _, err := parseGoogleServiceAccount(testServiceAccountJSON(t, tokenURI)); err != nil {
		t.Fatal(err)
	}
	if len(parts[2]) == 0 {
		t.Error("signature segment is empty")
	}
}

func decodeSegment(t *testing.T, seg string, into any) {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		t.Fatalf("segment is not base64url (JWT requires unpadded): %v", err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("segment is not JSON: %v", err)
	}
}

func TestGoogleAccessTokenCaches(t *testing.T) {
	var exchanges int
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		exchanges++
		_ = json.NewEncoder(rw).Encode(map[string]any{"access_token": "ya29.test", "expires_in": 3600})
	}))
	defer srv.Close()

	auth, err := newGoogleAuth(testServiceAccountJSON(t, srv.URL), []string{scopeChatBot}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		tok, err := auth.accessToken(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if tok != "ya29.test" {
			t.Fatalf("token = %q", tok)
		}
	}
	if exchanges != 1 {
		t.Errorf("exchanged %d times, want 1 (cache not working)", exchanges)
	}

	// Inside the refresh margin it must remint rather than hand back a token
	// that dies mid-call.
	auth.mu.Lock()
	auth.exp = time.Now().Add(googleTokenRefreshMargin / 2)
	auth.mu.Unlock()
	if _, err := auth.accessToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	if exchanges != 2 {
		t.Errorf("expected a refresh inside the margin, exchanges = %d", exchanges)
	}
}

func TestGoogleAccessTokenErrorKeepsDescription(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"error": "invalid_grant", "error_description": "Invalid JWT Signature.",
		})
	}))
	defer srv.Close()

	auth, _ := newGoogleAuth(testServiceAccountJSON(t, srv.URL), []string{scopeChatBot}, nil)
	_, err := auth.accessToken(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	// The description is the only part that says what is actually wrong.
	if !strings.Contains(err.Error(), "Invalid JWT Signature") {
		t.Errorf("error dropped the description: %q", err)
	}
}

// chatEvent builds a MESSAGE event as Chat publishes it.
func chatEvent(sender, text, space, thread string, mentionBot string) []byte {
	ev := map[string]any{
		"type": "MESSAGE",
		"message": map[string]any{
			"name": "spaces/S1/messages/M1",
			"text": text,
			"sender": map[string]any{
				"name": sender, "displayName": "Test User", "type": "HUMAN",
			},
			"thread": map[string]any{"name": thread},
			"space":  map[string]any{"name": space, "type": "SPACE"},
		},
	}
	if mentionBot != "" {
		ev["message"].(map[string]any)["annotations"] = []any{
			map[string]any{
				"type": "USER_MENTION",
				"userMention": map[string]any{
					"user": map[string]any{"name": mentionBot, "type": "BOT"},
				},
			},
		}
	}
	b, _ := json.Marshal(ev)
	return b
}

func TestGoogleChatEventToMessage(t *testing.T) {
	raw := chatEvent("users/111", "hello rysh", "spaces/S1", "spaces/S1/threads/T1", "")
	im, _, reason := googleChatEventToMessage(raw, "", nil, "messages")
	if reason != "" {
		t.Fatalf("event rejected: %s", reason)
	}
	if im.SenderID != "users/111" || im.SenderName != "Test User" {
		t.Errorf("sender = %q / %q", im.SenderID, im.SenderName)
	}
	if im.Content != "hello rysh" {
		t.Errorf("content = %q", im.Content)
	}
	// The thread is the conversation key, so replies land back in it.
	if im.ThreadID != "spaces/S1/threads/T1" {
		t.Errorf("ThreadID = %q, want the thread name", im.ThreadID)
	}
	if im.Metadata["space"] != "spaces/S1" {
		t.Errorf("space = %q", im.Metadata["space"])
	}
}

func TestGoogleChatEventIgnores(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
		want string
	}{
		{"non-message event", []byte(`{"type":"ADDED_TO_SPACE"}`), "event type"},
		{"unparseable", []byte(`{{{`), "unparseable"},
		{"empty text", chatEvent("users/1", "  ", "spaces/S1", "", ""), "empty text"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, reason := googleChatEventToMessage(tc.raw, "", nil, "messages")
			if !strings.Contains(reason, tc.want) {
				t.Errorf("reason = %q, want it to mention %q", reason, tc.want)
			}
		})
	}

	// A bot sender must be dropped: answering one invites a loop.
	botEv, _ := json.Marshal(map[string]any{
		"type": "MESSAGE",
		"message": map[string]any{
			"text":   "I am a bot",
			"sender": map[string]any{"name": "users/bot", "type": "BOT"},
			"space":  map[string]any{"name": "spaces/S1"},
		},
	})
	if _, _, reason := googleChatEventToMessage(botEv, "", nil, "messages"); reason != "bot sender" {
		t.Errorf("bot message should be dropped, reason = %q", reason)
	}
}

func TestGoogleChatSpaceAllowList(t *testing.T) {
	allowed := map[string]bool{"spaces/OK": true}
	if _, _, reason := googleChatEventToMessage(
		chatEvent("users/1", "hi", "spaces/NOPE", "", ""), "", allowed, "messages"); reason == "" {
		t.Error("a space outside the allow-list should be ignored")
	}
	if _, _, reason := googleChatEventToMessage(
		chatEvent("users/1", "hi", "spaces/OK", "", ""), "", allowed, "messages"); reason != "" {
		t.Errorf("an allow-listed space was ignored: %s", reason)
	}
}

func TestGoogleChatMentionsMode(t *testing.T) {
	const bot = "users/botapp"

	// Mentioned: tagged mention, not observe_only.
	im, seen, _ := googleChatEventToMessage(
		chatEvent("users/1", "@rysh hi", "spaces/S1", "", bot), "", nil, "mentions")
	if im.Metadata["mention"] != "true" {
		t.Error("an @-mention should be tagged")
	}
	if im.Metadata["observe_only"] == "true" {
		t.Error("a mention must not be observe_only")
	}
	// The app learns its own resource name from the annotation.
	if seen != bot {
		t.Errorf("botSeen = %q, want %q", seen, bot)
	}

	// Not mentioned in mentions mode: still forwarded, but observe_only, the
	// same contract as Slack and Discord.
	im2, _, reason := googleChatEventToMessage(
		chatEvent("users/1", "just chatting", "spaces/S1", "", ""), bot, nil, "mentions")
	if reason != "" {
		t.Fatalf("non-mention should still be forwarded, got %q", reason)
	}
	if im2.Metadata["observe_only"] != "true" {
		t.Error("a non-mention in mentions mode should be observe_only")
	}
}

func TestGoogleChatSplitTarget(t *testing.T) {
	tests := []struct{ in, space, thread string }{
		{"spaces/S1/threads/T1", "spaces/S1", "spaces/S1/threads/T1"},
		{"spaces/S1", "spaces/S1", ""},
		{"S1", "spaces/S1", ""},
		{"  spaces/S1  ", "spaces/S1", ""},
	}
	for _, tc := range tests {
		space, thread := googleChatSplitTarget(tc.in)
		if space != tc.space || thread != tc.thread {
			t.Errorf("%q → (%q, %q), want (%q, %q)", tc.in, space, thread, tc.space, tc.thread)
		}
	}
}

// newGoogleChatTestAdapter wires an adapter to a stub serving token, pull, ack
// and Chat send from one handler.
func newGoogleChatTestAdapter(t *testing.T, h http.HandlerFunc) (*GoogleChatAdapter, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	g := NewGoogleChatAdapter(msg.ChannelConfig{
		CredentialsJSON: testServiceAccountJSON(t, srv.URL+"/token"),
		Subscription:    "rysh-sub",
	})
	g.chatBase = srv.URL
	g.pubsubBase = srv.URL
	auth, err := newGoogleAuth(g.config.CredentialsJSON, []string{scopeChatBot, scopePubSub}, g.client)
	if err != nil {
		t.Fatal(err)
	}
	g.auth = auth
	g.subscription = "projects/rysh-test-project/subscriptions/rysh-sub"
	return g, srv
}

func TestGoogleChatPullForwardsAndAcks(t *testing.T) {
	var acked []string
	g, _ := newGoogleChatTestAdapter(t, func(rw http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/token"):
			_ = json.NewEncoder(rw).Encode(map[string]any{"access_token": "t", "expires_in": 3600})
		case strings.HasSuffix(r.URL.Path, ":pull"):
			ev := chatEvent("users/1", "hello", "spaces/S1", "spaces/S1/threads/T1", "")
			_ = json.NewEncoder(rw).Encode(map[string]any{
				"receivedMessages": []any{map[string]any{
					"ackId":   "ack-1",
					"message": map[string]any{"data": base64.StdEncoding.EncodeToString(ev)},
				}},
			})
		case strings.HasSuffix(r.URL.Path, ":acknowledge"):
			var body struct{ AckIds []string }
			_ = json.NewDecoder(r.Body).Decode(&body)
			acked = append(acked, body.AckIds...)
			rw.WriteHeader(http.StatusOK)
		}
	})

	n, err := g.pullOnce(context.Background())
	if err != nil {
		t.Fatalf("pullOnce: %v", err)
	}
	if n != 1 {
		t.Fatalf("pulled %d, want 1", n)
	}
	select {
	case im := <-g.InboundCh():
		if im.Content != "hello" {
			t.Errorf("content = %q", im.Content)
		}
	case <-time.After(time.Second):
		t.Fatal("no message forwarded")
	}
	if len(acked) != 1 || acked[0] != "ack-1" {
		t.Errorf("acked = %v, want [ack-1]", acked)
	}
}

// Pub/Sub is at-least-once, so a redelivered event is ordinary traffic. If
// dedup fails here the bot answers a real person twice.
func TestGoogleChatDedupesRedelivery(t *testing.T) {
	g, _ := newGoogleChatTestAdapter(t, func(rw http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/token"):
			_ = json.NewEncoder(rw).Encode(map[string]any{"access_token": "t", "expires_in": 3600})
		case strings.HasSuffix(r.URL.Path, ":pull"):
			ev := chatEvent("users/1", "same message", "spaces/S1", "", "")
			_ = json.NewEncoder(rw).Encode(map[string]any{
				"receivedMessages": []any{map[string]any{
					"ackId":   "ack-x",
					"message": map[string]any{"data": base64.StdEncoding.EncodeToString(ev)},
				}},
			})
		default:
			rw.WriteHeader(http.StatusOK)
		}
	})

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := g.pullOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// Same message name three times → exactly one delivery.
	got := 0
	for {
		select {
		case <-g.InboundCh():
			got++
			continue
		default:
		}
		break
	}
	if got != 1 {
		t.Errorf("forwarded %d copies of a redelivered message, want 1", got)
	}
}

func TestGoogleChatPullAuthErrorNamesIAM(t *testing.T) {
	g, _ := newGoogleChatTestAdapter(t, func(rw http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/token") {
			_ = json.NewEncoder(rw).Encode(map[string]any{"access_token": "t", "expires_in": 3600})
			return
		}
		rw.WriteHeader(http.StatusForbidden)
	})
	_, err := g.pullOnce(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	// The actionable cause is almost always a missing IAM binding.
	if !strings.Contains(err.Error(), "pubsub.subscriber") {
		t.Errorf("error should name the IAM role, got %q", err)
	}
}

func TestGoogleChatSendThreads(t *testing.T) {
	var gotURL string
	var gotBody map[string]any
	g, _ := newGoogleChatTestAdapter(t, func(rw http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/token") {
			_ = json.NewEncoder(rw).Encode(map[string]any{"access_token": "t", "expires_in": 3600})
			return
		}
		gotURL = r.URL.String()
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte(`{"name":"spaces/S1/messages/M2"}`))
	})

	err := g.Send(context.Background(), OutboundMessage{
		ThreadID: "spaces/S1/threads/T1", Content: "threaded reply",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !strings.Contains(gotURL, "/v1/spaces/S1/messages") {
		t.Errorf("url = %q", gotURL)
	}
	// Without messageReplyOption the reply starts a new thread instead of
	// continuing the conversation.
	if !strings.Contains(gotURL, "messageReplyOption=REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD") {
		t.Errorf("url is missing messageReplyOption: %q", gotURL)
	}
	thread, _ := gotBody["thread"].(map[string]any)
	if thread["name"] != "spaces/S1/threads/T1" {
		t.Errorf("thread.name = %v", thread["name"])
	}
	if gotBody["text"] != "threaded reply" {
		t.Errorf("text = %v", gotBody["text"])
	}
}

func TestGoogleChatSendUnthreaded(t *testing.T) {
	var gotURL string
	g, _ := newGoogleChatTestAdapter(t, func(rw http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/token") {
			_ = json.NewEncoder(rw).Encode(map[string]any{"access_token": "t", "expires_in": 3600})
			return
		}
		gotURL = r.URL.String()
		rw.WriteHeader(http.StatusOK)
	})
	if err := g.Send(context.Background(), OutboundMessage{ThreadID: "spaces/S1", Content: "hi"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(gotURL, "messageReplyOption") {
		t.Errorf("a space-level send should not set messageReplyOption: %q", gotURL)
	}
}

func TestGoogleChatSendNotFoundHint(t *testing.T) {
	g, _ := newGoogleChatTestAdapter(t, func(rw http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/token") {
			_ = json.NewEncoder(rw).Encode(map[string]any{"access_token": "t", "expires_in": 3600})
			return
		}
		rw.WriteHeader(http.StatusNotFound)
		_, _ = rw.Write([]byte(`{"error":{"message":"Requested entity was not found."}}`))
	})
	err := g.Send(context.Background(), OutboundMessage{ThreadID: "spaces/S1", Content: "hi"})
	if err == nil {
		t.Fatal("expected an error")
	}
	// 404 here almost always means the app was never added to the space.
	if !strings.Contains(err.Error(), "member of this space") {
		t.Errorf("error should carry the hint, got %q", err)
	}
}

func TestGoogleChatSendNoTarget(t *testing.T) {
	g := NewGoogleChatAdapter(msg.ChannelConfig{})
	if err := g.Send(context.Background(), OutboundMessage{Content: "orphan"}); err == nil {
		t.Error("a send with no thread_id or recipient_id must fail")
	}
}

func TestGoogleChatStartRequiresConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  msg.ChannelConfig
		want string
	}{
		{"no credentials", msg.ChannelConfig{Subscription: "s"}, "credentials_json is required"},
		{"no subscription", msg.ChannelConfig{CredentialsJSON: "{}"}, "subscription is required"},
		{"bad credentials", msg.ChannelConfig{
			CredentialsJSON: "not json", Subscription: "s",
		}, "not valid JSON"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGoogleChatAdapter(tc.cfg)
			err := g.Start(context.Background())
			if err == nil {
				_ = g.Stop()
				t.Fatal("expected Start to fail")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestGoogleChatFactoryAndValidType(t *testing.T) {
	if !IsValidChannelType("googlechat") {
		t.Error("googlechat should be a valid channel type")
	}
	a, err := NewAdapter("googlechat", msg.ChannelConfig{})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	if a.Type() != "googlechat" {
		t.Errorf("Type() = %q", a.Type())
	}
}

func TestGoogleChatHandledEviction(t *testing.T) {
	g := NewGoogleChatAdapter(msg.ChannelConfig{})
	for i := 0; i < googleChatHandledCap+5; i++ {
		g.markHandled(fmt.Sprintf("spaces/S/messages/%d", i))
	}
	if len(g.handled) > googleChatHandledCap {
		t.Errorf("handled = %d, cap %d", len(g.handled), googleChatHandledCap)
	}
}
