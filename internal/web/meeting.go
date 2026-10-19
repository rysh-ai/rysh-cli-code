// SPDX-License-Identifier: Apache-2.0

package web

import (
	"encoding/json"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/meeting"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// meetingTokenTimeout covers the daemon's call to the rysh server.
const meetingTokenTimeout = 25 * time.Second

// handleMeetingToken answers meeting_token (design 043 §Pane contract) for
// the ORIGINATING client only: a LiveKit participant token for the meeting
// pane's room. The daemon picks the room from the pane's registry entry; the
// client names only the pane. The token is passed through, never logged.
func (s *Server) handleMeetingToken(c *wsClient, data json.RawMessage) {
	var cmd struct {
		PaneID string `json:"pane_id"`
		Room   string `json:"room"` // no pane: a room on the session's rysh server (E-87)
	}
	if json.Unmarshal(data, &cmd) != nil {
		return
	}
	pub := s.pub
	go func() {
		res, err := pub.Request(msg.T("ws", "inbox"), &msg.MsgMeetingToken{PaneID: cmd.PaneID, Room: cmd.Room}, meetingTokenTimeout)
		r, ok := res.(*msg.MsgMeetingTokenReply)
		var frame []byte
		switch {
		case err != nil || !ok:
			frame, err = json.Marshal(map[string]interface{}{
				"type": "meeting_token",
				"data": map[string]interface{}{"pane_id": cmd.PaneID, "room": cmd.Room, "error": "the daemon did not answer the token request"},
			})
		case r.Error != "":
			frame, err = json.Marshal(map[string]interface{}{
				"type": "meeting_token",
				"data": map[string]interface{}{"pane_id": cmd.PaneID, "room": cmd.Room, "error": r.Error},
			})
		default:
			frame, err = json.Marshal(map[string]interface{}{
				"type": "meeting_token",
				"data": map[string]interface{}{"pane_id": cmd.PaneID, "room": cmd.Room, "url": r.URL, "token": r.Token, "identity": r.Identity, "expires_at": r.ExpiresAt},
			})
		}
		if err != nil {
			return
		}
		s.hub.sendWhere(frame, func(x *wsClient) bool { return x == c })
	}()
}

// meetingCommandTimeout matches `rysh meeting`'s own request timeout.
const meetingCommandTimeout = 30 * time.Second

// meetingPanelVerbs are the design-040 lifecycle controls the desktop app's
// meeting panel may send (MTG-6). list/status/leave/join and the rest stay
// with ##meeting and the CLI: this route can approve a turn and make the bot
// speak, so it carries nothing else.
var meetingPanelVerbs = map[string]bool{
	"pending": true, "approve": true, "reject": true, "speak": true,
	"pause": true, "resume": true, "cancel": true,
}

// handleMeetingCommand answers meeting_command for the ORIGINATING client:
// one lifecycle verb for one meeting, forwarded as the same MsgMeetingCommand
// the CLI and TUI send, to THIS server's session workspace. The meeting is
// named by its registry id — required: the daemon's "" default means "the
// only meeting", which with several open is exactly the cross-talk to avoid.
// It is reached only through /ws, so it sits behind the same login gate as
// every other ws action (authMiddleware, server.go). The reply frame echoes
// id and verb so the app routes it to the right panel.
func (s *Server) handleMeetingCommand(c *wsClient, data json.RawMessage) {
	var cmd struct {
		ID     string `json:"id"`
		Verb   string `json:"verb"`
		TurnID string `json:"turn_id"`
	}
	_ = json.Unmarshal(data, &cmd)
	reply := func(r meeting.Reply, errText string) {
		frame, err := json.Marshal(map[string]interface{}{
			"type": "meeting_reply",
			"data": map[string]interface{}{"id": cmd.ID, "verb": cmd.Verb, "reply": r, "error": errText},
		})
		if err != nil {
			return
		}
		s.hub.sendWhere(frame, func(x *wsClient) bool { return x == c })
	}
	switch {
	case !meetingPanelVerbs[cmd.Verb]:
		reply(meeting.Reply{}, "meeting_command takes pending, approve, reject, speak, pause, resume or cancel; use ##meeting for the rest")
		return
	case !meeting.ValidMeetingID(cmd.ID):
		reply(meeting.Reply{}, "meeting_command needs the meeting's id (m-xxxxxx)")
		return
	}
	pub := s.pub
	c2 := meeting.Command{Action: cmd.Verb, ID: cmd.ID, TurnID: cmd.TurnID}
	go func() {
		res, err := pub.Request(msg.T("ws", "inbox"), &msg.MsgMeetingCommand{Command: c2}, meetingCommandTimeout)
		r, ok := res.(*msg.MsgMeetingReply)
		switch {
		case err != nil || !ok:
			reply(meeting.Reply{}, "the daemon did not answer the meeting command")
		default:
			reply(r.Reply, r.Reply.Error)
		}
	}()
}
