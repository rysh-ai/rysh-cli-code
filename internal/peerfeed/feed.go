// SPDX-License-Identifier: Apache-2.0

// Package peerfeed follows the session's connected peer board for the TUI
// (design 044 N1/N2, root-order-051). It only READS the board: nothing here
// posts, and no local board, pane output or history leaves the session.
package peerfeed

import (
	"context"
	"strings"
	"time"
	"unicode"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/peer"
)

// DefaultMax bounds the messages the feed keeps and publishes.
const DefaultMax = 100

// pageLimit is one tail request; the server allows 1..200.
const pageLimit = 100

// heartbeat republishes an unchanged feed so a TUI that starts later is
// filled within this long without asking.
const heartbeat = 5 * time.Second

// Identity is who "me" is for alerts: this session's peer identity and the
// coordination endpoints it registered on the board.
type Identity struct {
	SessionID   string
	SessionName string
	Agents      map[string]bool
}

// Feed is the bounded, ordered copy of one peer board.
type Feed struct {
	max       int
	messages  []msg.PeerFeedMessage
	cursor    *int64
	primed    bool
	myThreads map[string]bool
}

func NewFeed(max int) *Feed {
	if max <= 0 {
		max = DefaultMax
	}
	f := &Feed{max: max}
	f.Reset()
	return f
}

// Reset forgets the board: the next Apply is history again.
func (f *Feed) Reset() {
	f.messages, f.cursor, f.primed, f.myThreads = nil, nil, false, map[string]bool{}
}

// Cursor is the newest sequence read, or nil before the first read.
func (f *Feed) Cursor() *int64 { return f.cursor }

// Messages is a copy of what the feed holds, oldest first.
func (f *Feed) Messages() []msg.PeerFeedMessage {
	return append([]msg.PeerFeedMessage(nil), f.messages...)
}

// Apply adds a page read after the cursor. addressed holds the ids of messages
// addressed to one of my endpoints. It returns the new messages that raise an
// alert; the first Apply after a Reset is history and never alerts.
func (f *Feed) Apply(me Identity, page []peer.Message, addressed map[string]bool) []msg.PeerFeedMessage {
	var alerts []msg.PeerFeedMessage
	for _, m := range page {
		if f.cursor != nil && m.Sequence <= *f.cursor {
			continue
		}
		mine := me.SessionID != "" && m.SessionID == me.SessionID
		if mine && m.ID == m.ThreadID {
			f.myThreads[m.ID] = true
		}
		item := msg.PeerFeedMessage{Sequence: m.Sequence, ID: m.ID, ThreadID: m.ThreadID, SessionID: m.SessionID, SessionName: m.SessionName, AgentName: m.AgentName, Text: m.Text, CreatedAt: m.CreatedAt.UnixMilli(), Mine: mine}
		if f.primed {
			item.Alert = Classify(me, m, f.myThreads, addressed)
			if item.Alert != "" {
				alerts = append(alerts, item)
			}
		}
		f.messages = append(f.messages, item)
		seq := m.Sequence
		f.cursor = &seq
	}
	if len(f.messages) > f.max {
		f.messages = append([]msg.PeerFeedMessage(nil), f.messages[len(f.messages)-f.max:]...)
	}
	f.primed = true
	return alerts
}

// Classify returns the alert reason for a message, or "" for none. My own
// messages never alert.
func Classify(me Identity, m peer.Message, myThreads, addressed map[string]bool) string {
	if me.SessionID != "" && m.SessionID == me.SessionID {
		return ""
	}
	switch {
	case addressed[m.ID]:
		return msg.PeerAlertAddressed
	case m.ThreadID != "" && m.ThreadID != m.ID && myThreads[m.ThreadID]:
		return msg.PeerAlertReply
	case mentions(m.Text, me.SessionName):
		return msg.PeerAlertMention
	}
	return msg.PeerAlertMessage
}

// mentions reports whether text names @name as a whole token, ignoring case.
func mentions(text, name string) bool {
	if name == "" {
		return false
	}
	lower, tag := strings.ToLower(text), "@"+strings.ToLower(name)
	for from := 0; ; {
		i := strings.Index(lower[from:], tag)
		if i < 0 {
			return false
		}
		start, end := from+i, from+i+len(tag)
		before := start == 0 || !isNameRune(rune(lower[start-1]))
		after := end == len(lower) || !isNameRune(rune(lower[end]))
		if before && after {
			return true
		}
		from = start + 1
	}
}

func isNameRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '.'
}

// coordinationPrefixes mark a v1 projection of a coordination event that can
// be addressed to an endpoint (design 044 §11: transition, event and readable
// message are one transaction, so they share the id).
var coordinationPrefixes = []string{"[message] ", "[task.assigned] "}

// Poller reads the connected board on a timer and publishes the feed.
type Poller struct {
	Root, Session string
	Key           func(name string) string
	Publish       func(*msg.MsgPeerFeed)
	Alert         func(peer.Connection, msg.PeerFeedMessage)
	Max           int

	feed      *Feed
	conn      string // the connection the feed belongs to
	published *msg.MsgPeerFeed
	at        time.Time
}

// Tick reads once and publishes when anything changed (or on the heartbeat).
func (p *Poller) Tick(ctx context.Context) {
	if p.feed == nil {
		p.feed = NewFeed(p.Max)
	}
	c, err := peer.Load(p.Root, p.Session)
	if err != nil || !c.Enabled {
		p.reset("")
		p.publish(&msg.MsgPeerFeed{})
		return
	}
	key := c.URL + "|" + c.Workspace + "|" + c.Board + "|" + c.SessionID
	if key != p.conn {
		p.reset(key)
	}
	out := &msg.MsgPeerFeed{Connected: true, URL: c.URL, Board: c.Board}
	if pubs, e := peer.LoadPublications(p.Root, p.Session); e == nil && len(pubs.Boards) > 0 {
		out.Published = pubs.Names()
	}
	client := peer.Client{Connection: c, Key: p.Key(c.KeySecret)}
	// History is the newest page; after that, every page past the cursor.
	prev := p.feed.Cursor()
	var page []peer.Message
	after := prev
	for pages := 0; pages < 10; pages++ {
		got, e := client.Tail(ctx, after, pageLimit)
		if e != nil {
			err = e
			break
		}
		page = append(page, got.Messages...)
		if after == nil || !got.HasMore || len(got.Messages) == 0 {
			break
		}
		next := got.Messages[len(got.Messages)-1].Sequence
		after = &next
	}
	me := Identity{SessionID: c.SessionID, SessionName: p.Session}
	addressed := p.addressed(ctx, c, client.Key, page, prev, &me)
	for _, a := range p.feed.Apply(me, page, addressed) {
		if p.Alert != nil {
			p.Alert(c, a)
		}
	}
	out.Messages = p.feed.Messages()
	if err != nil {
		out.Error = err.Error()
		if p.published != nil {
			out.Updated = p.published.Updated
		}
	} else {
		out.Updated = time.Now().UnixMilli()
	}
	p.publish(out)
}

// addressed reads coordination events only when a new message looks like an
// addressed projection, and returns the message ids addressed to an endpoint
// this session registered. Events share the board's sequence with messages
// (design 044 §11), so the message cursor before this page is the event
// cursor. History (prev == nil) never alerts; a board without coordination
// simply has none.
func (p *Poller) addressed(ctx context.Context, c peer.Connection, key string, page []peer.Message, prev *int64, me *Identity) map[string]bool {
	if prev == nil {
		return nil
	}
	look := false
	for _, m := range page {
		for _, prefix := range coordinationPrefixes {
			if strings.HasPrefix(m.Text, prefix) {
				look = true
			}
		}
	}
	if !look {
		return nil
	}
	cc := peer.CoordinationClient{Client: peer.Client{Connection: c, Key: key}}
	state, err := cc.State(ctx)
	if err != nil {
		return nil
	}
	me.Agents = map[string]bool{}
	for id, a := range state.Agents {
		if a == nil {
			continue
		}
		if peerOf, ok := state.Peers[a.PeerID]; ok && peerOf != nil && peerOf.SessionID == c.SessionID {
			me.Agents[id] = true
		}
	}
	out := map[string]bool{}
	cursor := *prev
	for i := 0; i < 10; i++ {
		pg, e := cc.Events(ctx, cursor)
		if e != nil {
			break
		}
		for _, ev := range pg.Events {
			if me.Agents[ev.RecipientID] && (ev.Kind == "message" || ev.Kind == "task.assigned") {
				out[ev.ID] = true
			}
			if ev.Sequence > cursor {
				cursor = ev.Sequence
			}
		}
		if !pg.HasMore || len(pg.Events) == 0 {
			break
		}
	}
	return out
}

func (p *Poller) reset(key string) {
	p.conn = key
	p.feed.Reset()
}

func (p *Poller) publish(f *msg.MsgPeerFeed) {
	if p.Publish == nil {
		return
	}
	if p.published != nil && sameFeed(p.published, f) && time.Since(p.at) < heartbeat {
		return
	}
	p.published, p.at = f, time.Now()
	p.Publish(f)
}

func sameFeed(a, b *msg.MsgPeerFeed) bool {
	if a.Connected != b.Connected || a.URL != b.URL || a.Board != b.Board || a.Error != b.Error || len(a.Messages) != len(b.Messages) || strings.Join(a.Published, ",") != strings.Join(b.Published, ",") {
		return false
	}
	return len(a.Messages) == 0 || a.Messages[len(a.Messages)-1].Sequence == b.Messages[len(b.Messages)-1].Sequence
}

// Run ticks until stop is closed.
func (p *Poller) Run(stop <-chan struct{}, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), every+10*time.Second)
		p.Tick(ctx)
		cancel()
		select {
		case <-stop:
			return
		case <-t.C:
		}
	}
}
