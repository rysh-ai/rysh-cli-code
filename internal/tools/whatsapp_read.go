// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
)

// WhatsAppReadTool returns the full content of a received WhatsApp message.
type WhatsAppReadTool struct {
	adapter *channels.WhatsAppAdapter
}

// WhatsAppReadParams holds the parameters for the whatsapp_read tool.
type WhatsAppReadParams struct {
	ID string `json:"id"`
}

// NewWhatsAppReadTool creates a new WhatsAppReadTool.
func NewWhatsAppReadTool(adapter *channels.WhatsAppAdapter) *WhatsAppReadTool {
	return &WhatsAppReadTool{adapter: adapter}
}

// Spec returns the tool specification.
func (t *WhatsAppReadTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {
    "id": {"type": "string", "description": "The short 4-char message ID from whatsapp_list (e.g. \"a3f9\")"}
  },
  "required": ["id"]
}`)
	return ToolSpec{
		Name:             "whatsapp_read",
		Description:      "Read the full content of a received WhatsApp message by its ID (from whatsapp_list).",
		Parameters:       schema,
		RequiresApproval: false,
	}
}

// RequiresApproval returns false — reading a message is read-only.
func (t *WhatsAppReadTool) RequiresApproval(_ json.RawMessage) bool { return false }

// Execute returns the full content of a received WhatsApp message.
func (t *WhatsAppReadTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p WhatsAppReadParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid whatsapp_read params: %w", err)
	}
	if p.ID == "" {
		return ErrOutput(ErrKindValidation, "id is required"), nil
	}

	m, ok := t.adapter.GetMessage(p.ID)
	if !ok {
		return ErrOutputf(ErrKindMissing, "message %q not found (use whatsapp_list to see available IDs)", p.ID), nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("ID: %s\n", m.ID))
	sb.WriteString(fmt.Sprintf("From: %s (%s)\n", m.Name, m.From))
	sb.WriteString(fmt.Sprintf("Time: %s\n", m.Time.Format("2006-01-02 15:04:05")))
	sb.WriteString(fmt.Sprintf("\n--- Message ---\n%s\n--- End ---\n", m.Text))
	sb.WriteString(fmt.Sprintf("\nTo reply, use whatsapp_draft with to=%q, then whatsapp_send after the human approves.", m.From))
	return &ToolOutput{
		Content:  sb.String(),
		Metadata: map[string]string{"from": m.From, "name": m.Name},
	}, nil
}
