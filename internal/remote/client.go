// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// ClientOptions tunes a Client. Zero values take the defaults.
type ClientOptions struct {
	Proto      int           // default ProtoVersion; tests set another to force a mismatch
	Version    string        // this rysh's build version, announced in hello
	AckTimeout time.Duration // default 5s
}

// maxBannerLines bounds the non-JSON lines tolerated before the hello. A login
// shell rc that prints to stdout puts them there; anything longer is not a
// rysh endpoint.
const maxBannerLines = 64

// Client is the local end of one remote session connection.
type Client struct {
	rwc  io.ReadWriteCloser
	fw   *frameWriter
	fr   *frameReader
	opts ClientOptions

	frames chan Frame
	done   chan struct{}

	mu      sync.Mutex
	nextID  uint64
	pending map[uint64]chan Frame
	err     error
	closed  bool
}

// NewClient wraps a connected stream. Call Handshake before anything else.
func NewClient(rwc io.ReadWriteCloser, opts ClientOptions) *Client {
	if opts.Proto == 0 {
		opts.Proto = ProtoVersion
	}
	if opts.AckTimeout <= 0 {
		opts.AckTimeout = 5 * time.Second
	}
	return &Client{
		rwc:     rwc,
		fw:      newFrameWriter(rwc),
		fr:      newFrameReader(rwc),
		opts:    opts,
		frames:  make(chan Frame, 1024),
		done:    make(chan struct{}),
		pending: make(map[uint64]chan Frame),
	}
}

// Handshake sends this side's hello and waits for the server's. A protocol
// mismatch — refused by the server, or detected here — returns
// ErrProtoMismatch and closes the connection.
func (c *Client) Handshake(ctx context.Context) (Hello, error) {
	type result struct {
		h   Hello
		err error
	}
	res := make(chan result, 1)
	go func() {
		if err := c.fw.write(Frame{T: FrameHello, Proto: c.opts.Proto, Version: c.opts.Version}); err != nil {
			res <- result{err: fmt.Errorf("send hello: %w", err)}
			return
		}
		for banner := 0; ; {
			line, err := c.fr.next()
			if err != nil {
				if errors.Is(err, io.EOF) {
					err = ErrClosed
				}
				res <- result{err: fmt.Errorf("waiting for remote hello: %w", err)}
				return
			}
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) == 0 || trimmed[0] != '{' {
				if banner++; banner > maxBannerLines {
					res <- result{err: errors.New("remote did not answer like rysh remote-serve")}
					return
				}
				continue
			}
			var f Frame
			if err := json.Unmarshal(trimmed, &f); err != nil {
				res <- result{err: fmt.Errorf("bad hello from remote: %w", err)}
				return
			}
			switch f.T {
			case FrameError:
				if f.Code == CodeProtoMismatch {
					res <- result{err: mismatchError(c.opts.Proto, f.Proto, f.Version)}
					return
				}
				res <- result{err: fmt.Errorf("remote refused: %s", f.Error)}
				return
			case FrameHello:
				if f.Proto != c.opts.Proto {
					res <- result{err: mismatchError(c.opts.Proto, f.Proto, f.Version)}
					return
				}
				res <- result{h: Hello{Proto: f.Proto, Version: f.Version, Session: f.Session}}
				return
			default:
				res <- result{err: fmt.Errorf("remote sent %q before hello", f.T)}
				return
			}
		}
	}()
	select {
	case r := <-res:
		if r.err != nil {
			c.fail(r.err)
			close(c.frames) // the read loop never started
			return Hello{}, r.err
		}
		go c.readLoop()
		return r.h, nil
	case <-ctx.Done():
		c.fail(ctx.Err())
		close(c.frames)
		return Hello{}, ctx.Err()
	}
}

// Frames delivers layout, screen and error frames in arrival order. It is
// closed when the connection ends.
func (c *Client) Frames() <-chan Frame { return c.frames }

// Done is closed when the connection has ended; Err then says why.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err returns why the connection ended, or nil while it is up.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Close ends the connection. Pending commands fail with ErrNotDelivered.
func (c *Client) Close() error {
	c.fail(ErrClosed)
	return nil
}

// Do sends one command and waits for the server's ack. Any outcome other than
// a positive ack is an error the caller must show; ErrNotDelivered means the
// input may not have reached the remote pane.
func (c *Client) Do(ctx context.Context, cmd Command) error {
	f, err := c.roundTrip(ctx, Frame{T: FrameCmd, Cmd: cmd.Type, Pane: cmd.Pane, Payload: cmd.Payload})
	if err != nil {
		return err
	}
	if !f.OK {
		if f.Error == "" {
			f.Error = "refused"
		}
		return fmt.Errorf("remote refused %s: %s", cmd.Type, f.Error)
	}
	return nil
}

// DoAsync writes one command now and returns a channel that receives the
// outcome once the ack (or a failure) arrives. Calls made one after another
// reach the remote in that order, so a typing burst is pipelined instead of
// paying a round trip per keystroke; every keystroke is still acked.
func (c *Client) DoAsync(cmd Command) <-chan error {
	out := make(chan error, 1)
	id, ch, err := c.send(Frame{T: FrameCmd, Cmd: cmd.Type, Pane: cmd.Pane, Payload: cmd.Payload})
	if err != nil {
		out <- err
		return out
	}
	go func() {
		f, err := c.await(context.Background(), id, ch)
		if err == nil && !f.OK {
			if f.Error == "" {
				f.Error = "refused"
			}
			err = fmt.Errorf("remote refused %s: %s", cmd.Type, f.Error)
		}
		out <- err
	}()
	return out
}

// Ping round-trips a ping; used as a keep-alive and liveness probe.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.roundTrip(ctx, Frame{T: FramePing})
	return err
}

func (c *Client) roundTrip(ctx context.Context, f Frame) (Frame, error) {
	id, ch, err := c.send(f)
	if err != nil {
		return Frame{}, err
	}
	return c.await(ctx, id, ch)
}

// send registers a pending reply and writes the frame.
func (c *Client) send(f Frame) (uint64, chan Frame, error) {
	c.mu.Lock()
	if c.closed {
		err := c.err
		c.mu.Unlock()
		return 0, nil, fmt.Errorf("%w: %v", ErrNotDelivered, err)
	}
	c.nextID++
	f.ID = c.nextID
	ch := make(chan Frame, 1)
	c.pending[f.ID] = ch
	c.mu.Unlock()
	if err := c.fw.write(f); err != nil {
		c.forget(f.ID)
		c.fail(err)
		return 0, nil, fmt.Errorf("%w: %v", ErrNotDelivered, err)
	}
	return f.ID, ch, nil
}

// await waits for the reply to id.
func (c *Client) await(ctx context.Context, id uint64, ch chan Frame) (Frame, error) {
	defer c.forget(id)
	t := time.NewTimer(c.opts.AckTimeout)
	defer t.Stop()
	select {
	case r := <-ch:
		return r, nil
	case <-c.done:
		// An ack may have raced the close.
		select {
		case r := <-ch:
			return r, nil
		default:
		}
		return Frame{}, fmt.Errorf("%w: %v", ErrNotDelivered, c.Err())
	case <-t.C:
		return Frame{}, fmt.Errorf("%w: no ack within %s", ErrNotDelivered, c.opts.AckTimeout)
	case <-ctx.Done():
		return Frame{}, fmt.Errorf("%w: %v", ErrNotDelivered, ctx.Err())
	}
}

func (c *Client) forget(id uint64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *Client) readLoop() {
	defer close(c.frames)
	for {
		line, err := c.fr.next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = ErrClosed
			}
			c.fail(err)
			return
		}
		var f Frame
		if err := json.Unmarshal(line, &f); err != nil {
			c.fail(fmt.Errorf("bad frame from remote: %w", err))
			return
		}
		switch f.T {
		case FrameAck, FramePong:
			c.mu.Lock()
			ch := c.pending[f.ID]
			c.mu.Unlock()
			if ch != nil {
				ch <- f
			}
		default:
			select {
			case c.frames <- f:
			case <-c.done:
				return
			}
		}
	}
}

// fail records the first terminal error and tears the connection down.
func (c *Client) fail(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.err = err
	c.mu.Unlock()
	close(c.done)
	_ = c.rwc.Close()
}
