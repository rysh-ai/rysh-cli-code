package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
)

// WhatsAppDraftTool creates a WhatsApp reply draft for human review before
// sending. It reuses the shared DraftStore (To = recipient phone, Body = message;
// Subject/InReplyTo are unused for WhatsApp).
type WhatsAppDraftTool struct {
	drafts *channels.DraftStore
}

// WhatsAppDraftParams holds the parameters for the whatsapp_draft tool.
type WhatsAppDraftParams struct {
	To   string `json:"to"`
	Body string `json:"body"`
}

// NewWhatsAppDraftTool creates a new WhatsAppDraftTool.
func NewWhatsAppDraftTool(drafts *channels.DraftStore) *WhatsAppDraftTool {
	return &WhatsAppDraftTool{drafts: drafts}
}

// Spec returns the tool specification.
func (t *WhatsAppDraftTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {
    "to": {"type": "string", "description": "Recipient phone number (the sender's wa_id from the message you are replying to)"},
    "body": {"type": "string", "description": "The reply text"}
  },
  "required": ["to", "body"]
}`)
	return ToolSpec{
		Name:             "whatsapp_draft",
		Description:      "Create a WhatsApp reply draft for the human to review before sending. Returns a draft ID and preview. The draft is NOT sent until whatsapp_send is called.",
		Parameters:       schema,
		RequiresApproval: false,
	}
}

// RequiresApproval returns false — creating a draft does not send anything.
func (t *WhatsAppDraftTool) RequiresApproval(_ json.RawMessage) bool { return false }

// Execute creates a new WhatsApp reply draft.
func (t *WhatsAppDraftTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p WhatsAppDraftParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid whatsapp_draft params: %w", err)
	}
	if p.To == "" || p.Body == "" {
		return ErrOutput(ErrKindValidation, "to and body are required"), nil
	}

	id := t.drafts.Create(p.To, "", p.Body, "")

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Draft created: %s\n\n", id))
	sb.WriteString(fmt.Sprintf("To: %s\n", p.To))
	sb.WriteString(fmt.Sprintf("\n--- Message ---\n%s\n", p.Body))
	sb.WriteString(fmt.Sprintf("\nShow this to the human. Once they approve, call whatsapp_send with draft_id=%q.", id))

	return &ToolOutput{
		Content:  sb.String(),
		Metadata: map[string]string{"draft_id": id},
	}, nil
}
