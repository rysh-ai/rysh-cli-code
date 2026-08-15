// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
)

// WhatsAppSendTool sends a WhatsApp message, either from a draft or directly.
type WhatsAppSendTool struct {
	adapter *channels.WhatsAppAdapter
	drafts  *channels.DraftStore
	// humanGoverned reports whether the owning humanoid is currently in
	// draft-and-confirm mode. Read per call (not captured at construction) so
	// `##humanoid governance <name> ai|human` takes effect immediately.
	// nil ⇒ never human-governed.
	humanGoverned func() bool
}

// WhatsAppSendParams holds the parameters for the whatsapp_send tool.
type WhatsAppSendParams struct {
	DraftID string `json:"draft_id"`
	// Direct send fields (used when draft_id is not provided).
	To   string `json:"to"`
	Body string `json:"body"`
}

// NewWhatsAppSendTool creates a new WhatsAppSendTool.
//
// humanGoverned may be nil (never gated). When it reports true, Execute
// refuses anything but an owner-approved draft — see requireApprovedDraft.
func NewWhatsAppSendTool(
	adapter *channels.WhatsAppAdapter,
	drafts *channels.DraftStore,
	humanGoverned func() bool,
) *WhatsAppSendTool {
	return &WhatsAppSendTool{adapter: adapter, drafts: drafts, humanGoverned: humanGoverned}
}

// Spec returns the tool specification.
func (t *WhatsAppSendTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {
    "draft_id": {"type": "string", "description": "ID of a previously created draft to send. Preferred over direct send."},
    "to": {"type": "string", "description": "Recipient phone number (only if not using draft_id)"},
    "body": {"type": "string", "description": "Message text (only if not using draft_id)"}
  }
}`)
	return ToolSpec{
		Name:             "whatsapp_send",
		Description:      "Send a WhatsApp message. Either provide a draft_id (from whatsapp_draft) or to+body. Only call this after the human has approved the reply.",
		Parameters:       schema,
		RequiresApproval: true,
	}
}

// RequiresApproval returns true — sending a message is a side-effecting operation.
func (t *WhatsAppSendTool) RequiresApproval(_ json.RawMessage) bool { return true }

// Execute sends a WhatsApp message.
func (t *WhatsAppSendTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p WhatsAppSendParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid whatsapp_send params: %w", err)
	}

	// The draft-and-confirm gate, shared by all tool-governed channels — see
	// requireApprovedDraft (draft_gate.go). Under human governance nothing
	// reaches WhatsApp except a draft the owner explicitly confirmed.
	if out := requireApprovedDraft(t.humanGoverned, t.drafts, p.DraftID, "whatsapp_send", "whatsapp_draft"); out != nil {
		return out, nil
	}

	var to, body string
	if p.DraftID != "" {
		draft, ok := t.drafts.Get(p.DraftID)
		if !ok {
			return ErrOutputf(ErrKindMissing, "draft %q not found", p.DraftID), nil
		}
		to = draft.To
		body = draft.Body
		defer t.drafts.Delete(p.DraftID)
	} else {
		if p.To == "" || p.Body == "" {
			return ErrOutput(ErrKindValidation, "either draft_id or to+body are required"), nil
		}
		to = p.To
		body = p.Body
	}

	if err := t.adapter.Send(ctx, channels.OutboundMessage{RecipientID: to, Content: body}); err != nil {
		return ErrOutputf(ErrKindInternal, "send failed: %v", err), nil
	}

	return &ToolOutput{
		Content:  fmt.Sprintf("WhatsApp message sent to %s.", to),
		Metadata: map[string]string{"to": to},
	}, nil
}
