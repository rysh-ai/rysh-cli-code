// SPDX-License-Identifier: Apache-2.0

package plugin

// stdioTransport — newline-delimited JSON-RPC 2.0 over the plugin process's
// stdin/stdout (design 002 §4.1 fallback transport). Core issues
// start/stop/send/status as id-matched requests; the plugin pushes
// ready/inbound/status notifications on the same stream; setReplyMode rides
// as a core→plugin notification. Reader goroutine + pending-map pattern,
// mirroring SignalAdapter (internal/channels/signal.go).

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

const (
	// pluginRPCTimeout bounds how long a request waits for the plugin's reply
	// when the caller's context has no earlier deadline.
	pluginRPCTimeout = 10 * time.Second
	// pluginMaxLine caps a single JSON-RPC line from the plugin.
	pluginMaxLine = 1024 * 1024
)

// Wire method names (plugin → core notifications).
const (
	methodReady   = "ready"
	methodInbound = "inbound"
	methodStatus  = "status"
)

// pluginRPCRequest is an outgoing JSON-RPC 2.0 line (ID empty ⇒ notification).
type pluginRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	ID      string          `json:"id,omitempty"`
}

// pluginRPCError is a JSON-RPC 2.0 error object.
type pluginRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// pluginRPCFrame is any incoming line: a response (id + result/error) or a
// notification (method + params).
type pluginRPCFrame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *pluginRPCError `json:"error,omitempty"`
}

// stringID normalizes the frame's id to a string (core issues string ids).
func (f *pluginRPCFrame) stringID() (string, bool) {
	raw := bytes.TrimSpace(f.ID)
	if len(raw) == 0 || string(raw) == "null" {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, true
	}
	return string(raw), true
}

type stdioTransport struct {
	name  string
	stdin io.WriteCloser
	h     transportHandlers

	writeMu sync.Mutex

	pendingMu sync.Mutex
	pending   map[string]chan pluginRPCFrame
	nextID    uint64

	closed    chan struct{}
	closeOnce sync.Once
}

var _ pluginTransport = (*stdioTransport)(nil)

// newStdioTransport wires the plugin's pipes and starts the reader goroutine.
func newStdioTransport(name string, stdin io.WriteCloser, stdout io.Reader, h transportHandlers) *stdioTransport {
	t := &stdioTransport{
		name:    name,
		stdin:   stdin,
		h:       h,
		pending: make(map[string]chan pluginRPCFrame),
		closed:  make(chan struct{}),
	}
	go t.readLoop(stdout)
	return t
}

// readLoop decodes newline-delimited frames from the plugin's stdout until
// the pipe closes (process exit or close()).
func (t *stdioTransport) readLoop(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), pluginMaxLine)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var frame pluginRPCFrame
		if err := json.Unmarshal(line, &frame); err != nil {
			slog.Warn("plugin: dropping unparseable stdio line", "plugin", t.name, "err", err)
			continue
		}
		t.dispatch(frame)
	}
	if err := scanner.Err(); err != nil {
		slog.Debug("plugin: stdio reader ended", "plugin", t.name, "err", err)
	}
	t.failPending()
}

// dispatch routes a frame: notifications (ready/inbound/status) to handlers,
// id-bearing responses to the pending map.
func (t *stdioTransport) dispatch(frame pluginRPCFrame) {
	switch frame.Method {
	case methodReady:
		if t.h.onReady != nil {
			t.h.onReady()
		}
		return
	case methodInbound:
		var m channels.InboundMessage
		if err := json.Unmarshal(frame.Params, &m); err != nil {
			slog.Warn("plugin: bad inbound notification", "plugin", t.name, "err", err)
			return
		}
		if t.h.onInbound != nil {
			t.h.onInbound(m)
		}
		return
	case methodStatus:
		var st msg.ChannelStatus
		if err := json.Unmarshal(frame.Params, &st); err != nil {
			slog.Warn("plugin: bad status notification", "plugin", t.name, "err", err)
			return
		}
		if t.h.onStatus != nil {
			t.h.onStatus(st)
		}
		return
	}
	if id, ok := frame.stringID(); ok {
		t.pendingMu.Lock()
		ch := t.pending[id]
		delete(t.pending, id)
		t.pendingMu.Unlock()
		if ch != nil {
			ch <- frame // buffered(1); receiver is the sole waiter
		} else {
			slog.Debug("plugin: response for unknown/expired request id", "plugin", t.name, "id", id)
		}
		return
	}
	slog.Debug("plugin: ignoring notification", "plugin", t.name, "method", frame.Method)
}

// failPending fails all in-flight RPCs (pipe lost); waiters observe the
// closed channel and surface a connection-lost error.
func (t *stdioTransport) failPending() {
	t.pendingMu.Lock()
	for id, ch := range t.pending {
		close(ch)
		delete(t.pending, id)
	}
	t.pendingMu.Unlock()
}

// writeFrame serializes one line to the plugin's stdin.
func (t *stdioTransport) writeFrame(req pluginRPCRequest) error {
	line, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("plugin stdio: marshal %s: %w", req.Method, err)
	}
	line = append(line, '\n')
	t.writeMu.Lock()
	_, werr := t.stdin.Write(line)
	t.writeMu.Unlock()
	if werr != nil {
		return fmt.Errorf("plugin stdio: write %s: %w", req.Method, werr)
	}
	return nil
}

// request implements pluginTransport.
func (t *stdioTransport) request(ctx context.Context, method string, params, result any) error {
	select {
	case <-t.closed:
		return fmt.Errorf("plugin stdio: transport closed")
	default:
	}

	t.pendingMu.Lock()
	t.nextID++
	id := fmt.Sprintf("rysh-%d", t.nextID)
	ch := make(chan pluginRPCFrame, 1)
	t.pending[id] = ch
	t.pendingMu.Unlock()
	defer func() {
		t.pendingMu.Lock()
		delete(t.pending, id)
		t.pendingMu.Unlock()
	}()

	req := pluginRPCRequest{JSONRPC: "2.0", Method: method, ID: id}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("plugin stdio: marshal %s params: %w", method, err)
		}
		req.Params = raw
	}
	if err := t.writeFrame(req); err != nil {
		return err
	}

	timer := time.NewTimer(pluginRPCTimeout)
	defer timer.Stop()
	select {
	case resp, ok := <-ch:
		if !ok {
			return fmt.Errorf("plugin stdio: connection lost awaiting %s response", method)
		}
		if resp.Error != nil {
			return fmt.Errorf("plugin: %s rejected: %s (code %d)", method, resp.Error.Message, resp.Error.Code)
		}
		if result != nil && len(resp.Result) > 0 {
			if err := json.Unmarshal(resp.Result, result); err != nil {
				return fmt.Errorf("plugin stdio: decode %s result: %w", method, err)
			}
		}
		return nil
	case <-timer.C:
		return fmt.Errorf("plugin stdio: %s response timeout after %s", method, pluginRPCTimeout)
	case <-ctx.Done():
		return ctx.Err()
	case <-t.closed:
		return fmt.Errorf("plugin stdio: transport closed awaiting %s response", method)
	}
}

// notify implements pluginTransport (JSON-RPC notification: no id, no reply).
func (t *stdioTransport) notify(method string, params any) error {
	req := pluginRPCRequest{JSONRPC: "2.0", Method: method}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("plugin stdio: marshal %s params: %w", method, err)
		}
		req.Params = raw
	}
	return t.writeFrame(req)
}

// close implements pluginTransport: closes the plugin's stdin (EOF signals
// shutdown to well-behaved plugins) and fails in-flight requests.
func (t *stdioTransport) close() error {
	t.closeOnce.Do(func() {
		close(t.closed)
		_ = t.stdin.Close()
		t.failPending()
	})
	return nil
}
