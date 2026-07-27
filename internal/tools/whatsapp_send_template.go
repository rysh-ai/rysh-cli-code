package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
)

// WhatsAppSendTemplateTool sends a pre-approved WhatsApp template message. It
// exists because free-form text is undeliverable outside WhatsApp's 24h
// customer-service window — re-engaging a recipient requires a template that
// was approved in Meta Business Manager, sent to a recipient who opted in.
type WhatsAppSendTemplateTool struct {
	adapter *channels.WhatsAppAdapter
}

// WhatsAppSendTemplateParams holds the parameters for the
// whatsapp_send_template tool.
type WhatsAppSendTemplateParams struct {
	To           string   `json:"to"`
	TemplateName string   `json:"template_name"`
	Language     string   `json:"language"`
	Params       []string `json:"params"`
}

// NewWhatsAppSendTemplateTool creates a new WhatsAppSendTemplateTool.
func NewWhatsAppSendTemplateTool(adapter *channels.WhatsAppAdapter) *WhatsAppSendTemplateTool {
	return &WhatsAppSendTemplateTool{adapter: adapter}
}

// Spec returns the tool specification.
func (t *WhatsAppSendTemplateTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {
    "to": {"type": "string", "description": "Recipient phone number (wa_id)"},
    "template_name": {"type": "string", "description": "Name of a template pre-approved in Meta Business Manager"},
    "language": {"type": "string", "description": "Template language code (e.g. en_US). Defaults to en_US."},
    "params": {"type": "array", "items": {"type": "string"}, "description": "Positional body parameters filling {{1}}, {{2}}, ... in the template. Omit for templates without parameters."}
  },
  "required": ["to", "template_name"]
}`)
	return ToolSpec{
		Name:             "whatsapp_send_template",
		Description:      "Send a pre-approved WhatsApp template message. Required to reach a recipient whose 24h session window has closed (free-form text would be rejected). The template must be approved in Meta Business Manager and the recipient must have opted in. Only call this after the human has approved.",
		Parameters:       schema,
		RequiresApproval: true,
	}
}

// RequiresApproval returns true — sending a message is a side-effecting operation.
func (t *WhatsAppSendTemplateTool) RequiresApproval(_ json.RawMessage) bool { return true }

// Execute sends a WhatsApp template message.
func (t *WhatsAppSendTemplateTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p WhatsAppSendTemplateParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid whatsapp_send_template params: %w", err)
	}
	if p.To == "" || p.TemplateName == "" {
		return ErrOutput(ErrKindValidation, "to and template_name are required"), nil
	}

	// The adapter defaults an empty language to en_US and validates connection
	// state; the Cloud API enforces template approval and opt-in.
	if err := t.adapter.SendTemplate(ctx, p.To, p.TemplateName, p.Language, p.Params); err != nil {
		return ErrOutputf(ErrKindInternal, "template send failed: %v", err), nil
	}

	return &ToolOutput{
		Content:  fmt.Sprintf("WhatsApp template %q sent to %s.", p.TemplateName, p.To),
		Metadata: map[string]string{"to": p.To, "template": p.TemplateName},
	}, nil
}
