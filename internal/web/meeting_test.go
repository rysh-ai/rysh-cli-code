// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// E-87: the app's Meeting panel has no pane, so meeting_token carries a room
// id. The handler passes it to the workspace and echoes it in the reply, which
// is how the app routes the answer back to the panel that asked.
func TestMeetingTokenPassesTheRoomThroughAndEchoesIt(t *testing.T) {
	nc := startInProcessNATS(t)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)
	s := NewServer(23236, "meeting-test-session", pub, nc, pub.Codecs())
	s.hub = newHub()

	got := make(chan msg.MsgMeetingToken, 1)
	sub, err := nc.Subscribe(msg.T("ws", "inbox"), func(m *nats.Msg) {
		var env msg.NATSEnvelope
		if err := json.Unmarshal(m.Data, &env); err != nil {
			t.Error(err)
			return
		}
		var req msg.MsgMeetingToken
		if err := json.Unmarshal(env.Payload, &req); err != nil {
			t.Error(err)
			return
		}
		got <- req
		re := msg.RequestEnvelope{ReplyTo: env.ReplyTo, NC: nc, Codecs: codecs}
		_ = re.Reply(&msg.MsgMeetingTokenReply{Room: req.Room, URL: "wss://lk.example", Token: "a.b.c", Identity: "u1"})
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	_ = nc.Flush()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.hub.run(ctx)
	c := &wsClient{hub: s.hub, send: make(chan []byte, 4)}
	s.hub.register <- c
	s.handleMeetingToken(c, json.RawMessage(`{"room":"room9"}`))

	select {
	case req := <-got:
		if req.Room != "room9" || req.PaneID != "" {
			t.Fatalf("workspace got %+v", req)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no request reached the workspace")
	}
	select {
	case raw := <-c.send:
		var frame struct {
			Type string            `json:"type"`
			Data map[string]string `json:"data"`
		}
		if err := json.Unmarshal(raw, &frame); err != nil {
			t.Fatal(err)
		}
		if frame.Type != "meeting_token" || frame.Data["room"] != "room9" || frame.Data["token"] != "a.b.c" || frame.Data["url"] != "wss://lk.example" {
			t.Fatalf("frame %s", raw)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no meeting_token frame")
	}
}
