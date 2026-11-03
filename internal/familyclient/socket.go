// SPDX-License-Identifier: Apache-2.0

package familyclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// FollowOptions tunes Client.Follow. The zero value is usable.
type FollowOptions struct {
	// PingInterval is how often a ping is sent (default 25s). The server is
	// dead to us when no frame or pong arrives within 2×PingInterval+10s.
	PingInterval time.Duration
	// MinBackoff / MaxBackoff bound the reconnect delay (default 500ms / 15s).
	MinBackoff time.Duration
	MaxBackoff time.Duration
	// OnConnect is called after every successful handshake; n counts from 1.
	OnConnect func(n int)
	// OnDisconnect is called when a connection drops or a dial fails and a
	// retry is scheduled after `retry`.
	OnDisconnect func(err error, retry time.Duration)
	// Dialer overrides the websocket dialer (tests).
	Dialer *websocket.Dialer
}

// ErrSocketGone is wrapped by Follow when the server refuses the socket for
// good (404: the family is gone or not ours; 401 that a refresh cannot fix).
var ErrSocketGone = errors.New("family socket refused")

// WSURL is the socket URL for family fid. It carries no token: Follow sends
// the access token in the Authorization header of the upgrade, so it never
// lands in an access log the way a URL does (?token= is for browsers only).
func (c *Client) WSURL(fid string) (string, error) {
	if err := ValidateServer(c.base()); err != nil {
		return "", err
	}
	u, err := url.Parse(c.base())
	if err != nil {
		return "", fmt.Errorf("server url %q: %w", c.base(), err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "ws":
		u.Scheme = "ws"
	case "https", "wss":
		u.Scheme = "wss"
	default:
		return "", fmt.Errorf("server url %q: want http(s)://", c.base())
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/family/families/" + url.PathEscape(fid) + "/ws"
	return u.String(), nil
}

// Follow connects to family fid's socket and calls fn for every event until
// ctx ends, reconnecting with backoff whenever the connection drops. fn runs
// on Follow's goroutine; it must not block for long.
//
// A handshake refused with 401 triggers one token refresh and an immediate
// retry; a 401 that survives the refresh, or a 404, ends Follow with an error
// wrapping ErrSocketGone. Follow returns ctx.Err() when ctx ends.
func (c *Client) Follow(ctx context.Context, fid string, opts FollowOptions, fn func(Event)) error {
	if opts.PingInterval <= 0 {
		opts.PingInterval = 25 * time.Second
	}
	if opts.MinBackoff <= 0 {
		opts.MinBackoff = 500 * time.Millisecond
	}
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = 15 * time.Second
	}
	dialer := opts.Dialer
	if dialer == nil {
		d := *websocket.DefaultDialer
		d.HandshakeTimeout = 15 * time.Second
		dialer = &d
	}

	backoff := opts.MinBackoff
	connects := 0
	refreshedFor := "" // the token a refresh was already spent on
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		token := c.Auth.Access()
		wsURL, err := c.WSURL(fid)
		if err != nil {
			return err
		}
		hdr := http.Header{}
		if token != "" {
			hdr.Set("Authorization", "Bearer "+token)
		}
		conn, resp, err := dialer.DialContext(ctx, wsURL, hdr)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			status := 0
			if resp != nil {
				status = resp.StatusCode
				resp.Body.Close()
			}
			switch status {
			case http.StatusUnauthorized:
				if refreshedFor == token || !c.Auth.canRefresh() {
					return fmt.Errorf("%w: %v", ErrSocketGone, &APIError{Status: 401, Code: "unauthorized"})
				}
				refreshedFor = token
				if rerr := c.Refresh(ctx, token); rerr != nil {
					return fmt.Errorf("%w: HTTP 401 and the token refresh failed: %v", ErrSocketGone, rerr)
				}
				continue
			case http.StatusNotFound, http.StatusForbidden:
				return fmt.Errorf("%w: %v", ErrSocketGone, &APIError{Status: status, Code: http.StatusText(status)})
			}
			if status != 0 {
				err = fmt.Errorf("socket handshake: HTTP %d", status)
			}
			if !sleepBackoff(ctx, err, &backoff, opts) {
				return ctx.Err()
			}
			continue
		}

		connects++
		backoff = opts.MinBackoff
		refreshedFor = ""
		if opts.OnConnect != nil {
			opts.OnConnect(connects)
		}
		err = readLoop(ctx, conn, opts.PingInterval, fn)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !sleepBackoff(ctx, err, &backoff, opts) {
			return ctx.Err()
		}
	}
}

func sleepBackoff(ctx context.Context, cause error, backoff *time.Duration, opts FollowOptions) bool {
	wait := *backoff
	if opts.OnDisconnect != nil {
		opts.OnDisconnect(cause, wait)
	}
	*backoff *= 2
	if *backoff > opts.MaxBackoff {
		*backoff = opts.MaxBackoff
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// readLoop reads frames until the connection fails or ctx ends.
func readLoop(ctx context.Context, conn *websocket.Conn, ping time.Duration, fn func(Event)) error {
	defer conn.Close()
	deadline := 2*ping + 10*time.Second
	_ = conn.SetReadDeadline(time.Now().Add(deadline))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(deadline))
	})

	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(ping)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				// Unblock ReadMessage.
				_ = conn.WriteControl(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
				_ = conn.Close()
				return
			case <-t.C:
				if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)); err != nil {
					_ = conn.Close()
					return
				}
			}
		}
	}()

	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		_ = conn.SetReadDeadline(time.Now().Add(deadline))
		if mt != websocket.TextMessage {
			continue
		}
		var ev Event
		if err := json.Unmarshal(data, &ev); err != nil || ev.Type == "" {
			continue // not a frame we understand; never fatal
		}
		ev.Received = time.Now()
		fn(ev)
	}
}
