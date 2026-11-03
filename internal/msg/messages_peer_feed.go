// SPDX-License-Identifier: Apache-2.0

package msg

// The peer feed (design 044 N1/N2, root-order-051): the daemon follows the
// session's connected peer board and publishes a bounded snapshot of it on
// ws.peerFeed, so the TUI board can show it and raise "needs you" on new
// messages. It is INBOUND only: nothing here posts to a peer board, and no
// local board, pane output or history leaves the session through it.

// AttentionPeer is the attention category of a peer-board alert. Like a
// "needs you" prompt, it follows `[ui] notify_terminal` (default off).
const AttentionPeer AttentionCategory = "peer"

// Peer alert reasons, most to least direct. Empty means no alert.
const (
	PeerAlertAddressed = "addressed" // a coordination message or task addressed to one of my endpoints
	PeerAlertReply     = "reply"     // a reply in a thread my session started
	PeerAlertMention   = "mention"   // @<my session name> in the text
	PeerAlertMessage   = "message"   // any other new message from another session
)

// PeerFeedMessage is one peer-board message as the TUI shows it. Text is
// remote data: the TUI strips controls before drawing it.
type PeerFeedMessage struct {
	Sequence    int64  `json:"sequence"`
	ID          string `json:"id"`
	ThreadID    string `json:"thread_id"`
	SessionID   string `json:"session_id"`
	SessionName string `json:"session_name"`
	AgentName   string `json:"agent_name"`
	Text        string `json:"text"`
	CreatedAt   int64  `json:"created_at"` // unix millis
	Mine        bool   `json:"mine,omitempty"`
	Alert       string `json:"alert,omitempty"`
}

// MsgPeerFeed is the daemon's snapshot of the connected peer board.
type MsgPeerFeed struct {
	Connected bool              `json:"connected"`
	URL       string            `json:"url,omitempty"`
	Board     string            `json:"board,omitempty"`
	Error     string            `json:"error,omitempty"`
	Messages  []PeerFeedMessage `json:"messages,omitempty"`
	Updated   int64             `json:"updated"` // unix millis of the last successful read
	// Published lists the local boards the user published to this peer
	// board (N3), so every board view can show that it is published.
	Published []string `json:"published,omitempty"`
}
