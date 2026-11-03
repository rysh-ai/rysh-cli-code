// SPDX-License-Identifier: Apache-2.0

package fakeplatform

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// DiscordSend is one POST /channels/{id}/messages the fake received.
type DiscordSend struct {
	ChannelID string
	Content   string
}

// Discord is a fake Discord API: REST under /api/v9 and /api/v10 (gateway discovery,
// users/@me, channel messages) and a Gateway WebSocket speaking HELLO →
// IDENTIFY → READY, heartbeats, and MESSAGE_CREATE dispatches the test pushes
// with PushDM / PushGuild.
type Discord struct {
	Token string
	BotID string

	srv      *httptest.Server
	upgrader websocket.Upgrader

	mu       sync.Mutex
	sends    []DiscordSend
	events   chan []byte
	seq      int64
	nextID   int64
	identify int
}

// NewDiscord starts a fake Discord on 127.0.0.1 that accepts only token.
func NewDiscord(token string) *Discord {
	f := &Discord{
		Token:  token,
		BotID:  "700000000000000001",
		events: make(chan []byte, 64),
		nextID: 900000000000000001,
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

// URL is the API root to put in ChannelConfig.APIBase.
func (f *Discord) URL() string { return f.srv.URL }

// Close stops the server.
func (f *Discord) Close() { f.srv.Close() }

// DMChannel is the DM channel id the fake uses for a user's 1:1 chat.
func (f *Discord) DMChannel(userID string) string { return "9" + userID }

// Sends returns a copy of every message posted so far.
func (f *Discord) Sends() []DiscordSend {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]DiscordSend(nil), f.sends...)
}

// Identifies is the number of IDENTIFY packets received.
func (f *Discord) Identifies() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.identify
}

// PushDM dispatches a 1:1 message (no guild) from userID.
func (f *Discord) PushDM(userID, username, content string) {
	f.dispatch("", f.DMChannel(userID), userID, username, content)
}

// PushGuild dispatches a message in a guild channel.
func (f *Discord) PushGuild(guildID, channelID, userID, username, content string) {
	f.dispatch(guildID, channelID, userID, username, content)
}

func (f *Discord) dispatch(guildID, channelID, userID, username, content string) {
	f.mu.Lock()
	id := f.nextID
	f.nextID++
	f.mu.Unlock()
	d := map[string]any{
		"id":         fmt.Sprint(id),
		"channel_id": channelID,
		"content":    content,
		"author":     map[string]any{"id": userID, "username": username},
	}
	if guildID != "" {
		d["guild_id"] = guildID
	}
	raw, _ := json.Marshal(map[string]any{"op": 0, "t": "MESSAGE_CREATE", "d": d})
	f.events <- raw
}

func (f *Discord) authorized(r *http.Request) bool {
	return r.Header.Get("Authorization") == "Bot "+f.Token
}

func (f *Discord) serve(rw http.ResponseWriter, r *http.Request) {
	// discordgo v0.29 speaks API v9; rysh's own Validate calls use v10. The
	// fake serves both under one route table.
	path := r.URL.Path
	for _, v := range []string{"/api/v9/", "/api/v10/"} {
		if strings.HasPrefix(path, v) {
			path = "/api/v10/" + strings.TrimPrefix(path, v)
			break
		}
	}
	switch {
	case path == "/gateway" || path == "/gateway/":
		f.gateway(rw, r)
		return
	case path == "/api/v10/gateway" || path == "/api/v10/gateway/bot":
		rw.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(rw, `{"url":%q,"shards":1}`, "ws://"+r.Host+"/gateway")
		return
	}
	if !f.authorized(r) {
		rw.WriteHeader(http.StatusUnauthorized)
		_, _ = rw.Write([]byte(`{"message":"401: Unauthorized","code":0}`))
		return
	}
	rw.Header().Set("Content-Type", "application/json")
	switch {
	case path == "/api/v10/users/@me":
		fmt.Fprintf(rw, `{"id":%q,"username":"rysh_fake_bot","bot":true}`, f.BotID)
	case path == "/api/v10/oauth2/applications/@me":
		fmt.Fprintf(rw, `{"id":%q,"name":"rysh"}`, f.BotID)
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/api/v10/channels/") && strings.HasSuffix(path, "/messages"):
		channelID := strings.TrimSuffix(strings.TrimPrefix(path, "/api/v10/channels/"), "/messages")
		var body struct {
			Content string `json:"content"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.sends = append(f.sends, DiscordSend{ChannelID: channelID, Content: body.Content})
		id := f.nextID
		f.nextID++
		f.mu.Unlock()
		fmt.Fprintf(rw, `{"id":"%d","channel_id":%q,"content":%q,"author":{"id":%q,"username":"rysh_fake_bot","bot":true}}`,
			id, channelID, body.Content, f.BotID)
	default:
		rw.WriteHeader(http.StatusNotFound)
		_, _ = rw.Write([]byte(`{"message":"404: Not Found","code":0}`))
	}
}

// gateway runs one Gateway session: HELLO, wait for IDENTIFY with the right
// token, READY, then heartbeat ACKs and pushed dispatches until close.
func (f *Discord) gateway(rw http.ResponseWriter, r *http.Request) {
	c, err := f.upgrader.Upgrade(rw, r, nil)
	if err != nil {
		return
	}
	defer c.Close()
	var wmu sync.Mutex
	write := func(v []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		return c.WriteMessage(websocket.TextMessage, v)
	}
	if write([]byte(`{"op":10,"d":{"heartbeat_interval":45000}}`)) != nil {
		return
	}
	var pkt struct {
		Op int `json:"op"`
		D  struct {
			Token string `json:"token"`
		} `json:"d"`
	}
	if err := c.ReadJSON(&pkt); err != nil || pkt.Op != 2 || pkt.D.Token != "Bot "+f.Token {
		_ = c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(4004, "Authentication failed."), time.Now().Add(time.Second))
		return
	}
	f.mu.Lock()
	f.identify++
	f.mu.Unlock()
	ready := fmt.Sprintf(`{"op":0,"s":%d,"t":"READY","d":{"v":10,"user":{"id":%q,"username":"rysh_fake_bot","bot":true},"session_id":"fake-session","resume_gateway_url":"ws://%s/gateway","guilds":[],"private_channels":[]}}`,
		f.nextSeq(), f.BotID, r.Host)
	if write([]byte(ready)) != nil {
		return
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			var in struct {
				Op int `json:"op"`
			}
			if err := c.ReadJSON(&in); err != nil {
				return
			}
			if in.Op == 1 {
				_ = write([]byte(`{"op":11}`))
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
			var m map[string]any
			_ = json.Unmarshal(ev, &m)
			m["s"] = f.nextSeq()
			raw, _ := json.Marshal(m)
			if write(raw) != nil {
				return
			}
		}
	}
}

func (f *Discord) nextSeq() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	return f.seq
}
