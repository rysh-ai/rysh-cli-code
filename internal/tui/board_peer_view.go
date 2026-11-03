// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/bus"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// The peer view (design 044 N1, root-order-051). P in an agents-board pane
// shows the session's connected peer board instead of the local one. It is
// READ-ONLY and fed by the daemon's peer feed (internal/peerfeed) on
// ws.peerFeed: this TUI never talks to the peer server. Posting stays
// `##peer post` / `##peer reply`, explicit as design 044 requires.
//
// Unread (N2) is what other sessions posted above peerSeen. The first snapshot
// a TUI receives is history, so a fresh TUI starts with nothing unread; the
// daemon's "needs you" alerts follow the same rule.

// peerFeedMsg carries one daemon snapshot of the peer board into Update.
type peerFeedMsg struct{ feed *msgpkg.MsgPeerFeed }

// setupPeerFeedSubscription subscribes to the daemon's peer feed. One exact
// subject, no wildcard.
func setupPeerFeedSubscription(b *bus.Bus, codecs *msgpkg.CodecRegistry) chan peerFeedMsg {
	ch := make(chan peerFeedMsg, 4)
	if b == nil || b.Conn() == nil || codecs == nil {
		return ch
	}
	_, _ = b.Conn().Subscribe(msgpkg.T("ws", "peerFeed"), func(natMsg *nats.Msg) {
		var env msgpkg.NATSEnvelope
		if err := json.Unmarshal(natMsg.Data, &env); err != nil {
			return
		}
		decoded, err := codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			return
		}
		if f, ok := decoded.(*msgpkg.MsgPeerFeed); ok {
			select {
			case ch <- peerFeedMsg{feed: f}:
			default: // the next snapshot carries the same and more
			}
		}
	})
	return ch
}

// listenPeerFeedCmd blocks until the next peer-feed snapshot.
func (m Model) listenPeerFeedCmd() tea.Cmd {
	ch := m.peerFeedCh
	if ch == nil {
		return nil
	}
	return func() tea.Msg { return <-ch }
}

// peerFeedChanged reports whether the new feed would draw a different frame.
// Updated (the time of the last read) changes on every feed and is drawn only
// in a board pane showing the peer view, so it counts only while one is open.
// A false "changed" costs one render; a false "unchanged" would hide news, so
// anything else that differs counts as changed.
func peerFeedChanged(old, next *msgpkg.MsgPeerFeed, updatedShown bool) bool {
	if old == nil || next == nil {
		return old != next
	}
	a, b := *old, *next
	if !updatedShown {
		a.Updated, b.Updated = 0, 0
	}
	return !reflect.DeepEqual(a, b)
}

// anyPeerView reports whether some board pane is in peer view.
func (m Model) anyPeerView() bool {
	for _, st := range m.boardViews {
		if st != nil && st.peer {
			return true
		}
	}
	return false
}

// applyPeerFeed stores a peer-feed snapshot.
func (m *Model) applyPeerFeed(f *msgpkg.MsgPeerFeed) {
	if f == nil {
		return
	}
	first := m.peerFeed == nil
	m.peerFeed = f
	if first || m.peerViewFocused() {
		m.markPeerSeen()
	}
}

// peerViewFocused reports whether the focused pane is a board in peer view.
func (m Model) peerViewFocused() bool {
	snap := m.focusedPaneSnapshot()
	if snap == nil || snap.PaneType != domain.PaneTypeAgentsBoard || m.boardViews == nil {
		return false
	}
	st, ok := m.boardViews[snap.ID]
	return ok && st.peer
}

// markPeerSeen marks everything in the feed read and clears peer alerts.
func (m *Model) markPeerSeen() {
	if m.peerFeed != nil {
		for _, msg := range m.peerFeed.Messages {
			if msg.Sequence > m.peerSeen {
				m.peerSeen = msg.Sequence
			}
		}
	}
	for id, info := range m.attentionState {
		if info != nil && info.Category == msgpkg.AttentionPeer {
			delete(m.attentionState, id)
		}
	}
}

// peerUnread counts other sessions' messages the user has not looked at.
func (m Model) peerUnread() int {
	if m.peerFeed == nil {
		return 0
	}
	n := 0
	for _, msg := range m.peerFeed.Messages {
		if !msg.Mine && msg.Sequence > m.peerSeen {
			n++
		}
	}
	return n
}

// peerAlertTag is the short mark an alerting message carries in the view.
func peerAlertTag(reason string) string {
	switch reason {
	case msgpkg.PeerAlertAddressed:
		return " [for you]"
	case msgpkg.PeerAlertReply:
		return " [reply]"
	case msgpkg.PeerAlertMention:
		return " [@you]"
	}
	return ""
}

// peerViewRows renders the feed oldest-first, like every board view. Remote
// text is data: escape sequences and controls are stripped before drawing.
func (m Model) peerViewRows(width int) []string {
	f := m.peerFeed
	if f == nil || !f.Connected {
		return []string{
			"not connected to a peer board.",
			"connect this session:  ##peer connect <https://server> --key-secret <NAME> [--board team]",
			"only messages posted with ##peer post / ##peer reply cross; nothing is mirrored.",
		}
	}
	var rows []string
	for _, msg := range f.Messages {
		who := clean(msg.SessionName) + "/" + clean(msg.AgentName)
		if msg.Mine {
			who += " (you)"
		}
		lead, indent := boardRootGlyph+" ", strings.Repeat(" ", boardRootIndent)
		if msg.ThreadID != "" && msg.ThreadID != msg.ID {
			lead, indent = "  "+boardReplyGlyph+" ", strings.Repeat(" ", boardReplyIndent)
		}
		stamp := ""
		if msg.CreatedAt > 0 {
			stamp = " " + time.UnixMilli(msg.CreatedAt).Local().Format("15:04")
		}
		unread := ""
		if !msg.Mine && msg.Sequence > m.peerSeen {
			unread = "  •new"
		}
		rows = append(rows, fmt.Sprintf("%s%s%s  #%d%s%s", lead, who, peerAlertTag(msg.Alert), msg.Sequence, stamp, unread))
		for _, line := range strings.Split(ansi.Strip(msg.Text), "\n") {
			for _, w := range wrapBoardLine(sanitiseBoardInline(line), max(1, width-len(indent))) {
				rows = append(rows, indent+w)
			}
		}
	}
	if len(rows) == 0 {
		rows = append(rows, "no messages on this peer board yet.")
	}
	return rows
}

func clean(s string) string {
	s = sanitiseBoardInline(ansi.Strip(s))
	if s == "" {
		return "?"
	}
	return s
}

// peerViewHeader is the header line of the peer view.
func (m Model) peerViewHeader() string {
	f := m.peerFeed
	if f == nil || !f.Connected {
		return "PEER BOARD | not connected · keys s p a f t P"
	}
	parts := []string{fmt.Sprintf("PEER BOARD %s | %s", clean(f.Board), plural(len(f.Messages), "message"))}
	if f.Error != "" {
		parts = append(parts, "⚠ "+clean(f.Error))
	} else if f.Updated > 0 {
		parts = append(parts, "updated "+time.UnixMilli(f.Updated).Local().Format("15:04:05"))
	}
	parts = append(parts, "read-only · ##peer post to write · keys s p a f t P")
	return strings.Join(parts, "  ·  ")
}

// boardPeerLayout is boardPanelLayout for the peer view: pinned to the newest
// message like the stream views, scrolled with the same keys.
func (m Model) boardPeerLayout(pane domain.PaneSnapshot, st *boardViewState, contentWidth, maxLines int) boardLayout {
	rows := m.peerViewRows(contentWidth)
	st.lastRows = len(rows)
	st.lastVisible = maxLines
	st.scrollOffset = clampBoardScroll(st.scrollOffset, st)
	start, end := visibleRowRange(len(rows), maxLines, st.scrollOffset)
	lay := boardLayout{rows: make([]string, 0, maxLines), start: start}
	for i := start; i < end; i++ {
		lay.rows = append(lay.rows, truncBoardLine(rows[i], contentWidth))
	}
	for len(lay.rows) < maxLines {
		lay.rows = append(lay.rows, "")
	}
	lay.refs = make([]boardRowRef, len(lay.rows))
	lay.all = lay.rows
	lay.allRefs = lay.refs
	lay.header = truncBoardLine(m.peerViewHeader(), contentWidth)
	lay.input = truncBoardLine(m.boardInputLine(pane.ID), contentWidth)
	lay.status = truncBoardLine(m.boardStatusLine(st), contentWidth)
	return lay
}

// peerUnreadNote is appended to the other views' headers while there is
// something unread on the peer board.
func (m Model) peerUnreadNote() string {
	if n := m.peerUnread(); n > 0 {
		return fmt.Sprintf("peer: %d unread (P)", n)
	}
	return ""
}

// peerPublishedNote marks a board the user published to the peer board (N3),
// in every view of it, so publishing is never invisible.
func (m Model) peerPublishedNote(boardID string) string {
	if m.peerFeed == nil || !m.peerFeed.Connected {
		return ""
	}
	id := msgpkg.NormalizeBoardID(boardID)
	for _, b := range m.peerFeed.Published {
		if msgpkg.NormalizeBoardID(b) == id {
			return "⇪ published to peer board " + clean(m.peerFeed.Board)
		}
	}
	return ""
}
