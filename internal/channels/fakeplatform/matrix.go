// SPDX-License-Identifier: Apache-2.0

package fakeplatform

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"time"
)

// MatrixSend is one m.room.message the fake received.
type MatrixSend struct {
	RoomID  string
	TxnID   string
	MsgType string
	Body    string
}

// Matrix is a fake Matrix homeserver (client-server API v3): whoami, /sync
// long-poll over a queue the test fills, join, and room send. Room summaries
// carry m.joined_member_count so a 1:1 room (2 members) reads as a DM.
type Matrix struct {
	Token  string
	UserID string

	srv *httptest.Server

	mu      sync.Mutex
	batch   int
	pending []matrixItem
	sends   []MatrixSend
	joins   []string
	txns    map[string]bool
	wake    chan struct{}
	nextEvt int
}

type matrixItem struct {
	room    string
	members int
	invite  bool
	event   map[string]any
}

// NewMatrix starts a fake homeserver on 127.0.0.1 that accepts only token.
func NewMatrix(token string) *Matrix {
	f := &Matrix{Token: token, UserID: "@rysh:fake.local", txns: map[string]bool{}, wake: make(chan struct{}, 1)}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

// URL is the homeserver root to put in ChannelConfig.ServerURL.
func (f *Matrix) URL() string { return f.srv.URL }

// Close stops the server.
func (f *Matrix) Close() { f.srv.Close() }

// DMRoom is the 1:1 room the fake uses between the bot and user.
func (f *Matrix) DMRoom(user string) string {
	local := strings.TrimPrefix(strings.SplitN(user, ":", 2)[0], "@")
	return "!dm-" + local + ":fake.local"
}

// Sends returns a copy of every message the bot sent.
func (f *Matrix) Sends() []MatrixSend {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]MatrixSend(nil), f.sends...)
}

// Joins returns the rooms the bot joined.
func (f *Matrix) Joins() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.joins...)
}

// PushDM queues a text message from user in their 1:1 room with the bot.
func (f *Matrix) PushDM(user, body string) { f.PushRoom(f.DMRoom(user), 2, user, body) }

// PushRoom queues a text message in room (members = joined member count).
func (f *Matrix) PushRoom(room string, members int, user, body string) {
	f.queue(matrixItem{room: room, members: members, event: map[string]any{
		"type": "m.room.message", "sender": user,
		"content": map[string]any{"msgtype": "m.text", "body": body},
	}})
}

// PushEncrypted queues an end-to-end encrypted event (which v1 cannot read).
func (f *Matrix) PushEncrypted(room string, user string) {
	f.queue(matrixItem{room: room, members: 2, event: map[string]any{
		"type": "m.room.encrypted", "sender": user,
		"content": map[string]any{"algorithm": "m.megolm.v1.aes-sha2", "ciphertext": "opaque"},
	}})
}

// Invite queues an invite of the bot to room by user.
func (f *Matrix) Invite(room, user string) {
	f.queue(matrixItem{room: room, invite: true, event: map[string]any{
		"type": "m.room.member", "sender": user, "state_key": f.UserID,
		"content": map[string]any{"membership": "invite", "is_direct": true},
	}})
}

func (f *Matrix) queue(it matrixItem) {
	f.mu.Lock()
	f.nextEvt++
	it.event["event_id"] = fmt.Sprintf("$evt%d", f.nextEvt)
	it.event["origin_server_ts"] = time.Now().UnixMilli()
	f.pending = append(f.pending, it)
	f.mu.Unlock()
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

func (f *Matrix) serve(rw http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+f.Token {
		rw.WriteHeader(http.StatusUnauthorized)
		_, _ = rw.Write([]byte(`{"errcode":"M_UNKNOWN_TOKEN","error":"Invalid access token"}`))
		return
	}
	rw.Header().Set("Content-Type", "application/json")
	const pre = "/_matrix/client/v3/"
	path := strings.TrimPrefix(r.URL.EscapedPath(), pre)
	switch {
	case r.Method == http.MethodGet && path == "account/whoami":
		fmt.Fprintf(rw, `{"user_id":%q}`, f.UserID)
	case r.Method == http.MethodGet && path == "sync":
		f.sync(rw, r)
	case r.Method == http.MethodPost && strings.HasPrefix(path, "join/"):
		room, _ := url.PathUnescape(strings.TrimPrefix(path, "join/"))
		f.mu.Lock()
		f.joins = append(f.joins, room)
		f.mu.Unlock()
		fmt.Fprintf(rw, `{"room_id":%q}`, room)
	case r.Method == http.MethodPut && strings.HasPrefix(path, "rooms/") && strings.Contains(path, "/send/m.room.message/"):
		rest := strings.TrimPrefix(path, "rooms/")
		roomEsc, txnEsc, _ := strings.Cut(rest, "/send/m.room.message/")
		room, _ := url.PathUnescape(roomEsc)
		txn, _ := url.PathUnescape(txnEsc)
		var c struct {
			MsgType string `json:"msgtype"`
			Body    string `json:"body"`
		}
		_ = json.NewDecoder(r.Body).Decode(&c)
		f.mu.Lock()
		if !f.txns[room+"/"+txn] { // idempotent per transaction id, as the spec requires
			f.txns[room+"/"+txn] = true
			f.sends = append(f.sends, MatrixSend{RoomID: room, TxnID: txn, MsgType: c.MsgType, Body: c.Body})
		}
		f.nextEvt++
		id := f.nextEvt
		f.mu.Unlock()
		fmt.Fprintf(rw, `{"event_id":"$sent%d"}`, id)
	default:
		rw.WriteHeader(http.StatusNotFound)
		_, _ = rw.Write([]byte(`{"errcode":"M_UNRECOGNIZED","error":"Unrecognized request"}`))
	}
}

// sync answers /sync: everything queued since the last batch, else an empty
// response after a short wait (a shortened long-poll).
func (f *Matrix) sync(rw http.ResponseWriter, r *http.Request) {
	deadline := time.After(200 * time.Millisecond)
	for {
		f.mu.Lock()
		items := f.pending
		f.pending = nil
		f.batch++
		next := fmt.Sprintf("s%d", f.batch)
		f.mu.Unlock()
		if len(items) > 0 {
			_ = json.NewEncoder(rw).Encode(f.render(next, items))
			return
		}
		select {
		case <-f.wake:
			continue
		case <-deadline:
			fmt.Fprintf(rw, `{"next_batch":%q,"rooms":{}}`, next)
			return
		case <-r.Context().Done():
			return
		}
	}
}

func (f *Matrix) render(next string, items []matrixItem) map[string]any {
	join := map[string]any{}
	invite := map[string]any{}
	for _, it := range items {
		if it.invite {
			invite[it.room] = map[string]any{"invite_state": map[string]any{"events": []any{it.event}}}
			continue
		}
		room, ok := join[it.room].(map[string]any)
		if !ok {
			room = map[string]any{
				"summary":  map[string]any{"m.joined_member_count": it.members},
				"timeline": map[string]any{"events": []any{}},
			}
			join[it.room] = room
		}
		tl := room["timeline"].(map[string]any)
		tl["events"] = append(tl["events"].([]any), it.event)
	}
	return map[string]any{"next_batch": next, "rooms": map[string]any{"join": join, "invite": invite}}
}
