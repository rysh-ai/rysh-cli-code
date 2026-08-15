// SPDX-License-Identifier: Apache-2.0

package board

import (
	"encoding/json"
	"sync"
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
	Kind EventKind

	// Board is the board this message was DELIVERED TO, read from the subject
	// (design 028). It is never read from the payload: a post carries no board
	// id, which is what keeps founder gate 4 closed. A consumer holding several
	// boards routes on this field.
	Board string

	Post     *msg.MsgBoardPost
	Register *msg.MsgBoardRegister
}

// PersistenceFor resolves the single writer for a board. Returning nil for a
// board makes the feed READ-ONLY for it — which is what every TUI passes, and
// what keeps the session's memory out of a process that may not be running.
//
// The function is called on the NATS callback goroutine, so an implementation
// that creates a Persistence on demand must not do I/O beyond what
// Persistence.prime already does on the first write.
type PersistenceFor func(board string) *Persistence

// SingleBoardPersistence is the PersistenceFor of a caller that records exactly
// one board and ignores the rest. It exists so that "one board" cannot be
// spelled as "return p for anything", which would write every board's posts
// into one board's keys.
func SingleBoardPersistence(board string, p *Persistence) PersistenceFor {
	want := msg.NormalizeBoardID(board)
	return func(b string) *Persistence {
		if msg.NormalizeBoardID(b) != want {
			return nil
		}
		return p
	}
}

// Subscriber is the live board feed: EVERY board in the session, decoded onto
// one buffered channel and tagged with the board it arrived on.
//
// Four subscriptions, not two, and the pairing is the point (design 028): the
// wildcard `board.*.post` hears every named board, and the legacy `board.post`
// hears the default one, whose subject is one token shorter and therefore does
// not match the wildcard. One connection and one callback path per family, no
// per-board subscription bookkeeping, and a board that appears mid-session is
// heard without anyone being told it exists.
type Subscriber struct {
	ch       chan Event
	subs     []*nats.Subscription
	dropped  atomic.Uint64
	persist  PersistenceFor
	writeErr atomic.Uint64

	// perBoardDropped counts drops per board. A shared counter would report
	// fleet 7's drops on fleet 3's board, which is the kind of number that gets
	// acted on by the wrong person.
	dropMu          sync.Mutex
	perBoardDropped map[string]uint64
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
func Subscribe(nc *nats.Conn, codecs *msg.CodecRegistry, buf int, persist PersistenceFor) (*Subscriber, error) {
	if buf <= 0 {
		buf = DefaultBufferSize
	}
	s := &Subscriber{
		ch:              make(chan Event, buf),
		persist:         persist,
		perBoardDropped: make(map[string]uint64),
	}

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

	onPost := func(n *nats.Msg) {
		// The board comes from the SUBJECT the message was delivered on. A
		// subject that does not parse is dropped rather than defaulted: filing
		// a fleet's post onto the session board would be a silent misroute,
		// and there is no subject this subscriber listens to that cannot parse.
		board, ok := msg.BoardIDFromSubject(n.Subject, "post")
		if !ok {
			return
		}
		if d, ok := decode(n.Data); ok {
			if p, ok := d.(*msg.MsgBoardPost); ok {
				// Unconditional: see the note at the top of this file.
				if err := s.persistFor(board).SavePost(p); err != nil {
					s.writeErr.Add(1)
				}
				s.push(Event{Kind: EventPost, Board: board, Post: p})
			}
		}
	}

	onRegister := func(n *nats.Msg) {
		board, ok := msg.BoardIDFromSubject(n.Subject, "register")
		if !ok {
			return
		}
		if d, ok := decode(n.Data); ok {
			if r, ok := d.(*msg.MsgBoardRegister); ok {
				if err := s.persistFor(board).SaveRegister(r); err != nil {
					s.writeErr.Add(1)
				}
				s.push(Event{Kind: EventRegister, Board: board, Register: r})
			}
		}
	}

	// Both shapes of every family. Failing halfway leaves NOTHING running: a
	// Subscriber that reports an error and keeps delivering posts is the worst
	// of both, and with four subscriptions the half-open case is likelier than
	// it was with two.
	for _, w := range []struct {
		subject string
		handler nats.MsgHandler
	}{
		{msg.BoardPostSubject(msg.DefaultBoardID), onPost},
		{msg.BoardPostPattern(), onPost},
		{msg.BoardRegisterSubject(msg.DefaultBoardID), onRegister},
		{msg.BoardRegisterPattern(), onRegister},
	} {
		sub, err := nc.Subscribe(w.subject, w.handler)
		if err != nil {
			s.Close()
			return nil, err
		}
		s.subs = append(s.subs, sub)
	}

	return s, nil
}

// persistFor is the writer for one board, or nil for a read-only feed. Nil
// *Persistence methods are no-ops, so callers need no branch.
func (s *Subscriber) persistFor(board string) *Persistence {
	if s.persist == nil {
		return nil
	}
	return s.persist(board)
}

// push is non-blocking by design: this runs on the NATS callback goroutine and
// blocking it stalls every other subscription on the connection.
func (s *Subscriber) push(ev Event) {
	select {
	case s.ch <- ev:
	default:
		s.dropped.Add(1)
		s.dropMu.Lock()
		s.perBoardDropped[ev.Board]++
		s.dropMu.Unlock()
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

// DroppedFor is the same count for ONE board, which is what a board view must
// render: a session-wide number shown on one fleet's board is a claim about
// that fleet that is not true.
func (s *Subscriber) DroppedFor(board string) uint64 {
	id := msg.NormalizeBoardID(board)
	s.dropMu.Lock()
	defer s.dropMu.Unlock()
	return s.perBoardDropped[id]
}

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
