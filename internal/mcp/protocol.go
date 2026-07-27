// Package mcp implements a Model Context Protocol (MCP) client for the rysh CLI.
//
// It lets rysh agents/humanoids consume tools exposed by external MCP servers
// over two transports:
//
//   - stdio: a child process speaking newline-delimited JSON-RPC on stdin/stdout.
//   - Streamable HTTP: a long-running HTTP service whose /mcp endpoint accepts
//     JSON-RPC POSTs and replies with application/json or text/event-stream.
//
// Discovered tools are adapted to the shared tools.ToolExecutor interface (see
// executor.go) and registered into the agent tool registry by the Manager (see
// manager.go), so they flow through the existing Anthropic tool-use bridge,
// approval gates, and audit path unchanged.
//
// The wire types mirror the in-repo reference servers under rysh-mcp-samples
// (mcp-server = stdio, mcp-rest-api-wrapper = Streamable HTTP).
package mcp

import (
	"encoding/json"
	"fmt"
)

// ProtocolVersion is the MCP revision this client advertises during the
// initialize handshake. It matches the version spoken by the reference servers
// in rysh-mcp-samples.
const ProtocolVersion = "2024-11-05"

// jsonrpcRequest is an outgoing JSON-RPC 2.0 request or notification. A nil/empty
// ID marks the message as a notification (no response is expected).
type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// jsonrpcMessage is a permissive inbound envelope. A server→client message is a
// response (has id + result/error), a notification (has method, no id), or a
// server-initiated request (has method + id); the reader distinguishes them by
// inspecting Method and ID.
type jsonrpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError is a JSON-RPC error object. It implements error.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	if e == nil {
		return "<nil rpc error>"
	}
	if len(e.Data) > 0 {
		return fmt.Sprintf("rpc error %d: %s (%s)", e.Code, e.Message, string(e.Data))
	}
	return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message)
}

// implementation identifies a client or server in the initialize handshake.
type implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// initializeParams is the body of the "initialize" request.
type initializeParams struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    clientCapabilities `json:"capabilities"`
	ClientInfo      implementation     `json:"clientInfo"`
}

// clientCapabilities advertises optional client features. rysh consumes tools
// only, so this is intentionally empty for Phase 0.
type clientCapabilities struct{}

// initializeResult is the server's reply to "initialize".
type initializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    serverCapabilities `json:"capabilities"`
	ServerInfo      implementation     `json:"serverInfo"`
}

type serverCapabilities struct {
	Tools *toolsCapability `json:"tools,omitempty"`
}

type toolsCapability struct {
	// ListChanged indicates the server will emit notifications/tools/list_changed
	// when its tool set changes. rysh re-lists on that notification (stdio only).
	ListChanged bool `json:"listChanged"`
}

// Tool is a tool advertised by an MCP server. InputSchema is kept as raw JSON so
// it can be handed to tools.ToolSpec.Parameters (a JSON Schema) without a lossy
// round-trip through a typed struct.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// toolsListParams carries the optional pagination cursor for "tools/list".
type toolsListParams struct {
	Cursor string `json:"cursor,omitempty"`
}

// toolsListResult is the server's reply to "tools/list". NextCursor, when
// non-empty, means more tools are available and the client should page again.
type toolsListResult struct {
	Tools      []Tool `json:"tools"`
	NextCursor string `json:"nextCursor,omitempty"`
}

// callToolParams is the body of the "tools/call" request.
type callToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// callToolResult is the server's reply to "tools/call".
type callToolResult struct {
	Content []contentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"`
}

// contentBlock is one element of a tool result. Phase 0 renders text natively
// and summarizes non-text blocks (image/audio/resource) rather than dropping
// them silently.
type contentBlock struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	Data     string          `json:"data,omitempty"`
	MIMEType string          `json:"mimeType,omitempty"`
	Resource json.RawMessage `json:"resource,omitempty"`
}
