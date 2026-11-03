// SPDX-License-Identifier: Apache-2.0

package fakeplatform

import (
	"bytes"
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
	"net/url"
	"strings"
	"sync"
	"time"
)

// TeamsSend is one activity POSTed to the fake Connector.
type TeamsSend struct {
	ConversationID string
	Text           string
}

// Teams is a fake Bot Framework: the Entra client-credentials token endpoint
// (/token), the OpenID metadata (/openid) and the JWKS it points at (/keys),
// and the Connector a reply goes to (/v3/conversations/{id}/activities).
// DeliverDM / DeliverChannel sign an activity with the fake's own RSA key
// (RS256, iss api.botframework.com, aud = the bot's app id) and POST it to the
// adapter's messaging endpoint. The adapter trusts that key only because a
// test pointed it here with channels.SetTeamsAuthEndpointsForTest.
type Teams struct {
	AppID     string
	AppSecret string
	BotID     string

	srv *httptest.Server
	key *rsa.PrivateKey

	mu     sync.Mutex
	sends  []TeamsSend
	nextID int
}

const (
	teamsFakeKid         = "fake-kid-1"
	teamsFakeAccessToken = "fake-teams-access-token"
)

// NewTeams starts the fake on 127.0.0.1 for one bot registration.
func NewTeams(appID, appSecret string) *Teams {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(fmt.Sprintf("fakeplatform: teams key: %v", err))
	}
	f := &Teams{AppID: appID, AppSecret: appSecret, BotID: "28:" + appID, key: key, nextID: 1}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

// TokenURL and OpenIDURL are what channels.SetTeamsAuthEndpointsForTest takes.
func (f *Teams) TokenURL() string  { return f.srv.URL + "/token" }
func (f *Teams) OpenIDURL() string { return f.srv.URL + "/openid" }

// Close stops the server.
func (f *Teams) Close() { f.srv.Close() }

// DMConversation is the 1:1 conversation id for user.
func (f *Teams) DMConversation(user string) string { return "a:dm-" + user }

// Sends returns a copy of every activity the Connector received.
func (f *Teams) Sends() []TeamsSend {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]TeamsSend(nil), f.sends...)
}

func (f *Teams) serve(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/token":
		_ = r.ParseForm()
		if r.PostForm.Get("client_id") != f.AppID || r.PostForm.Get("client_secret") != f.AppSecret ||
			r.PostForm.Get("grant_type") != "client_credentials" {
			rw.WriteHeader(http.StatusUnauthorized)
			_, _ = rw.Write([]byte(`{"error":"invalid_client","error_description":"bad credentials"}`))
			return
		}
		fmt.Fprintf(rw, `{"access_token":%q,"expires_in":3600,"token_type":"Bearer"}`, teamsFakeAccessToken)
	case r.Method == http.MethodGet && r.URL.Path == "/openid":
		fmt.Fprintf(rw, `{"issuer":"https://api.botframework.com","jwks_uri":%q}`, f.srv.URL+"/keys")
	case r.Method == http.MethodGet && r.URL.Path == "/keys":
		pub := f.key.PublicKey
		fmt.Fprintf(rw, `{"keys":[{"kty":"RSA","kid":%q,"use":"sig","n":%q,"e":%q}]}`, teamsFakeKid,
			base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()))
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v3/conversations/") &&
		strings.HasSuffix(r.URL.Path, "/activities"):
		if r.Header.Get("Authorization") != "Bearer "+teamsFakeAccessToken {
			rw.WriteHeader(http.StatusUnauthorized)
			return
		}
		conv, _ := url.PathUnescape(strings.TrimSuffix(strings.TrimPrefix(r.URL.EscapedPath(), "/v3/conversations/"), "/activities"))
		var act struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&act)
		f.mu.Lock()
		f.sends = append(f.sends, TeamsSend{ConversationID: conv, Text: act.Text})
		id := f.nextID
		f.nextID++
		f.mu.Unlock()
		fmt.Fprintf(rw, `{"id":"out-%d"}`, id)
	default:
		rw.WriteHeader(http.StatusNotFound)
	}
}

// DeliverDM POSTs a signed 1:1 ("personal") message from user to hook.
func (f *Teams) DeliverDM(hook, user, text string) {
	f.deliver(hook, user, text, f.DMConversation(user), "personal")
}

// DeliverChannel POSTs a signed channel message with no @mention of the bot.
func (f *Teams) DeliverChannel(hook, conversationID, user, text string) {
	f.deliver(hook, user, text, conversationID, "channel")
}

func (f *Teams) deliver(hook, user, text, conv, convType string) {
	f.mu.Lock()
	id := f.nextID
	f.nextID++
	f.mu.Unlock()
	act := map[string]any{
		"type":         "message",
		"id":           fmt.Sprintf("act-%d", id),
		"serviceUrl":   f.srv.URL,
		"channelId":    "msteams",
		"from":         map[string]string{"id": user, "name": "u" + user},
		"recipient":    map[string]string{"id": f.BotID, "name": "rysh"},
		"text":         text,
		"conversation": map[string]string{"id": conv, "conversationType": convType, "tenantId": "tenant-fake"},
	}
	body, _ := json.Marshal(act)
	req, err := http.NewRequest(http.MethodPost, hook, bytes.NewReader(body))
	if err != nil {
		panic(fmt.Sprintf("fakeplatform: teams request: %v", err))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.sign())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		panic(fmt.Sprintf("fakeplatform: teams delivery: %v", err))
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		panic(fmt.Sprintf("fakeplatform: teams endpoint answered HTTP %d", resp.StatusCode))
	}
}

// sign mints the activity JWT the Bot Connector would send.
func (f *Teams) sign() string {
	enc := base64.RawURLEncoding.EncodeToString
	hdr, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": teamsFakeKid, "typ": "JWT"})
	now := time.Now().Unix()
	claims, _ := json.Marshal(map[string]any{
		"iss": "https://api.botframework.com", "aud": f.AppID,
		"nbf": now - 60, "exp": now + 600, "serviceurl": f.srv.URL,
	})
	signing := enc(hdr) + "." + enc(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, sum[:])
	if err != nil {
		panic(fmt.Sprintf("fakeplatform: teams sign: %v", err))
	}
	return signing + "." + enc(sig)
}
