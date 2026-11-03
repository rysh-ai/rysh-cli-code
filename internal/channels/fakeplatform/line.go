// SPDX-License-Identifier: Apache-2.0

package fakeplatform

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// LinePush is one POST /v2/bot/message/push the fake received (one per text).
type LinePush struct {
	To       string
	Text     string
	RetryKey string
}

// Line is a fake LINE Messaging API (bot/info, message/push) plus a webhook
// sender: DeliverDM / DeliverGroup sign an event body with the channel secret
// (X-Line-Signature) and POST it to the adapter's webhook, as LINE does.
type Line struct {
	Token  string
	Secret string
	BotID  string

	srv *httptest.Server

	mu     sync.Mutex
	pushes []LinePush
	keys   map[string]bool
	nextID int
}

// NewLine starts a fake LINE API on 127.0.0.1 accepting only token.
func NewLine(token, secret string) *Line {
	f := &Line{Token: token, Secret: secret, BotID: "Ubot0000000000000000000000000001", keys: map[string]bool{}, nextID: 1}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

// URL is the API root to put in ChannelConfig.APIBase.
func (f *Line) URL() string { return f.srv.URL }

// Close stops the server.
func (f *Line) Close() { f.srv.Close() }

// Pushes returns a copy of every pushed text so far.
func (f *Line) Pushes() []LinePush {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]LinePush(nil), f.pushes...)
}

func (f *Line) serve(rw http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+f.Token {
		rw.WriteHeader(http.StatusUnauthorized)
		_, _ = rw.Write([]byte(`{"message":"Authentication failed"}`))
		return
	}
	rw.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v2/bot/info":
		fmt.Fprintf(rw, `{"userId":%q,"basicId":"@123fake","displayName":"rysh"}`, f.BotID)
	case r.Method == http.MethodPost && r.URL.Path == "/v2/bot/message/push":
		var body struct {
			To       string `json:"to"`
			Messages []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		key := r.Header.Get("X-Line-Retry-Key")
		f.mu.Lock()
		dup := key != "" && f.keys[key]
		if key != "" {
			f.keys[key] = true
		}
		if !dup {
			for _, m := range body.Messages {
				f.pushes = append(f.pushes, LinePush{To: body.To, Text: m.Text, RetryKey: key})
			}
		}
		f.mu.Unlock()
		if dup {
			rw.WriteHeader(http.StatusConflict)
			_, _ = rw.Write([]byte(`{"message":"The retry key is already accepted"}`))
			return
		}
		_, _ = rw.Write([]byte(`{"sentMessages":[]}`))
	default:
		rw.WriteHeader(http.StatusNotFound)
		_, _ = rw.Write([]byte(`{"message":"Not found"}`))
	}
}

// DeliverDM POSTs a signed 1:1 text event from userID to webhookURL and
// returns the HTTP status the adapter answered.
func (f *Line) DeliverDM(webhookURL, userID, text string) int {
	return f.deliver(webhookURL, map[string]any{"type": "user", "userId": userID}, text, false, true)
}

// DeliverGroup POSTs a signed group text event; mentionBot adds a mention of
// the bot (isSelf), as LINE does when the bot is @-mentioned.
func (f *Line) DeliverGroup(webhookURL, groupID, userID, text string, mentionBot bool) int {
	return f.deliver(webhookURL, map[string]any{"type": "group", "groupId": groupID, "userId": userID}, text, mentionBot, true)
}

// DeliverForged POSTs a 1:1 event with a wrong signature.
func (f *Line) DeliverForged(webhookURL, userID, text string) int {
	return f.deliver(webhookURL, map[string]any{"type": "user", "userId": userID}, text, false, false)
}

func (f *Line) deliver(webhookURL string, source map[string]any, text string, mentionBot, sign bool) int {
	f.mu.Lock()
	id := f.nextID
	f.nextID++
	f.mu.Unlock()
	m := map[string]any{"type": "text", "id": fmt.Sprint(id), "text": text}
	if mentionBot {
		m["mention"] = map[string]any{"mentionees": []any{map[string]any{"index": 0, "length": 5, "type": "user", "userId": f.BotID, "isSelf": true}}}
	}
	body, _ := json.Marshal(map[string]any{
		"destination": f.BotID,
		"events": []any{map[string]any{
			"type": "message", "mode": "active", "timestamp": time.Now().UnixMilli(),
			"webhookEventId": fmt.Sprintf("01FAKE%020d", id),
			"source":         source, "replyToken": "fake-reply-token", "message": m,
		}},
	})
	mac := hmac.New(sha256.New, []byte(f.Secret))
	mac.Write(body)
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if !sign {
		sig = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 32)))
	}
	req, _ := http.NewRequest(http.MethodPost, webhookURL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Line-Signature", sig)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	resp.Body.Close()
	return resp.StatusCode
}
