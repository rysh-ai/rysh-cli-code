package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// PaneSendTool sends a command or prompt to another pane.
type PaneSendTool struct {
	nc      *nats.Conn
	session string
}

// PaneSendParams holds the parameters for sending to a pane.
type PaneSendParams struct {
	PaneID string `json:"pane_id"`        // target pane ID
	Input  string `json:"input"`          // command or prompt text
	Mode   string `json:"mode,omitempty"` // "shell" or "prompt" (default: uses the pane's current mode)
}

// NewPaneSendTool creates a PaneSendTool.
func NewPaneSendTool(nc *nats.Conn, session string) *PaneSendTool {
	return &PaneSendTool{nc: nc, session: session}
}

// Spec returns the tool specification for the LLM.
func (t *PaneSendTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {
    "pane_id": {"type": "string", "description": "The UUID of the target pane"},
    "input": {"type": "string", "description": "The command (shell mode) or prompt (prompt mode) to send"},
    "mode": {"type": "string", "enum": ["shell", "prompt"], "description": "Input mode to use. If omitted, uses the pane's current mode."}
  },
  "required": ["pane_id", "input"]
}`)
	return ToolSpec{
		Name:             "pane_send",
		Description:      "Send a command or prompt to another pane. Enables multi-agent coordination by delegating tasks across panes.",
		Parameters:       schema,
		RequiresApproval: true,
	}
}

// RequiresApproval always returns true — sending input to other panes is a significant action.
func (t *PaneSendTool) RequiresApproval(_ json.RawMessage) bool {
	return true
}

// Execute sends input to the target pane via NATS.
func (t *PaneSendTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p PaneSendParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid pane_send params: %w", err)
	}

	if p.PaneID == "" {
		return ErrOutput(ErrKindValidation, "pane_id is required"), nil
	}
	if p.Input == "" {
		return ErrOutput(ErrKindValidation, "input is required"), nil
	}

	if t.nc == nil {
		return ErrOutput(ErrKindMissing, "NATS connection not available"), nil
	}

	// PaneActor only handles MsgPaneExecShell / MsgPaneExecPrompt directly.
	// MsgSubmitInput is handled by PaneGroupActor in the normal routing chain,
	// so we must send the correct typed message to the pane inbox.
	subject := msg.T("pane", p.PaneID, "inbox")

	var paneMsg interface{}
	var typeTag string
	switch p.Mode {
	case "prompt":
		paneMsg = &msg.MsgPaneExecPrompt{Prompt: p.Input}
		typeTag = "MsgPaneExecPrompt"
	default:
		paneMsg = &msg.MsgPaneExecShell{Command: p.Input}
		typeTag = "MsgPaneExecShell"
	}

	payloadBytes, err := json.Marshal(paneMsg)
	if err != nil {
		return ErrOutputf(ErrKindInternal, "failed to marshal payload: %v", err), nil
	}

	envelope := msg.NATSEnvelope{
		TypeTag: typeTag,
		Payload: payloadBytes,
	}
	data, err := json.Marshal(envelope)
	if err != nil {
		return ErrOutputf(ErrKindInternal, "failed to marshal envelope: %v", err), nil
	}

	if err := t.nc.Publish(subject, data); err != nil {
		return ErrOutputf(ErrKindTransient, "failed to send to pane %s: %v", p.PaneID, err), nil
	}

	return &ToolOutput{
		Content: fmt.Sprintf("Input sent to pane %s: %s", p.PaneID, truncateStr(p.Input, 100)),
		Metadata: map[string]string{
			"pane_id": p.PaneID,
			"mode":    p.Mode,
		},
	}, nil
}
