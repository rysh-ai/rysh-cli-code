// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// PaneInspectTool reads another pane's output buffer or status.
type PaneInspectTool struct {
	nc      *nats.Conn
	session string
}

// PaneInspectParams holds the parameters for inspecting a pane.
type PaneInspectParams struct {
	PaneID    string `json:"pane_id"`              // target pane ID
	TailLines int    `json:"tail_lines,omitempty"` // number of lines from the end (default 50)
}

// NewPaneInspectTool creates a PaneInspectTool.
func NewPaneInspectTool(nc *nats.Conn, session string) *PaneInspectTool {
	return &PaneInspectTool{nc: nc, session: session}
}

// Spec returns the tool specification for the LLM.
func (t *PaneInspectTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {
    "pane_id": {"type": "string", "description": "The UUID of the pane to inspect"},
    "tail_lines": {"type": "integer", "description": "Number of lines to read from the end of the output buffer (default 50, max 500)"}
  },
  "required": ["pane_id"]
}`)
	return ToolSpec{
		Name:             "pane_inspect",
		Description:      "Read another pane's output buffer and status. Enables cross-pane awareness for multi-agent coordination.",
		Parameters:       schema,
		RequiresApproval: false,
	}
}

// RequiresApproval always returns false — read-only.
func (t *PaneInspectTool) RequiresApproval(_ json.RawMessage) bool {
	return false
}

// Execute requests a snapshot from the target pane via NATS.
func (t *PaneInspectTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p PaneInspectParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid pane_inspect params: %w", err)
	}

	if p.PaneID == "" {
		return ErrOutput(ErrKindValidation, "pane_id is required"), nil
	}

	tailLines := p.TailLines
	if tailLines <= 0 {
		tailLines = 50
	}
	if tailLines > 500 {
		tailLines = 500
	}

	if t.nc == nil {
		return ErrOutput(ErrKindMissing, "NATS connection not available"), nil
	}

	// Request pane snapshot via NATS request/reply.
	subject := msg.T("pane", p.PaneID, "snapshot")
	envelope := msg.NATSEnvelope{
		TypeTag: "MsgPaneSnapshot",
		Payload: []byte("{}"),
	}
	data, err := json.Marshal(envelope)
	if err != nil {
		return ErrOutputf(ErrKindInternal, "failed to marshal request: %v", err), nil
	}

	reply, err := t.nc.Request(subject, data, 5*time.Second)
	if err != nil {
		return ErrOutputf(ErrKindTransient, "pane %s not responding (may not exist): %v", p.PaneID, err), nil
	}

	// Parse the snapshot response.
	var respEnvelope msg.NATSEnvelope
	if err := json.Unmarshal(reply.Data, &respEnvelope); err != nil {
		return ErrOutputf(ErrKindValidation, "invalid response from pane: %v", err), nil
	}

	// Return the raw snapshot payload — the pane snapshot contains output, status, mode, etc.
	content := string(respEnvelope.Payload)

	return &ToolOutput{
		Content: content,
		Metadata: map[string]string{
			"pane_id":    p.PaneID,
			"tail_lines": fmt.Sprintf("%d", tailLines),
		},
	}, nil
}
