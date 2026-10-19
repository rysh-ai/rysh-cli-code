// SPDX-License-Identifier: Apache-2.0

package msg

import "github.com/rysh-ai/rysh-cli-code/internal/meeting"

type MsgMeetingCommand struct {
	Command meeting.Command `json:"command"`
}
type MsgMeetingReply struct {
	Reply meeting.Reply `json:"reply"`
}

// MsgMeetingToken asks the workspace for a LiveKit participant token for the
// human in a meeting pane (design 043, desktop app `meeting_token`). The room
// comes from the pane's registry entry, never from the requester. Without a
// pane, Room names a rysh-server room on the session's own rysh server (the
// app's Meeting panel, E-87); the server and the key stay the daemon's.
type MsgMeetingToken struct {
	PaneID string `json:"pane_id"`
	Room   string `json:"room,omitempty"`
}

// MsgMeetingTokenReply carries the grant or an error. The token is passed
// through to the requesting client only; it is never stored or logged.
type MsgMeetingTokenReply struct {
	PaneID    string `json:"pane_id"`
	Room      string `json:"room,omitempty"` // echoes MsgMeetingToken.Room
	URL       string `json:"url,omitempty"`
	Token     string `json:"token,omitempty"`
	Identity  string `json:"identity,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	Error     string `json:"error,omitempty"`
}
