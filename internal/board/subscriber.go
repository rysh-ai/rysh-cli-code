package board

import (
	"encoding/json"
	"sync/atomic"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// The live feed. Modelled on internal/tui/email_view.go:99-146
// (setupEmailSubscriptions): decode the NATSEnvelope through the codec
// registry, push onto a buffered channel, never block the NATS callback.
//
// ONE CORRECTION TO THAT PRECEDENT, and it is the reason this file has counters
// the email one does not. email_view.go:95-98 argues drop-on-full is safe there
// because a missed list or detail is re-fetched on the next user action. THAT
// REASONING DOES NOT TRANSFER to a live board feed. Drop-on-full stays — a full
// buffer must never block the bus — but the drops are COUNTED and exposed, so a
// view can tell the human what it is not showing instead of quietly rendering a
// shorter history than really happened.
//
// WHAT MAKES A DROP SURVIVABLE: since founder gate 2 the board persists, and the
// KV write is UNCONDITIONAL — it runs on every delivered message, never inside
// the buffer's drop branch. That, not the order of the two statements, is what
// lets Subscriber.Dropped mean "not shown live" rather than "lost": a dropped
// event is still in the KV and a view can recover it.
//
// (Stated precisely because the obvious phrasing — "the write happens first" —
// is wrong: push() does not return early on a drop, so either order persists
// everything. The property to protect is that persistence is not conditional on
// the hand-off succeeding, and TestPersistenceOutlivesARenderDrop is what pins
// it.) See persist.go for exactly what is durable and what is still lost.
//
// This file takes a *nats.Conn and a *msg.CodecRegistry rather than a *bus.Bus
// so that the package stays free of the bus's embedded-server dependencies and
// can be tested against a bare in-process NATS server.

// DefaultBufferSize is the event buffer between the NATS callbacks and the
// consumer.
//
// Sized for the burst this epic exists for: a 44-agent fleet where every agent
// reports a milestone at once, plus headroom for a consumer that is mid-render.
// email_view.go uses 256 for a stream whose losses heal themselves; a board
// post's loss does not heal, so this is 4x that. Beyond it, drops are counted
// rather than hidden — see Subscriber.Dropped.
const DefaultBufferSize = 1024

// EventKind distinguishes the two board subjects.
type EventKind int

const (
	// EventPost carries a message for the stream.
	EventPost EventKind = iota
	// EventRegister carries a persona announcement.
	EventRegister
)

// Event is one decoded board message on its way to a Store.
type Event struct {
	Kind     EventKind
	Post     *msg.MsgBoardPost
	Register *msg.MsgBoardRegister
}

// Subscriber is the live board feed: two NATS subscriptions decoded onto one
// buffered channel.
type Subscriber struct {
	ch       chan Event
	subs     []*nats.Subscription
	dropped  atomic.Uint64
	persist  *Persistence
	writeErr atomic.Uint64
}

// Subscribe wires the board subjects onto a buffered channel. buf <= 0 means
// DefaultBufferSize. persist may be nil, which gives a live-only board rather
// than a refusal to start.
//
// The caller drains Events() and must Close when done.
//
// Subjects come from msg.BoardPostSubject / msg.BoardRegisterSubject, which are
// built with msg.T(...) — never literals, because T's prefix is the SESSION
// NAME and a literal "rysh.board.post" breaks the moment a session is called
// anything else (rysh-shared/msg/topics.go).
func Subscribe(nc *nats.Conn, codecs *msg.CodecRegistry, buf int, persist *Persistence) (*Subscriber, error) {
	if buf <= 0 {
		buf = DefaultBufferSize
	}
	s := &Subscriber{ch: make(chan Event, buf), persist: persist}

	decode := func(data []byte) (interface{}, bool) {
		var env msg.NATSEnvelope
		if json.Unmarshal(data, &env) != nil {
			return nil, false
		}
		d, err := codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			return nil, false
		}
		return d, true
	}

	postSub, err := nc.Subscribe(msg.BoardPostSubject(), func(n *nats.Msg) {
		if d, ok := decode(n.Data); ok {
			if p, ok := d.(*msg.MsgBoardPost); ok {
				// Unconditional: see the note at the top of this file.
				if err := s.persist.SavePost(p); err != nil {
					s.writeErr.Add(1)
				}
				s.push(Event{Kind: EventPost, Post: p})
			}
		}
	})
	if err != nil {
		return nil, err
	}
	s.subs = append(s.subs, postSub)

	regSub, err := nc.Subscribe(msg.BoardRegisterSubject(), func(n *nats.Msg) {
		if d, ok := decode(n.Data); ok {
			if r, ok := d.(*msg.MsgBoardRegister); ok {
				if err := s.persist.SaveRegister(r); err != nil {
					s.writeErr.Add(1)
				}
				s.push(Event{Kind: EventRegister, Register: r})
			}
		}
	})
	if err != nil {
		// Undo the first subscription rather than leaving half a feed running:
		// a Subscriber that reports an error but keeps delivering posts is the
		// worst of both.
		_ = postSub.Unsubscribe()
		s.subs = nil
		return nil, err
	}
	s.subs = append(s.subs, regSub)

	return s, nil
}

// push is non-blocking by design: this runs on the NATS callback goroutine and
// blocking it stalls every other subscription on the connection.
func (s *Subscriber) push(ev Event) {
	select {
	case s.ch <- ev:
	default:
		s.dropped.Add(1)
	}
}

// Events is the feed. A consumer drains it and calls Store.ApplyEvent.
func (s *Subscriber) Events() <-chan Event { return s.ch }

// Dropped is how many messages the render buffer refused. With persistence
// configured these are NOT lost — the KV write is unconditional, so a dropped
// event was still recorded — and a view reports them as "not shown live",
// recoverable by re-reading. Without persistence they are gone, and the view
// must say so.
func (s *Subscriber) Dropped() uint64 { return s.dropped.Load() }

// WriteErrors counts messages the KV refused. A non-zero value means the board
// is live-only for those messages: they were delivered but will not survive a
// restart. Surfaced rather than logged-and-forgotten, because "the board
// persists" quietly becoming false is the failure mode worth seeing.
func (s *Subscriber) WriteErrors() uint64 { return s.writeErr.Load() }

// Close unsubscribes. The event channel is left open: a consumer draining it in
// a select would otherwise spin on a closed channel, and an abandoned buffered
// channel is collected with the Subscriber.
func (s *Subscriber) Close() {
	for _, sub := range s.subs {
		_ = sub.Unsubscribe()
	}
	s.subs = nil
}
