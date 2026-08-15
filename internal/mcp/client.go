// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
)

// clientVersion is advertised to servers in the initialize handshake.
const clientVersion = "0.1.0"

// maxToolPages caps tools/list pagination to avoid an unbounded loop against a
// misbehaving server that always returns a nextCursor.
const maxToolPages = 1000

// Client is a protocol-level MCP client over a single transport. It performs the
// initialize handshake, lists tools (with pagination), and calls tools. It is
// safe for concurrent use.
type Client struct {
	name string
	t    transport

	mu         sync.Mutex
	nextID     int64
	serverInfo implementation
	caps       serverCapabilities
	onChanged  func()
}

func newClient(name string, t transport) *Client {
	c := &Client{name: name, t: t}
	t.onNotification(c.handleNotification)
	return c
}

// SetToolsChangedHandler registers a callback invoked when the server announces
// notifications/tools/list_changed (stdio transport only).
func (c *Client) SetToolsChangedHandler(fn func()) {
	c.mu.Lock()
	c.onChanged = fn
	c.mu.Unlock()
}

func (c *Client) handleNotification(method string, _ json.RawMessage) {
	if method != notificationToolsListChanged {
		return
	}
	c.mu.Lock()
	fn := c.onChanged
	c.mu.Unlock()
	if fn != nil {
		fn()
	}
}

func (c *Client) id() json.RawMessage {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.mu.Unlock()
	return json.RawMessage(strconv.FormatInt(id, 10))
}

// Initialize performs the MCP handshake: it sends "initialize" and, on success,
// the "notifications/initialized" acknowledgement.
func (c *Client) Initialize(ctx context.Context) error {
	params := initializeParams{
		ProtocolVersion: ProtocolVersion,
		Capabilities:    clientCapabilities{},
		ClientInfo:      implementation{Name: "rysh", Version: clientVersion},
	}
	raw, _ := json.Marshal(params)
	resp, err := c.t.roundTrip(ctx, jsonrpcRequest{ID: c.id(), Method: "initialize", Params: raw})
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	var res initializeResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		return fmt.Errorf("initialize result: %w", err)
	}
	c.mu.Lock()
	c.serverInfo = res.ServerInfo
	c.caps = res.Capabilities
	c.mu.Unlock()

	// Best-effort acknowledgement; servers tolerate its absence but expect it.
	_ = c.t.notify(ctx, jsonrpcRequest{Method: notificationInitialized})
	return nil
}

// ServerInfo returns the name/version reported by the server (valid after
// Initialize).
func (c *Client) ServerInfo() (name, version string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.serverInfo.Name, c.serverInfo.Version
}

// ListTools returns every tool the server advertises, following nextCursor
// pagination so large catalogs are fully enumerated.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	var all []Tool
	cursor := ""
	for page := 0; page < maxToolPages; page++ {
		raw, _ := json.Marshal(toolsListParams{Cursor: cursor})
		resp, err := c.t.roundTrip(ctx, jsonrpcRequest{ID: c.id(), Method: "tools/list", Params: raw})
		if err != nil {
			return nil, fmt.Errorf("tools/list: %w", err)
		}
		var res toolsListResult
		if err := json.Unmarshal(resp.Result, &res); err != nil {
			return nil, fmt.Errorf("tools/list result: %w", err)
		}
		all = append(all, res.Tools...)
		if res.NextCursor == "" {
			return all, nil
		}
		cursor = res.NextCursor
	}
	return all, fmt.Errorf("tools/list exceeded %d pages (possible cursor loop)", maxToolPages)
}

// CallTool invokes a tool by its server-side name with raw JSON arguments.
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (*callToolResult, error) {
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	raw, _ := json.Marshal(callToolParams{Name: name, Arguments: args})
	resp, err := c.t.roundTrip(ctx, jsonrpcRequest{ID: c.id(), Method: "tools/call", Params: raw})
	if err != nil {
		return nil, err
	}
	var res callToolResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		return nil, fmt.Errorf("tools/call result: %w", err)
	}
	return &res, nil
}

// Close shuts down the underlying transport.
func (c *Client) Close() error { return c.t.close() }
