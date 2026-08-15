// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	sharedtools "github.com/rysh-ai/rysh-cli-shared/tools"
)

// toolExecutor adapts a single MCP tool to the shared tools.ToolExecutor
// interface so it registers into the same ToolRegistry as native rysh tools and
// flows through the Anthropic tool-use bridge, approval gates, and audit path.
type toolExecutor struct {
	client     *Client
	manager    *Manager // optional; when set, transport failures trigger MarkUnhealthy (follow-up item 6)
	serverName string   // MCP server alias (for diagnostics)
	remoteName string   // tool name as known to the server (used in tools/call)
	spec       sharedtools.ToolSpec
	approval   bool
}

// Spec returns the registry-facing tool definition (prefixed name + JSON Schema).
func (e *toolExecutor) Spec() sharedtools.ToolSpec { return e.spec }

// RequiresApproval reflects the owning server's configured approval policy.
func (e *toolExecutor) RequiresApproval(_ json.RawMessage) bool { return e.approval }

// Execute forwards the call to the MCP server and renders the result. A
// transport/connection failure is returned as a Go error (surfaced as "tool
// failed"); a tool that itself reports failure (isError) is returned as a
// ToolOutput.Error so the model can read and recover from it.
func (e *toolExecutor) Execute(ctx context.Context, params json.RawMessage) (*sharedtools.ToolOutput, error) {
	res, err := e.client.CallTool(ctx, e.remoteName, params)
	if err != nil {
		// Follow-up item 6: tell the manager so it can attempt an async
		// reconnect with bounded backoff. The model still sees the call as
		// failed for this turn (a transient kind on this turn; if the
		// reconnect succeeds, the next turn's call lands on the new
		// transport).
		if e.manager != nil {
			e.manager.MarkUnhealthy(e.serverName, err)
		}
		return sharedtools.ErrOutputf(sharedtools.ErrKindTransient,
			"mcp %s/%s: %v (auto-restart pending)", e.serverName, e.remoteName, err), nil
	}
	text := renderContent(res.Content)
	if res.IsError {
		if text == "" {
			text = "tool reported an error"
		}
		return &sharedtools.ToolOutput{Error: text}, nil
	}
	return &sharedtools.ToolOutput{
		Content:  text,
		Metadata: map[string]string{"mcp_server": e.serverName, "mcp_tool": e.remoteName},
	}, nil
}

// renderContent flattens MCP content blocks into a single string. Text is used
// verbatim; non-text blocks are summarized rather than dropped silently.
func renderContent(blocks []contentBlock) string {
	var sb strings.Builder
	for _, b := range blocks {
		switch {
		case b.Type == "text" || (b.Text != "" && b.Type == ""):
			sb.WriteString(b.Text)
		case b.Type == "image":
			fmt.Fprintf(&sb, "[image content: %s, %d bytes base64]", orUnknown(b.MIMEType), len(b.Data))
		case b.Type == "audio":
			fmt.Fprintf(&sb, "[audio content: %s, %d bytes base64]", orUnknown(b.MIMEType), len(b.Data))
		case b.Type == "resource" || len(b.Resource) > 0:
			fmt.Fprintf(&sb, "[resource: %s]", strings.TrimSpace(string(b.Resource)))
		case b.Text != "":
			sb.WriteString(b.Text)
		default:
			fmt.Fprintf(&sb, "[%s content]", orUnknown(b.Type))
		}
		sb.WriteString("\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
