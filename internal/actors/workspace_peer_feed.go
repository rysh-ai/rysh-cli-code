// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"time"

	"github.com/asynkron/protoactor-go/actor"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/peer"
	"github.com/rysh-ai/rysh-cli-code/internal/peerfeed"
)

// The peer feed (design 044 N1/N2, root-order-051). The farm owns one poller
// for the session's peer connection, like the coordination runtime: it reads
// the connected board every two seconds and publishes the bounded snapshot on
// ws.peerFeed for the TUI's peer view. New messages from other sessions raise
// the existing attention path on the pane that connected. Inbound only:
// nothing here posts to the peer board.

// peerFeedEvery is the poll interval, the same two seconds as `peer watch`.
const peerFeedEvery = 2 * time.Second

// peerFeedKeyRequest asks the active workspace for the peer key by secret name.
// The value goes straight into the poller's HTTP client and nowhere else.
type peerFeedKeyRequest struct{ name string }

// peerFeedAlert carries one alerting message to the active workspace.
type peerFeedAlert struct {
	conn    peer.Connection
	message msg.PeerFeedMessage
}

func (f *WorkspaceFarmActor) startPeerFeed(ctx actor.Context) {
	root, pid := ctx.ActorSystem().Root, ctx.Self()
	pub := f.pub
	poller := &peerfeed.Poller{
		Root: f.cfg.RyshDir, Session: f.sessionName, Max: peerfeed.DefaultMax,
		Key: func(name string) string {
			reply, err := root.RequestFuture(pid, &peerFeedKeyRequest{name: name}, 3*time.Second).Result()
			if err != nil {
				return ""
			}
			key, _ := reply.(string)
			return key
		},
		Publish: func(feed *msg.MsgPeerFeed) {
			if pub != nil {
				_ = pub.Send(msg.T("ws", "peerFeed"), feed)
			}
		},
		Alert: func(c peer.Connection, m msg.PeerFeedMessage) {
			root.Send(pid, &peerFeedAlert{conn: c, message: m})
		},
	}
	f.peerFeedStop = make(chan struct{})
	go poller.Run(f.peerFeedStop, peerFeedEvery)

	// N3: the publisher for boards the user explicitly published. It holds
	// no subscription until `peer publish` names a board.
	var codecs *msg.CodecRegistry
	if pub != nil {
		codecs = pub.Codecs()
	}
	publisher := &peerfeed.Publisher{Root: f.cfg.RyshDir, Session: f.sessionName, NC: f.nc, Codecs: codecs, Key: poller.Key, Max: 256}
	go publisher.Run(f.peerFeedStop, time.Second)
}

func (f *WorkspaceFarmActor) stopPeerFeed() {
	if f.peerFeedStop != nil {
		close(f.peerFeedStop)
		f.peerFeedStop = nil
	}
}

// activeChild is the workspace currently subscribed to ws.*, or nil.
func (f *WorkspaceFarmActor) activeChild() *workspaceChild {
	if f.activeIdx >= 0 && f.activeIdx < len(f.children) {
		return f.children[f.activeIdx]
	}
	return nil
}

// handlePeerFeedAlert raises "needs you" for a new peer-board message
// (design 044 N2) on the pane that connected the board, or on the active pane
// when that pane is unknown or gone. The event names the sender and the
// reason, never the message text.
func (w *WorkspaceActor) handlePeerFeedAlert(c peer.Connection, m msg.PeerFeedMessage) {
	if m.Alert == "" || w.pub == nil {
		return
	}
	pane := c.Pane
	if pane == "" || w.paneSnapshotByID(pane) == nil {
		pane = w.activePaneID
	}
	if pane == "" {
		return
	}
	priority := msg.AttentionPriorityHigh
	if m.Alert == msg.PeerAlertMessage {
		priority = msg.AttentionPriorityNormal
	}
	sender := peer.Display(m.SessionName)
	if m.AgentName != "" {
		sender += "/" + peer.Display(m.AgentName)
	}
	_ = w.pub.Send(msg.T("ws", "attention"), &msg.MsgAttentionEvent{
		PaneID:    pane,
		Category:  msg.AttentionPeer,
		Priority:  priority,
		Title:     "Peer board " + peer.Display(c.Board),
		Summary:   fmt.Sprintf("%s · %s · #%d", m.Alert, sender, m.Sequence),
		Timestamp: time.Now().Unix(),
	})
}
