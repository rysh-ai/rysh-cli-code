// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

// Event is a change notice from a Backend: the layout changed, or one pane's
// screen did.
type Event struct {
	Layout bool
	Pane   string
}

// Backend is the remote session as remote-serve sees it.
type Backend interface {
	// Layout returns every tab of the session, trimmed for the wire.
	Layout() ([]domain.TabSnapshot, error)
	// Screen returns a pane's current screen.
	Screen(paneID string) (Screen, error)
	// Apply delivers one command to the session. It returns an error when the
	// command could not be handed to the session.
	Apply(Command) error
	// Watch sets the panes whose screen changes should arrive as Events.
	Watch(paneIDs []string)
	// Events delivers change notices.
	Events() <-chan Event
	// Close releases the backend (subscriptions, size claims, connection).
	Close()
}

// ServeOptions tunes Serve. Zero values take the defaults.
type ServeOptions struct {
	Proto          int           // default ProtoVersion
	Version        string        // build version, announced in hello
	Session        string        // the remote session name, announced in hello
	PollInterval   time.Duration // layout re-read without an event; default 500ms
	ScreenThrottle time.Duration // coalescing window for screen frames; default 33ms
}

// Serve runs the server end of one connection until the client leaves, the
// backend fails, or ctx ends. It returns nil when the client simply left.
func Serve(ctx context.Context, r io.Reader, w io.Writer, b Backend, opts ServeOptions) error {
	defer b.Close()
	if opts.Proto == 0 {
		opts.Proto = ProtoVersion
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 500 * time.Millisecond
	}
	if opts.ScreenThrottle <= 0 {
		opts.ScreenThrottle = 33 * time.Millisecond
	}
	fw := newFrameWriter(w)
	fr := newFrameReader(r)

	// Handshake: the client speaks first.
	line, err := fr.next()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	var hello Frame
	if err := json.Unmarshal(line, &hello); err != nil || hello.T != FrameHello {
		_ = fw.write(Frame{T: FrameError, Code: CodeBadFrame, Error: "expected hello"})
		return errors.New("remote-serve: client did not open with hello")
	}
	if hello.Proto != opts.Proto {
		_ = fw.write(Frame{T: FrameError, Code: CodeProtoMismatch, Proto: opts.Proto, Version: opts.Version,
			Error: fmt.Sprintf("protocol %d requested, this remote speaks %d", hello.Proto, opts.Proto)})
		return mismatchError(opts.Proto, hello.Proto, hello.Version)
	}
	if err := fw.write(Frame{T: FrameHello, Proto: opts.Proto, Version: opts.Version, Session: opts.Session}); err != nil {
		return err
	}

	in := make(chan Frame, 64)
	readErr := make(chan error, 1)
	go func() {
		defer close(in)
		for {
			line, err := fr.next()
			if err != nil {
				readErr <- err
				return
			}
			var f Frame
			if err := json.Unmarshal(line, &f); err != nil {
				readErr <- fmt.Errorf("bad frame: %w", err)
				return
			}
			in <- f
		}
	}()

	s := &serveState{b: b, fw: fw, panes: map[string]bool{}, dirty: map[string]bool{}, sent: map[string][]byte{}}
	if err := s.sendLayout(); err != nil {
		return err
	}
	poll := time.NewTicker(opts.PollInterval)
	defer poll.Stop()
	flush := time.NewTimer(time.Hour)
	flush.Stop()
	armed := false
	arm := func() {
		if !armed && len(s.dirty) > 0 {
			flush.Reset(opts.ScreenThrottle)
			armed = true
		}
	}
	arm()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case f, ok := <-in:
			if !ok {
				err := <-readErr
				if errors.Is(err, io.EOF) {
					return nil // client left
				}
				return err
			}
			if err := s.handle(f); err != nil {
				return err
			}
			arm() // an applied command marks its pane dirty
		case ev := <-b.Events():
			if ev.Layout {
				if err := s.sendLayout(); err != nil {
					return err
				}
			}
			if ev.Pane != "" && s.panes[ev.Pane] {
				s.dirty[ev.Pane] = true
			}
			arm()
		case <-poll.C:
			if err := s.sendLayout(); err != nil {
				return err
			}
			// Text panes have no rawDirty-style signal for every output path;
			// re-read them on the poll (an unchanged screen is not re-sent).
			for _, id := range s.order {
				s.dirty[id] = true
			}
			arm()
		case <-flush.C:
			armed = false
			if err := s.flushScreens(); err != nil {
				return err
			}
		}
	}
}

type serveState struct {
	b          Backend
	fw         *frameWriter
	lastLayout []byte
	order      []string        // pane ids in layout order
	panes      map[string]bool // pane ids in the current layout
	dirty      map[string]bool
	sent       map[string][]byte // last screen frame sent per pane
}

func (s *serveState) sendLayout() error {
	tabs, err := s.b.Layout()
	if err != nil {
		_ = s.fw.write(Frame{T: FrameError, Code: CodeBackend, Error: err.Error()})
		return fmt.Errorf("remote-serve: read layout: %w", err)
	}
	f := Frame{T: FrameLayout, Tabs: tabs}
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if bytes.Equal(data, s.lastLayout) {
		return nil
	}
	s.lastLayout = data
	var order []string
	panes := map[string]bool{}
	for i := range tabs {
		for _, id := range domain.PaneIDsInTab(&tabs[i]) {
			if !panes[id] {
				panes[id] = true
				order = append(order, id)
			}
		}
	}
	for id := range s.sent {
		if !panes[id] {
			delete(s.sent, id)
		}
	}
	for id := range panes {
		if !s.panes[id] {
			s.dirty[id] = true // a new pane: send its first screen
		}
	}
	s.panes, s.order = panes, order
	s.b.Watch(order)
	return s.fw.write(f)
}

func (s *serveState) flushScreens() error {
	for _, id := range s.order {
		if !s.dirty[id] {
			continue
		}
		delete(s.dirty, id)
		sc, err := s.b.Screen(id)
		if err != nil {
			continue // a pane that just closed; the next layout drops it
		}
		f := Frame{T: FrameScreen, Pane: id, Interactive: sc.Interactive, Screen: sc.Rows, CursorRow: sc.CursorRow, CursorCol: sc.CursorCol, Text: sc.Text}
		data, err := json.Marshal(f)
		if err != nil {
			return err
		}
		if bytes.Equal(data, s.sent[id]) {
			continue
		}
		s.sent[id] = data
		if err := s.fw.write(f); err != nil {
			return err
		}
	}
	for id := range s.dirty {
		if !s.panes[id] {
			delete(s.dirty, id)
		}
	}
	return nil
}

func (s *serveState) handle(f Frame) error {
	switch f.T {
	case FramePing:
		return s.fw.write(Frame{T: FramePong, ID: f.ID})
	case FrameCmd:
		ack := Frame{T: FrameAck, ID: f.ID}
		switch {
		case !KnownCommand(f.Cmd):
			ack.Error = "unknown command type " + f.Cmd
		case !s.panes[f.Pane]:
			// Fail closed: only panes of the session's current layout are reachable.
			ack.Error = "pane " + f.Pane + " is not in the remote session"
		default:
			if err := s.b.Apply(Command{Type: f.Cmd, Pane: f.Pane, Payload: f.Payload}); err != nil {
				ack.Error = err.Error()
			} else {
				ack.OK = true
				s.dirty[f.Pane] = true
			}
		}
		return s.fw.write(ack)
	default:
		return s.fw.write(Frame{T: FrameError, Code: CodeBadFrame, Error: "unexpected frame " + f.T})
	}
}
