// SPDX-License-Identifier: Apache-2.0

package familyclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// ErrReplyTimeout is returned by Chat.Ask when reply_done does not arrive
// before the context's deadline.
var ErrReplyTimeout = errors.New("timed out waiting for the reply")

// Chat is an open family socket used to send messages and stream replies.
type Chat struct {
	c      *Client
	fid    string
	events chan Event
	errc   chan error
	cancel context.CancelFunc
	fmt    *EventFormatter
}

// OpenChat connects to family fid's socket and returns once the server has
// accepted it (the `hello` frame, or a short grace after the handshake), so
// a reply to a message sent afterwards cannot be missed. ctx bounds only the
// connect; Close ends the socket.
func (c *Client) OpenChat(ctx context.Context, fam Family, opts FollowOptions) (*Chat, error) {
	sctx, cancel := context.WithCancel(context.Background())
	ch := &Chat{
		c:      c,
		fid:    fam.ID,
		events: make(chan Event, 1024),
		errc:   make(chan error, 1),
		cancel: cancel,
		fmt:    NewEventFormatter(fam),
	}
	connected := make(chan struct{}, 1)
	userConnect := opts.OnConnect
	opts.OnConnect = func(n int) {
		select {
		case connected <- struct{}{}:
		default:
		}
		if userConnect != nil {
			userConnect(n)
		}
	}
	go func() {
		err := c.Follow(sctx, fam.ID, opts, func(ev Event) {
			select {
			case ch.events <- ev:
			default: // a reader that is not asking anything does not stall the socket
			}
		})
		ch.errc <- err
	}()

	select {
	case <-connected:
	case err := <-ch.errc:
		cancel()
		return nil, err
	case <-ctx.Done():
		cancel()
		return nil, fmt.Errorf("connecting to the family socket: %w", ctx.Err())
	}
	// Wait briefly for hello: the server sends it once the subscription is live.
	grace := time.NewTimer(2 * time.Second)
	defer grace.Stop()
	for {
		select {
		case ev := <-ch.events:
			ch.fmt.Observe(ev)
			if ev.Type == "hello" {
				return ch, nil
			}
		case <-grace.C:
			return ch, nil
		case err := <-ch.errc:
			cancel()
			return nil, err
		case <-ctx.Done():
			cancel()
			return nil, fmt.Errorf("connecting to the family socket: %w", ctx.Err())
		}
	}
}

// Close ends the socket.
func (ch *Chat) Close() { ch.cancel() }

// Ask sends body to avatar aid, writes the streamed reply to out as it
// arrives, and returns the final message when reply_done arrives. Avatar-to-
// avatar notes and errors seen meanwhile are written, one line each, to info
// (which may be nil). ctx's deadline is the reply timeout.
func (ch *Chat) Ask(ctx context.Context, aid, body string, out, info io.Writer) (Message, error) {
	// Drop whatever arrived while nobody was asking.
	for drained := false; !drained; {
		select {
		case ev := <-ch.events:
			ch.fmt.Observe(ev)
		default:
			drained = true
		}
	}
	if _, err := ch.c.SendMessage(ctx, ch.fid, aid, body); err != nil {
		return Message{}, err
	}
	var streamed strings.Builder
	for {
		select {
		case <-ctx.Done():
			if streamed.Len() > 0 {
				fmt.Fprintln(out)
			}
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return Message{}, ErrReplyTimeout
			}
			return Message{}, ctx.Err()
		case err := <-ch.errc:
			ch.errc <- err // keep it for the next Ask
			if streamed.Len() > 0 {
				fmt.Fprintln(out)
			}
			return Message{}, err
		case ev := <-ch.events:
			ch.fmt.Observe(ev)
			switch ev.Type {
			case "delta":
				if ev.AvatarID == aid {
					streamed.WriteString(ev.Text)
					_, _ = io.WriteString(out, ev.Text)
				}
			case "reply_done":
				if ev.AvatarID != aid {
					continue
				}
				var final Message
				if ev.Message != nil {
					final = *ev.Message
				}
				got := streamed.String()
				switch {
				case got == "":
					_, _ = io.WriteString(out, final.Body)
				case strings.HasPrefix(final.Body, got):
					_, _ = io.WriteString(out, final.Body[len(got):])
				case final.Body != "" && final.Body != got:
					// Deltas were lost (a reconnect mid-reply): print the
					// stored reply whole rather than a torn one.
					fmt.Fprintf(out, "\n[reconnected mid-reply; the full reply follows]\n%s", final.Body)
				}
				text := final.Body
				if text == "" {
					text = got
				}
				if !strings.HasSuffix(text, "\n") {
					fmt.Fprintln(out)
				}
				return final, nil
			case "note", "error":
				if info != nil {
					if l := ch.fmt.Line(ev); l != "" {
						fmt.Fprintln(info, l)
					}
				}
			}
		}
	}
}
