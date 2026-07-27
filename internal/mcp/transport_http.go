package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// httpMaxBody bounds a single Streamable-HTTP response body.
const httpMaxBody = 16 << 20

// httpTransport implements the MCP "Streamable HTTP" transport: each JSON-RPC
// message is POSTed to the server's endpoint, and the reply arrives either as a
// single application/json body or as a text/event-stream (SSE) containing one
// JSON-RPC message. The server may assign an Mcp-Session-Id on initialize, which
// the client echoes on every subsequent request.
//
// Phase 0 does not open a long-lived GET stream, so server→client notifications
// are not delivered over HTTP (onNotification is stored but never invoked).
type httpTransport struct {
	name    string
	url     string
	headers map[string]string
	client  *http.Client

	mu        sync.Mutex
	sessionID string
	notifyFn  func(method string, params json.RawMessage)
}

func newHTTPTransport(name, url string, headers map[string]string, client *http.Client) *httpTransport {
	return &httpTransport{
		name:    name,
		url:     url,
		headers: headers,
		client:  client,
	}
}

func (t *httpTransport) onNotification(fn func(method string, params json.RawMessage)) {
	t.mu.Lock()
	t.notifyFn = fn
	t.mu.Unlock()
}

func (t *httpTransport) roundTrip(ctx context.Context, req jsonrpcRequest) (*jsonrpcMessage, error) {
	req.JSONRPC = "2.0"
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	resp, ct, err := t.post(ctx, body)
	if err != nil {
		return nil, err
	}

	msg, err := parseHTTPResult(ct, resp, string(req.ID))
	if err != nil {
		return nil, err
	}
	if msg == nil {
		return nil, fmt.Errorf("mcp server %q returned no response for %s", t.name, req.Method)
	}
	if msg.Error != nil {
		return msg, msg.Error
	}
	return msg, nil
}

func (t *httpTransport) notify(ctx context.Context, req jsonrpcRequest) error {
	req.JSONRPC = "2.0"
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal notification: %w", err)
	}
	resp, _, err := t.post(ctx, body)
	_ = resp // notifications expect 202/200 with empty/ignored body
	return err
}

// post sends one JSON-RPC payload and returns the raw response body plus its
// Content-Type. It captures the Mcp-Session-Id header for session continuity.
func (t *httpTransport) post(ctx context.Context, body []byte) ([]byte, string, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// Streamable HTTP requires the client to accept both response framings.
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range t.headers {
		httpReq.Header.Set(k, v)
	}
	t.mu.Lock()
	if t.sessionID != "" {
		httpReq.Header.Set("Mcp-Session-Id", t.sessionID)
	}
	t.mu.Unlock()

	resp, err := t.client.Do(httpReq)
	if err != nil {
		return nil, "", fmt.Errorf("reach %q: %w", t.name, err)
	}
	defer resp.Body.Close()

	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		t.mu.Lock()
		t.sessionID = sid
		t.mu.Unlock()
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, httpMaxBody))
	if err != nil {
		return nil, "", fmt.Errorf("read %q response: %w", t.name, err)
	}

	if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNoContent {
		return nil, resp.Header.Get("Content-Type"), nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("mcp server %q returned HTTP %d: %s", t.name, resp.StatusCode, truncate(string(data), 512))
	}
	return data, resp.Header.Get("Content-Type"), nil
}

func (t *httpTransport) close() error {
	t.client.CloseIdleConnections()
	return nil
}

// parseHTTPResult decodes a JSON-RPC response from either an application/json
// body or an SSE (text/event-stream) body. For SSE it returns the data payload
// whose id matches wantID, falling back to the first JSON-RPC message found.
func parseHTTPResult(contentType string, body []byte, wantID string) (*jsonrpcMessage, error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil, nil
	}

	if strings.Contains(contentType, "text/event-stream") {
		return parseSSE(body, wantID)
	}

	// Default: a single JSON object, or a JSON-RPC batch array.
	if body[0] == '[' {
		var batch []jsonrpcMessage
		if err := json.Unmarshal(body, &batch); err != nil {
			return nil, fmt.Errorf("parse JSON-RPC batch: %w", err)
		}
		return pickMessage(batch, wantID), nil
	}
	var m jsonrpcMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("parse JSON-RPC response: %w", err)
	}
	return &m, nil
}

// parseSSE extracts JSON-RPC messages from Server-Sent Events frames. Each event
// accumulates one or more "data:" lines; a blank line terminates the event.
func parseSSE(body []byte, wantID string) (*jsonrpcMessage, error) {
	var msgs []jsonrpcMessage
	var data strings.Builder
	flush := func() {
		payload := strings.TrimSpace(data.String())
		data.Reset()
		if payload == "" {
			return
		}
		var m jsonrpcMessage
		if err := json.Unmarshal([]byte(payload), &m); err == nil {
			msgs = append(msgs, m)
		}
	}

	for _, raw := range strings.Split(string(body), "\n") {
		line := strings.TrimRight(raw, "\r")
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		case strings.HasPrefix(line, ":"):
			// SSE comment / keep-alive; ignore.
		}
	}
	flush()

	if len(msgs) == 0 {
		return nil, fmt.Errorf("no JSON-RPC message in event stream")
	}
	return pickMessage(msgs, wantID), nil
}

// pickMessage selects the response matching wantID (a non-notification), else
// the first message.
func pickMessage(msgs []jsonrpcMessage, wantID string) *jsonrpcMessage {
	for i := range msgs {
		if string(msgs[i].ID) == wantID && msgs[i].Method == "" {
			return &msgs[i]
		}
	}
	for i := range msgs {
		if msgs[i].Method == "" {
			return &msgs[i]
		}
	}
	return &msgs[0]
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
