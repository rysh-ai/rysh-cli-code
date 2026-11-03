// SPDX-License-Identifier: Apache-2.0

package fakeplatform

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
)

// MattermostPost is one POST /api/v4/posts the fake received.
type MattermostPost struct {
	ChannelID string `json:"channel_id"`
	Message   string `json:"message"`
	RootID    string `json:"root_id,omitempty"`
}

// Mattermost is a fake Mattermost server: REST v4 (users/me, posts) and the
// /api/v4/websocket event stream with its authentication_challenge, over
// which the test pushes "posted" events with PushDM / PushChannel.
type Mattermost struct {
	Token       string
	BotID       string
	BotUsername string

	srv      *httptest.Server
	upgrader websocket.Upgrader

	mu      sync.Mutex
	posts   []MattermostPost
	events  chan []byte
	nextID  int
	authOK  int
	authBad int
}

// NewMattermost starts a fake Mattermost on 127.0.0.1 that accepts only token.
func NewMattermost(token string) *Mattermost {
	f := &Mattermost{
		Token:       token,
		BotID:       "botuserid0000000000000000a",
		BotUsername: "ryshbot",
		events:      make(chan []byte, 64),
		nextID:      1,
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

// URL is the server root to put in ChannelConfig.ServerURL.
func (f *Mattermost) URL() string { return f.srv.URL }

// Close stops the server.
func (f *Mattermost) Close() { f.srv.Close() }

// DMChannel is the direct channel id the fake uses between the bot and user.
func (f *Mattermost) DMChannel(userID string) string { return "dm_" + userID }

// Posts returns a copy of every post created so far.
func (f *Mattermost) Posts() []MattermostPost {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]MattermostPost(nil), f.posts...)
}

// Authentications reports accepted and refused websocket challenges.
func (f *Mattermost) Authentications() (ok, refused int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.authOK, f.authBad
}

// PushDM emits a "posted" event in the bot's direct channel with userID.
func (f *Mattermost) PushDM(userID, username, message string) {
	f.emit(f.DMChannel(userID), "D", userID, username, message, "")
}

// PushChannel emits a "posted" event in an open channel.
func (f *Mattermost) PushChannel(channelID, userID, username, message string) {
	f.emit(channelID, "O", userID, username, message, "")
}

func (f *Mattermost) emit(channelID, channelType, userID, username, message, rootID string) {
	f.mu.Lock()
	id := fmt.Sprintf("post%022d", f.nextID)
	f.nextID++
	f.mu.Unlock()
	post, _ := json.Marshal(map[string]any{
		"id": id, "user_id": userID, "channel_id": channelID, "message": message, "root_id": rootID,
	})
	raw, _ := json.Marshal(map[string]any{
		"event": "posted",
		"data": map[string]any{
			"channel_type": channelType,
			"sender_name":  "@" + username,
			"post":         string(post),
		},
		"broadcast": map[string]any{"channel_id": channelID},
	})
	f.events <- raw
}

func (f *Mattermost) serve(rw http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/v4/websocket" {
		f.websocket(rw, r)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+f.Token {
		rw.WriteHeader(http.StatusUnauthorized)
		_, _ = rw.Write([]byte(`{"id":"api.context.session_expired.app_error","status_code":401}`))
		return
	}
	rw.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v4/users/me":
		fmt.Fprintf(rw, `{"id":%q,"username":%q,"is_bot":true}`, f.BotID, f.BotUsername)
	case r.Method == http.MethodPost && r.URL.Path == "/api/v4/posts":
		var p MattermostPost
		_ = json.NewDecoder(r.Body).Decode(&p)
		f.mu.Lock()
		f.posts = append(f.posts, p)
		id := fmt.Sprintf("post%022d", f.nextID)
		f.nextID++
		f.mu.Unlock()
		rw.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(rw).Encode(map[string]any{"id": id, "channel_id": p.ChannelID, "message": p.Message, "user_id": f.BotID})
	default:
		rw.WriteHeader(http.StatusNotFound)
		_, _ = rw.Write([]byte(`{"status_code":404}`))
	}
}

// websocket accepts one event-stream connection: the client must send an
// authentication_challenge carrying the token before any event is pushed.
func (f *Mattermost) websocket(rw http.ResponseWriter, r *http.Request) {
	c, err := f.upgrader.Upgrade(rw, r, nil)
	if err != nil {
		return
	}
	defer c.Close()
	var auth struct {
		Seq    int    `json:"seq"`
		Action string `json:"action"`
		Data   struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := c.ReadJSON(&auth); err != nil {
		return
	}
	if auth.Action != "authentication_challenge" || auth.Data.Token != f.Token {
		f.mu.Lock()
		f.authBad++
		f.mu.Unlock()
		_ = c.WriteJSON(map[string]any{"status": "FAIL", "seq_reply": auth.Seq})
		return
	}
	f.mu.Lock()
	f.authOK++
	f.mu.Unlock()
	var wmu sync.Mutex
	write := func(b []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		return c.WriteMessage(websocket.TextMessage, b)
	}
	if write([]byte(fmt.Sprintf(`{"status":"OK","seq_reply":%d}`, auth.Seq))) != nil {
		return
	}
	_ = write([]byte(`{"event":"hello","data":{"server_version":"fake"},"broadcast":{}}`))
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}()
	for {
		select {
		case <-done:
			return
		case <-r.Context().Done():
			return
		case ev := <-f.events:
			if strings.TrimSpace(string(ev)) == "" {
				continue
			}
			if write(ev) != nil {
				return
			}
		}
	}
}
