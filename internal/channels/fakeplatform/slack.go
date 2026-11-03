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

// SlackSend is one chat.postMessage the fake received. Channel is where Slack
// would deliver it: a user id ("U…") is delivered into the app's DM with that
// user, as Slack does, so it is recorded as that user's DM channel; RawChannel
// is the value the adapter sent.
type SlackSend struct {
	Channel    string
	RawChannel string
	Text       string
	ThreadTS   string
}

// Slack is a fake Slack for the Socket Mode adapter (channels, E33; qa coverage
// gap): Web API methods under /api/ (auth.test, apps.connections.open,
// users.info, conversations.info, chat.postMessage) and the Socket Mode
// WebSocket, which sends "hello", the events_api envelopes the test pushes
// with PushDM / PushChannel, and WebSocket pings (slack-go drops a connection
// that Slack stops pinging).
//
// It accepts only BotToken on Web API methods and only AppToken on
// apps.connections.open, as Slack does. It never talks to slack.com.
type Slack struct {
	BotToken string
	AppToken string
	BotID    string // the bot's user id, returned by auth.test

	srv      *httptest.Server
	upgrader websocket.Upgrader

	mu     sync.Mutex
	sends  []SlackSend
	events chan []byte
	acks   map[string]bool
	nextTS int64
	nextEv int64
	opens  int
	// lastTop is each user's latest top-level DM ts, the thread a
	// PushDMThreadReply answers in.
	lastTop map[string]string
}

// NewSlack starts a fake Slack on 127.0.0.1.
func NewSlack(botToken, appToken string) *Slack {
	f := &Slack{
		BotToken: botToken,
		AppToken: appToken,
		BotID:    "UBOTFAKE01",
		events:   make(chan []byte, 64),
		acks:     map[string]bool{},
		lastTop:  map[string]string{},
		nextTS:   1700000000,
	}
	// slack-go dials with "Origin: https://api.slack.com"; accept exactly that,
	// as gorilla's default check would refuse it (host mismatch).
	f.upgrader.CheckOrigin = func(r *http.Request) bool {
		return r.Header.Get("Origin") == "https://api.slack.com"
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

// URL is the root to put in ChannelConfig.APIBase.
func (f *Slack) URL() string { return f.srv.URL }

// Close stops the server.
func (f *Slack) Close() { f.srv.Close() }

// DMChannel is the 1:1 DM channel id the fake uses for a user.
func (f *Slack) DMChannel(userID string) string { return "D" + userID }

// Sends returns a copy of every chat.postMessage so far.
func (f *Slack) Sends() []SlackSend {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]SlackSend(nil), f.sends...)
}

// Opens is the number of apps.connections.open calls (Socket Mode connects).
func (f *Slack) Opens() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opens
}

// Acked reports whether the adapter acknowledged the envelope.
func (f *Slack) Acked(envelopeID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.acks[envelopeID]
}

// PushDM delivers a 1:1 message from userID ("message" event, channel_type
// "im"). It returns the envelope id.
func (f *Slack) PushDM(userID, text string) string {
	return f.push(f.DMChannel(userID), "im", userID, text, "")
}

// PushChannel delivers a message in a public channel (channel_type "channel").
func (f *Slack) PushChannel(channelID, userID, text string) string {
	return f.push(channelID, "channel", userID, text, "")
}

// PushDMThreadReply delivers a reply from userID inside the thread of their
// latest top-level DM (where the bot's threaded answers sit). With no earlier
// DM it is a top-level DM.
func (f *Slack) PushDMThreadReply(userID, text string) string {
	f.mu.Lock()
	thread := f.lastTop[userID]
	f.mu.Unlock()
	return f.push(f.DMChannel(userID), "im", userID, text, thread)
}

func (f *Slack) push(channel, channelType, userID, text, threadTS string) string {
	f.mu.Lock()
	f.nextTS++
	f.nextEv++
	ts := fmt.Sprintf("%d.000100", f.nextTS)
	if channelType == "im" && threadTS == "" {
		f.lastTop[userID] = ts
	}
	env := fmt.Sprintf("env-%d", f.nextEv)
	evID := fmt.Sprintf("Ev%06d", f.nextEv)
	f.mu.Unlock()
	event := map[string]any{
		"type":         "message",
		"channel":      channel,
		"channel_type": channelType,
		"user":         userID,
		"text":         text,
		"ts":           ts,
		"event_ts":     ts,
	}
	if threadTS != "" {
		event["thread_ts"] = threadTS
	}
	envelope := map[string]any{
		"envelope_id":              env,
		"type":                     "events_api",
		"accepts_response_payload": false,
		"retry_attempt":            0,
		"payload": map[string]any{
			"token":      "",
			"team_id":    "TFAKE01",
			"api_app_id": "AFAKE01",
			"type":       "event_callback",
			"event_id":   evID,
			"event_time": time.Now().Unix(),
			"event":      event,
		},
	}
	raw, _ := json.Marshal(envelope)
	f.events <- raw
	return env
}

// bearer returns the request's bearer token, or the form "token" field.
func bearer(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	_ = r.ParseForm()
	return r.Form.Get("token")
}

func writeJSON(rw http.ResponseWriter, v any) {
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(v)
}

func (f *Slack) serve(rw http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/socket" {
		f.socket(rw, r)
		return
	}
	method := strings.TrimPrefix(r.URL.Path, "/api/")
	tok := bearer(r)
	if method == "apps.connections.open" {
		if tok != f.AppToken {
			writeJSON(rw, map[string]any{"ok": false, "error": "invalid_auth"})
			return
		}
		f.mu.Lock()
		f.opens++
		f.mu.Unlock()
		writeJSON(rw, map[string]any{"ok": true, "url": "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/socket"})
		return
	}
	if tok != f.BotToken {
		writeJSON(rw, map[string]any{"ok": false, "error": "invalid_auth"})
		return
	}
	switch method {
	case "auth.test":
		writeJSON(rw, map[string]any{"ok": true, "url": "https://fake.slack.invalid/", "team": "fake",
			"user": "rysh", "team_id": "TFAKE01", "user_id": f.BotID, "bot_id": "BFAKE01"})
	case "users.info":
		id := r.Form.Get("user")
		writeJSON(rw, map[string]any{"ok": true, "user": map[string]any{
			"id": id, "name": "u" + id, "real_name": "User " + id,
			"profile": map[string]any{"display_name": "u" + id, "real_name": "User " + id},
		}})
	case "conversations.info":
		id := r.Form.Get("channel")
		writeJSON(rw, map[string]any{"ok": true, "channel": map[string]any{"id": id, "name": strings.ToLower(id)}})
	case "chat.postMessage":
		raw := r.Form.Get("channel")
		delivered := raw
		if strings.HasPrefix(raw, "U") {
			delivered = f.DMChannel(raw)
		}
		s := SlackSend{Channel: delivered, RawChannel: raw, Text: r.Form.Get("text"), ThreadTS: r.Form.Get("thread_ts")}
		f.mu.Lock()
		f.sends = append(f.sends, s)
		f.nextTS++
		ts := fmt.Sprintf("%d.000200", f.nextTS)
		f.mu.Unlock()
		writeJSON(rw, map[string]any{"ok": true, "channel": s.Channel, "ts": ts})
	default:
		writeJSON(rw, map[string]any{"ok": false, "error": "unknown_method"})
	}
}

// socket serves one Socket Mode connection: hello, then pushed envelopes and
// periodic pings; acks are recorded by envelope id.
func (f *Slack) socket(rw http.ResponseWriter, r *http.Request) {
	conn, err := f.upgrader.Upgrade(rw, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	var wmu sync.Mutex
	write := func(mt int, data []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		return conn.WriteMessage(mt, data)
	}
	if err := write(websocket.TextMessage, []byte(`{"type":"hello","num_connections":1,"connection_info":{"app_id":"AFAKE01"}}`)); err != nil {
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var ack struct {
				EnvelopeID string `json:"envelope_id"`
			}
			if json.Unmarshal(data, &ack) == nil && ack.EnvelopeID != "" {
				f.mu.Lock()
				f.acks[ack.EnvelopeID] = true
				f.mu.Unlock()
			}
		}
	}()
	ping := time.NewTicker(2 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-done:
			return
		case raw := <-f.events:
			if err := write(websocket.TextMessage, raw); err != nil {
				return
			}
		case <-ping.C:
			if err := write(websocket.PingMessage, []byte("ping")); err != nil {
				return
			}
		}
	}
}
