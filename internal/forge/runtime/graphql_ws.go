// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gorilla/websocket"
)

// GraphQL subscriptions ride the graphql-transport-ws protocol (the modern
// "graphql-ws" library protocol): connection_init → connection_ack, then
// subscribe → next*/error/complete, with ping answered by pong. The legacy
// subscriptions-transport-ws protocol (start/data/stop) is NOT supported.
//
// The websocket URL is derived from the executor's HTTP endpoint by scheme
// convention (http→ws, https→wss) unless SetWSEndpoint provides an explicit
// override (##forge add --ws-url) for servers that mount the subscription
// socket elsewhere.

// graphql-transport-ws message types.
const (
	gqlWSConnectionInit = "connection_init"
	gqlWSConnectionAck  = "connection_ack"
	gqlWSPing           = "ping"
	gqlWSPong           = "pong"
	gqlWSSubscribe      = "subscribe"
	gqlWSNext           = "next"
	gqlWSError          = "error"
	gqlWSComplete       = "complete"
)

type gqlWSMessage struct {
	ID      string          `json:"id,omitempty"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// SetWSEndpoint overrides the derived websocket URL for subscriptions.
func (e *GraphQLExecutor) SetWSEndpoint(u string) { e.wsEndpoint = u }

// WSEndpoint returns the websocket URL subscriptions will dial: the explicit
// override when set, else the HTTP endpoint with its scheme swapped per
// convention (http→ws, https→wss).
func (e *GraphQLExecutor) WSEndpoint() string {
	if e.wsEndpoint != "" {
		return e.wsEndpoint
	}
	return DeriveWSURL(e.endpoint)
}

// DeriveWSURL maps an HTTP(S) URL to its WS(S) twin. Non-http schemes are
// returned unchanged (already ws://, or malformed — the dial will say so).
func DeriveWSURL(httpURL string) string {
	switch {
	case strings.HasPrefix(httpURL, "https://"):
		return "wss://" + strings.TrimPrefix(httpURL, "https://")
	case strings.HasPrefix(httpURL, "http://"):
		return "ws://" + strings.TrimPrefix(httpURL, "http://")
	default:
		return httpURL
	}
}

// Subscribe starts a GraphQL subscription as a streaming session (design 015
// §2.1): it dials the websocket, performs the connection_init/ack handshake,
// sends one subscribe, and pumps "next" payloads into the session's ring
// buffer until complete / error / cancellation. Frames are redacted with
// Options.Redact before entering the ring. The returned session id is what the
// model polls/stops with.
func (e *GraphQLExecutor) Subscribe(sm *StreamManager, query string, variables map[string]any) (string, error) {
	if strings.TrimSpace(query) == "" {
		return "", fmt.Errorf("graphql subscription query is required")
	}
	wsURL := e.WSEndpoint()
	if !strings.Contains(wsURL, "://") {
		return "", fmt.Errorf("no absolute websocket URL for subscriptions (endpoint %q)", e.endpoint)
	}

	// Resolve auth into headers the same way the HTTP path does: InjectAuth on
	// a throwaway request, plus the bearer provider and extra headers.
	hdr := http.Header{}
	fake, _ := http.NewRequest(http.MethodGet, e.endpoint, nil)
	if fake != nil {
		InjectAuth(fake, e.auth, e.cred)
		for k, vs := range fake.Header {
			hdr[k] = vs
		}
	}
	if e.opts.TokenProvider != nil {
		if tok, err := e.opts.TokenProvider(context.Background()); err == nil && tok != "" {
			header, scheme := e.opts.AuthHeader, e.opts.AuthScheme
			if header == "" {
				header = "Authorization"
			}
			if scheme == "" {
				scheme = "Bearer"
			}
			hdr.Set(header, scheme+" "+tok)
		}
	}
	for k, v := range e.opts.ExtraHeaders {
		hdr.Set(k, v)
	}

	label := subscriptionLabel(query)
	handshakeTimeout := e.opts.Timeout

	return sm.Start("graphql-subscription", label, func(ctx context.Context, push func(string)) error {
		dialer := websocket.Dialer{
			Subprotocols:     []string{"graphql-transport-ws"},
			HandshakeTimeout: handshakeTimeout,
		}
		conn, resp, err := dialer.DialContext(ctx, wsURL, hdr)
		if err != nil {
			if resp != nil {
				return fmt.Errorf("dial %s: %w (HTTP %d)", wsURL, err, resp.StatusCode)
			}
			return fmt.Errorf("dial %s: %w", wsURL, err)
		}
		defer conn.Close()

		// Cancellation seam: closing the conn unblocks any pending ReadJSON, so
		// Stop/expiry/CloseAll tear the socket down promptly.
		done := make(chan struct{})
		defer close(done)
		go func() {
			select {
			case <-ctx.Done():
				conn.Close()
			case <-done:
			}
		}()

		if err := conn.WriteJSON(gqlWSMessage{Type: gqlWSConnectionInit, Payload: json.RawMessage("{}")}); err != nil {
			return fmt.Errorf("connection_init: %w", err)
		}
		// Wait for the ack (answering pings) before subscribing.
		for {
			var m gqlWSMessage
			if err := conn.ReadJSON(&m); err != nil {
				return wsErr(ctx, fmt.Errorf("waiting for connection_ack: %w", err))
			}
			if m.Type == gqlWSPing {
				_ = conn.WriteJSON(gqlWSMessage{Type: gqlWSPong})
				continue
			}
			if m.Type == gqlWSConnectionAck {
				break
			}
			return fmt.Errorf("expected connection_ack, got %q", m.Type)
		}

		payload := map[string]any{"query": query}
		if len(variables) > 0 {
			payload["variables"] = variables
		}
		pb, _ := json.Marshal(payload)
		if err := conn.WriteJSON(gqlWSMessage{ID: "1", Type: gqlWSSubscribe, Payload: pb}); err != nil {
			return fmt.Errorf("subscribe: %w", err)
		}

		for {
			var m gqlWSMessage
			if err := conn.ReadJSON(&m); err != nil {
				return wsErr(ctx, err)
			}
			switch m.Type {
			case gqlWSNext:
				text := prettyJSON(m.Payload)
				if e.opts.Redact != nil {
					text = e.opts.Redact(text)
				}
				push(text)
			case gqlWSPing:
				_ = conn.WriteJSON(gqlWSMessage{Type: gqlWSPong})
			case gqlWSError:
				return fmt.Errorf("subscription error: %s", truncate(compactJSON(m.Payload), 1024))
			case gqlWSComplete:
				return nil // server completed the subscription
			}
		}
	})
}

// wsErr maps a read error after cancellation to the context error so the
// session end reason says "cancelled" instead of "use of closed connection".
func wsErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
		return nil
	}
	return err
}

// subscriptionLabel derives a short listing label from the document: the first
// field name after the subscription root, falling back to a trimmed prefix.
func subscriptionLabel(query string) string {
	s := strings.TrimSpace(query)
	if i := strings.IndexByte(s, '{'); i >= 0 {
		rest := strings.TrimSpace(s[i+1:])
		end := len(rest)
		for j, r := range rest {
			if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
				end = j
				break
			}
		}
		if end > 0 {
			return rest[:end]
		}
	}
	return truncate(s, 32)
}
