// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/meeting"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// E17 (k1), MTG-6: the desktop app's meeting panel sends the design-040
// lifecycle verbs through `meeting_command`. The web server forwards each as
// the SAME MsgMeetingCommand the CLI and TUI send, addressed by the meeting's
// registry id, and answers the originating client with a `meeting_reply`
// frame that echoes the id and verb so the app routes it to the right panel.
// Only the seven lifecycle verbs pass; list/status/leave/join stay with
// ##meeting, and a missing or malformed id is refused before the daemon sees
// it (the daemon's "" default means "the only meeting").

type meetingFrame struct {
	Type string `json:"type"`
	Data struct {
		ID    string        `json:"id"`
		Verb  string        `json:"verb"`
		Reply meeting.Reply `json:"reply"`
		Error string        `json:"error"`
	} `json:"data"`
}

func meetingCommandServer(t *testing.T) (*Server, *wsClient, <-chan meeting.Command) {
	t.Helper()
	nc := startInProcessNATS(t)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)
	s := NewServer(23237, "meeting-cmd-test", pub, nc, pub.Codecs())
	s.hub = newHub()

	got := make(chan meeting.Command, 8)
	sub, err := nc.Subscribe(msg.T("ws", "inbox"), func(m *nats.Msg) {
		var env msg.NATSEnvelope
		if json.Unmarshal(m.Data, &env) != nil {
			return
		}
		d, err := codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			return
		}
		mc, ok := d.(*msg.MsgMeetingCommand)
		if !ok {
			return
		}
		got <- mc.Command
		re := msg.RequestEnvelope{ReplyTo: env.ReplyTo, NC: nc, Codecs: codecs}
		_ = re.Reply(&msg.MsgMeetingReply{Reply: meeting.Reply{ID: mc.Command.ID, State: "ok-" + mc.Command.Action, TurnID: mc.Command.TurnID}})
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	_ = nc.Flush()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go s.hub.run(ctx)
	c := &wsClient{hub: s.hub, send: make(chan []byte, 8)}
	s.hub.register <- c
	return s, c, got
}

// nextCommand returns the next command the fake daemon received.
func nextCommand(t *testing.T, got <-chan meeting.Command) meeting.Command {
	t.Helper()
	select {
	case c := <-got:
		return c
	case <-time.After(3 * time.Second):
		t.Fatal("no MsgMeetingCommand reached the daemon")
	}
	return meeting.Command{}
}

func nextMeetingFrame(t *testing.T, c *wsClient) meetingFrame {
	t.Helper()
	select {
	case raw := <-c.send:
		var f meetingFrame
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Fatalf("frame %s: %v", raw, err)
		}
		if f.Type != "meeting_reply" {
			t.Fatalf("frame type %q, want meeting_reply: %s", f.Type, raw)
		}
		return f
	case <-time.After(3 * time.Second):
		t.Fatal("no meeting_reply frame")
	}
	return meetingFrame{}
}

func TestMeetingCommandRoutesByMeetingID(t *testing.T) {
	s, c, got := meetingCommandServer(t)

	// Two meetings: each command reaches the daemon with its own id, and each
	// reply echoes the id it was for.
	s.handleClientCommand(c, "meeting_command", json.RawMessage(`{"id":"m-bbbbbb","verb":"approve","turn_id":"tb"}`))
	cmd := nextCommand(t, got)
	if cmd.Action != "approve" || cmd.ID != "m-bbbbbb" || cmd.TurnID != "tb" {
		t.Fatalf("daemon got %+v, want approve of turn tb in meeting m-bbbbbb", cmd)
	}
	f := nextMeetingFrame(t, c)
	if f.Data.ID != "m-bbbbbb" || f.Data.Verb != "approve" || f.Data.Reply.State != "ok-approve" || f.Data.Error != "" {
		t.Fatalf("reply frame %+v, want meeting m-bbbbbb approve ok", f.Data)
	}

	s.handleClientCommand(c, "meeting_command", json.RawMessage(`{"id":"m-aaaaaa","verb":"pause"}`))
	if cmd := nextCommand(t, got); cmd.Action != "pause" || cmd.ID != "m-aaaaaa" {
		t.Fatalf("daemon got %+v, want pause of meeting m-aaaaaa", cmd)
	}
	if f := nextMeetingFrame(t, c); f.Data.ID != "m-aaaaaa" || f.Data.Verb != "pause" {
		t.Fatalf("reply frame %+v, want meeting m-aaaaaa pause", f.Data)
	}

	for _, verb := range []string{"pending", "reject", "speak", "resume", "cancel"} {
		s.handleClientCommand(c, "meeting_command", json.RawMessage(`{"id":"m-aaaaaa","verb":"`+verb+`","turn_id":"t1"}`))
		if cmd := nextCommand(t, got); cmd.Action != verb || cmd.ID != "m-aaaaaa" {
			t.Fatalf("verb %s: daemon got %+v", verb, cmd)
		}
		nextMeetingFrame(t, c)
	}
}

func TestMeetingCommandRefusesOutsideTheLifecycleVerbs(t *testing.T) {
	s, c, got := meetingCommandServer(t)
	for _, body := range []string{
		`{"id":"m-aaaaaa","verb":"leave"}`,  // ##meeting leave (PM-4)
		`{"id":"m-aaaaaa","verb":"join"}`,   // starts a bot: not a panel control
		`{"id":"m-aaaaaa","verb":"status"}`, // PM-1
		`{"id":"","verb":"pause"}`,          // no id: the daemon would pick "the only meeting"
		`{"id":"meeting-1","verb":"pause"}`, // not a registry id
		`{"verb":"approve","turn_id":"t"}`,
	} {
		s.handleClientCommand(c, "meeting_command", json.RawMessage(body))
		f := nextMeetingFrame(t, c)
		if f.Data.Error == "" {
			t.Errorf("%s: no error in the reply %+v", body, f.Data)
		}
		select {
		case cmd := <-got:
			t.Errorf("%s reached the daemon as %+v", body, cmd)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// TestMeetingCommandStaysInItsSession: one web server serves one session
// (server_shared_test.go's two-door rule), and a command goes to THAT
// session's workspace inbox. A request cannot name another session: a
// "session" field in the body is ignored, and another session's workspace
// never hears the command.
func TestMeetingCommandStaysInItsSession(t *testing.T) {
	s, c, got := meetingCommandServer(t)
	foreign := make(chan struct{}, 1)
	sub, err := s.nc.Subscribe("other-session.ws.inbox", func(*nats.Msg) { foreign <- struct{}{} })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	_ = s.nc.Flush()

	s.handleClientCommand(c, "meeting_command", json.RawMessage(`{"session":"other-session","id":"m-aaaaaa","verb":"pause"}`))
	if cmd := nextCommand(t, got); cmd.ID != "m-aaaaaa" {
		t.Fatalf("own session got %+v", cmd)
	}
	nextMeetingFrame(t, c)
	select {
	case <-foreign:
		t.Fatal("a meeting_command reached another session's workspace")
	case <-time.After(100 * time.Millisecond):
	}
}
