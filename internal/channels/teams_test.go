package channels

// Microsoft Teams adapter tests. Inbound activities are signed with a real
// RSA key served from a fake JWKS, so the authentication path under test is
// the one that runs in production — a test that bypassed verification would
// leave the only thing standing between this endpoint and the public internet
// unexercised.

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

const (
	testTeamsAppID      = "11111111-2222-3333-4444-555555555555"
	testTeamsServiceURL = "https://smba.trafficmanager.net/emea/"
	testTeamsConvID     = "19:meeting_abc@thread.tacv2"
	testTeamsKid        = "test-key-1"
)

// teamsSigner mints activity tokens the adapter can verify, and serves the
// JWKS that validates them.
type teamsSigner struct {
	key *rsa.PrivateKey
}

func newTeamsSigner(t *testing.T) *teamsSigner {
	t.Helper()
	// 2048 is the smallest size that is both realistic and fast enough to
	// generate per test binary.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return &teamsSigner{key: key}
}

// jwksHandler serves the OpenID metadata and the key document it points at.
func (s *teamsSigner) jwksServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var base string
	mux.HandleFunc("/keys", func(rw http.ResponseWriter, _ *http.Request) {
		n := base64.RawURLEncoding.EncodeToString(s.key.N.Bytes())
		e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(s.key.E)).Bytes())
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"keys": []map[string]string{{"kty": "RSA", "kid": testTeamsKid, "n": n, "e": e}},
		})
	})
	mux.HandleFunc("/openid", func(rw http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(rw).Encode(map[string]string{"jwks_uri": base + "/keys"})
	})
	srv := httptest.NewServer(mux)
	base = srv.URL
	t.Cleanup(srv.Close)
	return srv
}

// token signs a JWT with the given claims, defaulting the ones a valid
// activity token always carries.
func (s *teamsSigner) token(t *testing.T, claims map[string]any) string {
	t.Helper()
	full := map[string]any{
		"iss":        teamsConnectorIssuer,
		"aud":        testTeamsAppID,
		"serviceurl": testTeamsServiceURL,
		"nbf":        time.Now().Add(-time.Minute).Unix(),
		"exp":        time.Now().Add(time.Hour).Unix(),
	}
	for k, v := range claims {
		full[k] = v
	}

	header := map[string]string{"alg": "RS256", "typ": "JWT", "kid": testTeamsKid}
	if alg, ok := claims["__alg"].(string); ok {
		header["alg"] = alg
		delete(full, "__alg")
	}
	if kid, ok := claims["__kid"].(string); ok {
		header["kid"] = kid
		delete(full, "__kid")
	}

	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	signing := enc(header) + "." + enc(full)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func teamsCfg() msg.ChannelConfig {
	return msg.ChannelConfig{
		AppID:     testTeamsAppID,
		AppSecret: "secret-test",
	}
}

// newTestTeams builds an adapter whose auth helper points at the fake JWKS.
func newTestTeams(t *testing.T, s *teamsSigner, cfg msg.ChannelConfig) *TeamsAdapter {
	t.Helper()
	a := NewTeamsAdapter(cfg)
	a.auth.openIDConfigURL = s.jwksServer(t).URL + "/openid"
	a.retryBaseDelay = time.Millisecond
	return a
}

// activityJSON builds a Teams message activity.
func activityJSON(id, text string, extra map[string]any) string {
	act := map[string]any{
		"type":       "message",
		"id":         id,
		"serviceUrl": testTeamsServiceURL,
		"channelId":  "msteams",
		"from":       map[string]string{"id": "29:user-halil", "name": "Halil"},
		"recipient":  map[string]string{"id": "28:" + testTeamsAppID, "name": "rysh"},
		"conversation": map[string]any{
			"id":               testTeamsConvID,
			"conversationType": "personal",
			"tenantId":         "tenant-1",
		},
		"text": text,
	}
	for k, v := range extra {
		act[k] = v
	}
	b, _ := json.Marshal(act)
	return string(b)
}

// postActivityJSON drives the messaging endpoint directly.
func postActivityJSON(a *TeamsAdapter, body, token string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	a.handleActivity(rec, req)
	return rec
}

func drainTeams(a *TeamsAdapter) []InboundMessage {
	var out []InboundMessage
	for {
		select {
		case im := <-a.inbound:
			out = append(out, im)
		default:
			return out
		}
	}
}

// ---------------------------------------------------------------------------
// Inbound authentication.
// ---------------------------------------------------------------------------

func TestTeamsAcceptsSignedActivity(t *testing.T) {
	t.Chdir(t.TempDir())
	s := newTeamsSigner(t)
	a := newTestTeams(t, s, teamsCfg())

	rec := postActivityJSON(a, activityJSON("act-1", "what is the build status?", nil), s.token(t, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("signed activity status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	got := drainTeams(a)
	if len(got) != 1 {
		t.Fatalf("humanoid received %d messages, want 1", len(got))
	}
	if got[0].Content != "what is the build status?" {
		t.Errorf("Content = %q", got[0].Content)
	}
	if got[0].SenderName != "Halil" {
		t.Errorf("SenderName = %q", got[0].SenderName)
	}
	if got[0].ThreadID != testTeamsConvID {
		t.Errorf("ThreadID = %q, want the conversation id", got[0].ThreadID)
	}
	if _, bad := got[0].Metadata["channel"]; bad {
		t.Error(`metadata must not carry a "channel" key`)
	}
}

// The endpoint is reachable from the public internet through the operator's
// tunnel. Every one of these is a way in if it is not checked.
func TestTeamsRejectsUnauthenticatedActivities(t *testing.T) {
	s := newTeamsSigner(t)
	other := newTeamsSigner(t) // a valid key that is not Microsoft's

	cases := []struct {
		name  string
		token func(*testing.T) string
	}{
		{"no token at all", func(*testing.T) string { return "" }},
		{"signed by the wrong key", func(t *testing.T) string { return other.token(t, nil) }},
		{"audience is another bot", func(t *testing.T) string {
			return s.token(t, map[string]any{"aud": "99999999-0000-0000-0000-000000000000"})
		}},
		{"issuer is not the connector", func(t *testing.T) string {
			return s.token(t, map[string]any{"iss": "https://evil.example"})
		}},
		{"expired", func(t *testing.T) string {
			return s.token(t, map[string]any{"exp": time.Now().Add(-2 * time.Hour).Unix()})
		}},
		{"not yet valid", func(t *testing.T) string {
			return s.token(t, map[string]any{"nbf": time.Now().Add(2 * time.Hour).Unix()})
		}},
		{"serviceurl claim points elsewhere", func(t *testing.T) string {
			return s.token(t, map[string]any{"serviceurl": "https://attacker.example/"})
		}},
		{"unsigned alg=none", func(t *testing.T) string {
			return s.token(t, map[string]any{"__alg": "none"})
		}},
		{"unknown signing key", func(t *testing.T) string {
			return s.token(t, map[string]any{"__kid": "not-a-real-kid"})
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			a := newTestTeams(t, s, teamsCfg())
			rec := postActivityJSON(a, activityJSON("act-forged", "delete production", nil), tc.token(t))
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
			if got := drainTeams(a); len(got) != 0 {
				t.Errorf("a forged activity reached the humanoid (%d messages)", len(got))
			}
		})
	}
}

// A single-tenant bot's tokens are issued by its own tenant, which is only
// acceptable when that tenant is the one configured.
func TestTeamsTenantIssuerAcceptedOnlyWhenConfigured(t *testing.T) {
	s := newTeamsSigner(t)
	tenantIss := teamsLoginBase + "/tenant-1/v2.0"

	t.Run("configured tenant is accepted", func(t *testing.T) {
		t.Chdir(t.TempDir())
		cfg := teamsCfg()
		cfg.TenantID = "tenant-1"
		a := newTestTeams(t, s, cfg)
		rec := postActivityJSON(a, activityJSON("act-t", "hi", nil), s.token(t, map[string]any{"iss": tenantIss}))
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("another tenant is refused", func(t *testing.T) {
		t.Chdir(t.TempDir())
		cfg := teamsCfg()
		cfg.TenantID = "tenant-1"
		a := newTestTeams(t, s, cfg)
		rec := postActivityJSON(a, activityJSON("act-t2", "hi", nil),
			s.token(t, map[string]any{"iss": teamsLoginBase + "/tenant-2/v2.0"}))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})
}

// ---------------------------------------------------------------------------
// Inbound behaviour.
// ---------------------------------------------------------------------------

func TestTeamsDedupesRedeliveredActivity(t *testing.T) {
	t.Chdir(t.TempDir())
	s := newTeamsSigner(t)
	a := newTestTeams(t, s, teamsCfg())

	body := activityJSON("act-dup", "hello", nil)
	postActivityJSON(a, body, s.token(t, nil))
	postActivityJSON(a, body, s.token(t, nil))

	if got := drainTeams(a); len(got) != 1 {
		t.Fatalf("humanoid received %d copies of one activity, want 1", len(got))
	}
}

func TestTeamsDedupeSurvivesRestart(t *testing.T) {
	t.Chdir(t.TempDir())
	s := newTeamsSigner(t)

	first := newTestTeams(t, s, teamsCfg())
	body := activityJSON("act-answered", "already handled", nil)
	postActivityJSON(first, body, s.token(t, nil))
	if got := drainTeams(first); len(got) != 1 {
		t.Fatalf("setup: first run received %d, want 1", len(got))
	}

	second := newTestTeams(t, s, teamsCfg())
	postActivityJSON(second, body, s.token(t, nil))
	if got := drainTeams(second); len(got) != 0 {
		t.Fatalf("after restart the redelivered activity was answered again (%d)", len(got))
	}
}

// Teams wraps @mentions in <at> markup. Left in, the humanoid reads its own
// name as part of the question.
func TestTeamsStripsMentionMarkup(t *testing.T) {
	t.Chdir(t.TempDir())
	s := newTeamsSigner(t)
	a := newTestTeams(t, s, teamsCfg())

	body := activityJSON("act-mention", "<at>rysh</at> what is the build status?", map[string]any{
		"conversation": map[string]any{"id": testTeamsConvID, "conversationType": "channel"},
		"entities": []map[string]any{{
			"type":      "mention",
			"text":      "<at>rysh</at>",
			"mentioned": map[string]string{"id": "28:" + testTeamsAppID, "name": "rysh"},
		}},
	})
	postActivityJSON(a, body, s.token(t, nil))

	got := drainTeams(a)
	if len(got) != 1 {
		t.Fatalf("received %d messages, want 1", len(got))
	}
	if got[0].Content != "what is the build status?" {
		t.Errorf("Content = %q, want the mention markup removed", got[0].Content)
	}
	if got[0].Metadata["mention"] != "true" {
		t.Error("an @mention of the bot should be marked in metadata")
	}
}

// In mentions-only mode a channel message that does not address the bot is
// still forwarded, marked observe_only, so the humanoid can watch without
// answering (the slack.go / telegram.go semantics).
func TestTeamsMentionsOnlyMarksUnaddressedMessages(t *testing.T) {
	t.Chdir(t.TempDir())
	s := newTeamsSigner(t)
	cfg := teamsCfg()
	cfg.ReplyMode = "mentions"
	a := newTestTeams(t, s, cfg)

	body := activityJSON("act-chatter", "anyone else seeing flaky CI?", map[string]any{
		"conversation": map[string]any{"id": testTeamsConvID, "conversationType": "channel"},
	})
	postActivityJSON(a, body, s.token(t, nil))

	got := drainTeams(a)
	if len(got) != 1 {
		t.Fatalf("received %d messages, want 1", len(got))
	}
	if got[0].Metadata["observe_only"] != "true" {
		t.Errorf("an unaddressed channel message should be observe_only, got %v", got[0].Metadata)
	}
}

// A 1:1 chat has no mention entity — every message there is addressed to the
// bot by construction, so mentions-only must not silence it.
func TestTeamsPersonalChatAlwaysCountsAsAddressed(t *testing.T) {
	t.Chdir(t.TempDir())
	s := newTeamsSigner(t)
	cfg := teamsCfg()
	cfg.ReplyMode = "mentions"
	a := newTestTeams(t, s, cfg)

	postActivityJSON(a, activityJSON("act-dm", "are you there?", nil), s.token(t, nil))

	got := drainTeams(a)
	if len(got) != 1 {
		t.Fatalf("received %d messages, want 1", len(got))
	}
	if got[0].Metadata["observe_only"] == "true" {
		t.Error("a direct message must not be marked observe_only")
	}
}

func TestTeamsIgnoresNonMessageActivitiesButLearnsTheirServiceURL(t *testing.T) {
	t.Chdir(t.TempDir())
	s := newTeamsSigner(t)
	a := newTestTeams(t, s, teamsCfg())

	// conversationUpdate is what a bot sees when it is installed — no message,
	// but it carries the endpoint a later reply needs.
	body := activityJSON("act-install", "", map[string]any{"type": "conversationUpdate"})
	postActivityJSON(a, body, s.token(t, nil))

	if got := drainTeams(a); len(got) != 0 {
		t.Fatalf("a conversationUpdate was forwarded as a message (%d)", len(got))
	}
	if u, ok := a.serviceURLFor(testTeamsConvID); !ok || u != testTeamsServiceURL {
		t.Errorf("serviceUrl not remembered: %q, ok=%v", u, ok)
	}
}

// The bot's own posts come back as activities. Forwarding them would have the
// humanoid answer itself in a loop.
func TestTeamsIgnoresItsOwnEcho(t *testing.T) {
	t.Chdir(t.TempDir())
	s := newTeamsSigner(t)
	a := newTestTeams(t, s, teamsCfg())

	body := activityJSON("act-echo", "something I said", map[string]any{
		"from": map[string]string{"id": "28:" + testTeamsAppID, "name": "rysh"},
	})
	postActivityJSON(a, body, s.token(t, nil))

	if got := drainTeams(a); len(got) != 0 {
		t.Fatalf("the bot's own message was forwarded back to it (%d)", len(got))
	}
}

func TestTeamsConversationAllowList(t *testing.T) {
	t.Chdir(t.TempDir())
	s := newTeamsSigner(t)
	cfg := teamsCfg()
	cfg.Channels = []string{"19:allowed@thread.tacv2"}
	a := newTestTeams(t, s, cfg)

	postActivityJSON(a, activityJSON("act-blocked", "hi", nil), s.token(t, nil))
	if got := drainTeams(a); len(got) != 0 {
		t.Fatalf("a message from a non-listed conversation got through (%d)", len(got))
	}

	allowed := activityJSON("act-allowed", "hi", map[string]any{
		"conversation": map[string]any{"id": "19:allowed@thread.tacv2", "conversationType": "channel"},
	})
	postActivityJSON(a, allowed, s.token(t, nil))
	if got := drainTeams(a); len(got) != 1 {
		t.Fatalf("a message from the listed conversation was dropped (%d)", len(got))
	}
}

// ---------------------------------------------------------------------------
// Outbound.
// ---------------------------------------------------------------------------

// fakeConnector stands in for the Bot Framework Connector API and the Entra
// token endpoint.
type fakeConnector struct {
	mu        sync.Mutex
	posts     []teamsOutboundActivity
	paths     []string
	authSeen  []string
	tokensCut int // number of tokens issued
	respond   func(n int) (int, string)
}

func newFakeConnector(t *testing.T, respond func(n int) (int, string)) (*fakeConnector, *httptest.Server) {
	t.Helper()
	f := &fakeConnector{respond: respond}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(rw http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.tokensCut++
		n := f.tokensCut
		f.mu.Unlock()
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"access_token": fmt.Sprintf("connector-token-%d", n),
			"expires_in":   3600,
			"token_type":   "Bearer",
		})
	})
	mux.HandleFunc("/v3/", func(rw http.ResponseWriter, r *http.Request) {
		var act teamsOutboundActivity
		_ = json.NewDecoder(r.Body).Decode(&act)
		f.mu.Lock()
		f.posts = append(f.posts, act)
		f.paths = append(f.paths, r.URL.Path)
		f.authSeen = append(f.authSeen, r.Header.Get("Authorization"))
		n := len(f.posts)
		f.mu.Unlock()

		status, body := http.StatusOK, `{"id":"sent-1"}`
		if f.respond != nil {
			status, body = f.respond(n)
		}
		rw.WriteHeader(status)
		_, _ = rw.Write([]byte(body))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeConnector) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.posts)
}

// sendableTeams returns a connected adapter that can reach the fake connector
// and already knows how to route to testTeamsConvID.
func sendableTeams(t *testing.T, respond func(n int) (int, string)) (*TeamsAdapter, *fakeConnector) {
	t.Helper()
	s := newTeamsSigner(t)
	fake, srv := newFakeConnector(t, respond)
	a := newTestTeams(t, s, teamsCfg())
	a.auth.tokenURL = srv.URL + "/token"
	a.connected = true
	a.rememberConversation(testTeamsConvID, srv.URL)
	return a, fake
}

func TestTeamsSendPostsToTheConversation(t *testing.T) {
	t.Chdir(t.TempDir())
	a, fake := sendableTeams(t, nil)

	if err := a.Send(context.Background(), OutboundMessage{
		ThreadID: testTeamsConvID, Content: "build is green",
	}); err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	if fake.count() != 1 {
		t.Fatalf("posted %d activities, want 1", fake.count())
	}
	if fake.posts[0].Text != "build is green" {
		t.Errorf("Text = %q", fake.posts[0].Text)
	}
	if fake.posts[0].Type != "message" {
		t.Errorf("Type = %q, want \"message\"", fake.posts[0].Type)
	}
	if !strings.Contains(fake.paths[0], "/v3/conversations/") {
		t.Errorf("path = %q, want the conversations endpoint", fake.paths[0])
	}
	if fake.authSeen[0] != "Bearer connector-token-1" {
		t.Errorf("Authorization = %q, want the Entra token", fake.authSeen[0])
	}
}

// Teams only accepts a reply on the endpoint that delivered the message. A
// conversation we have never heard from is genuinely unreachable, and saying
// so beats posting into a URL we guessed.
func TestTeamsSendRefusesUnknownConversation(t *testing.T) {
	t.Chdir(t.TempDir())
	a, fake := sendableTeams(t, nil)

	err := a.Send(context.Background(), OutboundMessage{
		ThreadID: "19:never-seen@thread.tacv2", Content: "hello?",
	})
	if err == nil {
		t.Fatal("Send must fail when the conversation's serviceUrl is unknown")
	}
	if !strings.Contains(err.Error(), "NOT sent") {
		t.Errorf("error must state nothing was sent, got: %v", err)
	}
	if fake.count() != 0 {
		t.Errorf("nothing should have been posted, got %d", fake.count())
	}
}

func TestTeamsSendRefusesWhenNotConnected(t *testing.T) {
	t.Chdir(t.TempDir())
	s := newTeamsSigner(t)
	a := newTestTeams(t, s, teamsCfg())
	if err := a.Send(context.Background(), OutboundMessage{
		ThreadID: testTeamsConvID, Content: "hello",
	}); err == nil {
		t.Fatal("Send must fail before Start")
	}
}

func TestTeamsSendSplitsLongContent(t *testing.T) {
	t.Chdir(t.TempDir())
	a, fake := sendableTeams(t, nil)

	long := strings.Repeat("b", teamsSplitLen+500)
	if err := a.Send(context.Background(), OutboundMessage{
		ThreadID: testTeamsConvID, Content: long,
	}); err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	if fake.count() < 2 {
		t.Fatalf("long content posted as %d activity(s), want it split", fake.count())
	}
	var rebuilt strings.Builder
	for _, p := range fake.posts {
		if len(p.Text) > teamsSplitLen {
			t.Errorf("chunk is %d chars, over the %d limit", len(p.Text), teamsSplitLen)
		}
		rebuilt.WriteString(p.Text)
	}
	if rebuilt.String() != long {
		t.Error("the split lost or reordered content")
	}
}

// Progress steps render de-emphasized. The xml format is used because a step
// title containing markdown punctuation would otherwise break the italics.
func TestTeamsStepRendersDeEmphasizedAndEscaped(t *testing.T) {
	t.Chdir(t.TempDir())
	a, fake := sendableTeams(t, nil)

	if err := a.Send(context.Background(), OutboundMessage{
		ThreadID: testTeamsConvID,
		Content:  "run migrate_db <fast>",
		Kind:     OutboundKindStep,
	}); err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	got := fake.posts[0]
	if got.TextFormat != "xml" {
		t.Errorf("TextFormat = %q, want xml", got.TextFormat)
	}
	if !strings.HasPrefix(got.Text, "<i>") || !strings.HasSuffix(got.Text, "</i>") {
		t.Errorf("step should be italicised, got %q", got.Text)
	}
	if strings.Contains(got.Text, "<fast>") {
		t.Errorf("user content must be escaped, got %q", got.Text)
	}
}

// A secret can be rotated under a running bot; one refresh-and-retry turns a
// dead channel back into a live one without operator action.
func TestTeamsSendRefreshesTokenOnUnauthorized(t *testing.T) {
	t.Chdir(t.TempDir())
	a, fake := sendableTeams(t, func(n int) (int, string) {
		if n == 1 {
			return http.StatusUnauthorized, `{"error":{"code":"Unauthorized","message":"expired"}}`
		}
		return http.StatusOK, `{"id":"sent-1"}`
	})

	if err := a.Send(context.Background(), OutboundMessage{
		ThreadID: testTeamsConvID, Content: "hello",
	}); err != nil {
		t.Fatalf("Send should have succeeded after refreshing, got %v", err)
	}
	if fake.count() != 2 {
		t.Errorf("made %d attempts, want 2", fake.count())
	}
	if fake.authSeen[1] == fake.authSeen[0] {
		t.Errorf("the retry reused the rejected token %q", fake.authSeen[1])
	}
}

// A 403 means the bot is not a member of that conversation. Retrying cannot
// change that, and a caller waiting on three round trips learns nothing new.
func TestTeamsSendDoesNotRetryForbidden(t *testing.T) {
	t.Chdir(t.TempDir())
	a, fake := sendableTeams(t, func(int) (int, string) {
		return http.StatusForbidden, `{"error":{"code":"BotNotInConversationRoster","message":"not a member"}}`
	})

	err := a.Send(context.Background(), OutboundMessage{ThreadID: testTeamsConvID, Content: "hello"})
	if err == nil {
		t.Fatal("Send must fail when the bot cannot post")
	}
	if !strings.Contains(err.Error(), "NOT sent") {
		t.Errorf("error must state nothing was sent, got: %v", err)
	}
	if !strings.Contains(err.Error(), "BotNotInConversationRoster") {
		t.Errorf("error should carry the Connector's reason, got: %v", err)
	}
	if fake.count() != 1 {
		t.Errorf("made %d attempts, want exactly 1", fake.count())
	}
}

// The routing table is what makes a restarted session able to reply at all.
func TestTeamsServiceURLSurvivesRestart(t *testing.T) {
	t.Chdir(t.TempDir())
	s := newTeamsSigner(t)

	first := newTestTeams(t, s, teamsCfg())
	postActivityJSON(first, activityJSON("act-route", "hi", nil), s.token(t, nil))

	second := newTestTeams(t, s, teamsCfg())
	u, ok := second.serviceURLFor(testTeamsConvID)
	if !ok || u != testTeamsServiceURL {
		t.Fatalf("restored serviceUrl = %q, ok=%v — a restart must stay able to reply", u, ok)
	}
}

// ---------------------------------------------------------------------------
// Configuration and status.
// ---------------------------------------------------------------------------

func TestTeamsStartRequiresCredentials(t *testing.T) {
	cases := []struct {
		name string
		cfg  msg.ChannelConfig
		want string
	}{
		{"no app_id", msg.ChannelConfig{AppSecret: "s"}, "app_id"},
		{"no app_secret", msg.ChannelConfig{AppID: "a"}, "app_secret"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := NewTeamsAdapter(tc.cfg)
			err := a.Start(context.Background())
			if err == nil {
				t.Fatal("Start must fail when a credential is missing")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error should name %q, got %v", tc.want, err)
			}
			if a.Status().Connected {
				t.Error("Status must not report Connected after a failed Start")
			}
		})
	}
}

// Bad credentials must fail at Start, not at the first attempted reply hours
// later — the same reason telegram.go calls getMe first.
func TestTeamsStartFailsOnRejectedCredentials(t *testing.T) {
	t.Chdir(t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusUnauthorized)
		_, _ = rw.Write([]byte(`{"error":"invalid_client","error_description":"secret is expired"}`))
	}))
	defer srv.Close()

	a := NewTeamsAdapter(teamsCfg())
	a.auth.tokenURL = srv.URL

	err := a.Start(context.Background())
	if err == nil {
		t.Fatal("Start must fail when Entra rejects the credentials")
	}
	if !strings.Contains(err.Error(), "secret is expired") {
		t.Errorf("error should carry Entra's explanation, got %v", err)
	}
	if a.Status().Connected {
		t.Error("Status must not report Connected")
	}
}

func TestTeamsValidateChecksCredentialsWithoutBinding(t *testing.T) {
	t.Chdir(t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"access_token": "tok", "expires_in": 3600,
		})
	}))
	defer srv.Close()

	a := NewTeamsAdapter(teamsCfg())
	a.auth.tokenURL = srv.URL
	if err := a.Validate(context.Background()); err != nil {
		t.Errorf("valid credentials should pass, got %v", err)
	}
	if a.Status().Connected {
		t.Error("Validate must not connect the adapter")
	}
}

// The token is cached: re-fetching on every activity would add an Entra round
// trip to every reply.
func TestTeamsTokenIsCachedUntilExpiry(t *testing.T) {
	t.Chdir(t.TempDir())
	a, fake := sendableTeams(t, nil)

	for i := 0; i < 3; i++ {
		if err := a.Send(context.Background(), OutboundMessage{
			ThreadID: testTeamsConvID, Content: fmt.Sprintf("message %d", i),
		}); err != nil {
			t.Fatalf("Send %d failed: %v", i, err)
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.tokensCut != 1 {
		t.Errorf("fetched %d tokens for 3 sends, want 1", fake.tokensCut)
	}
}

func TestTeamsStatusReportsRoutingReach(t *testing.T) {
	t.Chdir(t.TempDir())
	a, _ := sendableTeams(t, nil)
	st := a.Status()
	if st.Type != "teams" {
		t.Errorf("Type = %q", st.Type)
	}
	if !strings.Contains(st.Details, testTeamsAppID) {
		t.Errorf("details should name the app, got %q", st.Details)
	}
	if !strings.Contains(st.Details, "1 conversation") {
		t.Errorf("details should report how many conversations are reachable, got %q", st.Details)
	}
}

func TestStripTeamsMentions(t *testing.T) {
	cases := []struct{ in, want string }{
		{"<at>rysh</at> hello", "hello"},
		{"<at id=\"0\">rysh bot</at>  status?", "status?"},
		{"hi <at>rysh</at> and <at>ops</at> both", "hi and both"},
		{"no mentions here", "no mentions here"},
		{"<at>rysh</at>", ""},
	}
	for _, tc := range cases {
		if got := stripTeamsMentions(tc.in); got != tc.want {
			t.Errorf("stripTeamsMentions(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
