// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"context"
	"encoding/json"
)

// transport is a low-level JSON-RPC channel to a single MCP server. It is the
// only place that differs between stdio and Streamable HTTP; everything above
// (Client, executor, Manager) is transport-agnostic.
type transport interface {
	// roundTrip sends a request that carries an ID and returns the matching
	// response. It must honor ctx cancellation (a new prompt cancels in-flight
	// tool calls under rysh's "last-prompt-wins" semantics).
	roundTrip(ctx context.Context, req jsonrpcRequest) (*jsonrpcMessage, error)

	// notify sends a fire-and-forget notification (no ID, no response).
	notify(ctx context.Context, req jsonrpcRequest) error

	// onNotification registers a handler invoked for server→client
	// notifications (e.g. notifications/tools/list_changed). Transports without
	// a server→client stream (stateless HTTP) may never call it.
	onNotification(func(method string, params json.RawMessage))

	// close releases the transport: kills the child process (stdio) or closes
	// idle connections (http).
	close() error
}

// notificationToolsListChanged is the method name MCP servers use to tell the
// client their advertised tool set has changed.
const notificationToolsListChanged = "notifications/tools/list_changed"

// notificationInitialized is sent by the client after a successful initialize.
const notificationInitialized = "notifications/initialized"
