// SPDX-License-Identifier: Apache-2.0

// Package fakeplatform holds local fake chat-platform servers for tests (gap05,
// G13). Each fake binds 127.0.0.1 only, speaks just enough of one platform's
// wire API for a real channel adapter to run against it unchanged (pointed at
// it via ChannelConfig.APIBase / ServerURL), and records every outbound call so
// a test can assert exactly what would have reached the platform.
//
// A test that passes against a fake proves rysh's side of the wire only: it
// encodes our reading of the platform's API. It is never a live proof.
//
// Only test code imports this package; it is not linked into the binary.
package fakeplatform

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// TelegramSend is one sendMessage call the fake received.
type TelegramSend struct {
	ChatID          string `json:"chat_id"`
	Text            string `json:"text"`
	MessageThreadID int64  `json:"message_thread_id,omitempty"`
	ParseMode       string `json:"parse_mode,omitempty"`
}

// Telegram is a fake Bot API server: getMe, getUpdates (long-poll over a queue
// the test fills with PushPrivate/PushGroup) and sendMessage.
type Telegram struct {
	Token       string
	BotID       int64
	BotUsername string

	srv *httptest.Server

	mu       sync.Mutex
	nextID   int64
	nextMsg  int64
	queue    []json.RawMessage
	sends    []TelegramSend
	requests int
	wake     chan struct{}
}

// NewTelegram starts a fake Bot API on 127.0.0.1 that accepts only token.
func NewTelegram(token string) *Telegram {
	f := &Telegram{
		Token:       token,
		BotID:       7000001,
		BotUsername: "rysh_fake_bot",
		nextID:      1,
		nextMsg:     1,
		wake:        make(chan struct{}, 1),
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

// URL is the API root to put in ChannelConfig.APIBase.
func (f *Telegram) URL() string { return f.srv.URL }

// Close stops the server.
func (f *Telegram) Close() { f.srv.Close() }

// Sends returns a copy of every sendMessage received so far.
func (f *Telegram) Sends() []TelegramSend {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]TelegramSend(nil), f.sends...)
}

// Requests is the number of API requests received (any method).
func (f *Telegram) Requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

// PushPrivate queues a 1:1 message from user fromID to the bot.
func (f *Telegram) PushPrivate(fromID int64, username, text string) {
	f.push(fromID, username, fromID, "private", text)
}

// PushGroup queues a message from fromID in group chatID.
func (f *Telegram) PushGroup(chatID, fromID int64, username, text string) {
	f.push(fromID, username, chatID, "supergroup", text)
}

func (f *Telegram) push(fromID int64, username string, chatID int64, chatType, text string) {
	f.mu.Lock()
	u := map[string]any{
		"update_id": f.nextID,
		"message": map[string]any{
			"message_id": f.nextMsg,
			"from":       map[string]any{"id": fromID, "is_bot": false, "first_name": username, "username": username},
			"chat":       map[string]any{"id": chatID, "type": chatType},
			"date":       time.Now().Unix(),
			"text":       text,
		},
	}
	f.nextID++
	f.nextMsg++
	raw, _ := json.Marshal(u)
	f.queue = append(f.queue, raw)
	f.mu.Unlock()
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

func (f *Telegram) serve(rw http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests++
	f.mu.Unlock()
	prefix := "/bot" + f.Token + "/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		rw.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(rw, `{"ok":false,"error_code":401,"description":"Unauthorized"}`)
		return
	}
	switch strings.TrimPrefix(r.URL.Path, prefix) {
	case "getMe":
		fmt.Fprintf(rw, `{"ok":true,"result":{"id":%d,"is_bot":true,"first_name":"rysh","username":%q}}`, f.BotID, f.BotUsername)
	case "getUpdates":
		var req struct {
			Offset int64 `json:"offset"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		deadline := time.After(200 * time.Millisecond)
		for {
			if batch := f.take(req.Offset); len(batch) > 0 {
				fmt.Fprintf(rw, `{"ok":true,"result":[%s]}`, strings.Join(batch, ","))
				return
			}
			select {
			case <-f.wake:
			case <-deadline:
				_, _ = io.WriteString(rw, `{"ok":true,"result":[]}`)
				return
			case <-r.Context().Done():
				return
			}
		}
	case "sendMessage":
		var p TelegramSend
		_ = json.NewDecoder(r.Body).Decode(&p)
		f.mu.Lock()
		f.sends = append(f.sends, p)
		id := f.nextMsg
		f.nextMsg++
		f.mu.Unlock()
		fmt.Fprintf(rw, `{"ok":true,"result":{"message_id":%d}}`, id)
	case "deleteWebhook", "setWebhook":
		_, _ = io.WriteString(rw, `{"ok":true,"result":true}`)
	default:
		rw.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(rw, `{"ok":false,"error_code":404,"description":"Not Found"}`)
	}
}

// take confirms every update below offset (Bot API semantics) and returns the
// rest as raw JSON.
func (f *Telegram) take(offset int64) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var keep []json.RawMessage
	var out []string
	for _, raw := range f.queue {
		var u struct {
			UpdateID int64 `json:"update_id"`
		}
		_ = json.Unmarshal(raw, &u)
		if u.UpdateID < offset {
			continue
		}
		keep = append(keep, raw)
		out = append(out, string(raw))
	}
	f.queue = keep
	return out
}
