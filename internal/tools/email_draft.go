package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
)

// EmailDraftTool creates an email draft for review before sending.
type EmailDraftTool struct {
	drafts *channels.DraftStore
}

// EmailDraftParams holds the parameters for the email_draft tool.
type EmailDraftParams struct {
	To        string `json:"to"`
	Subject   string `json:"subject"`
	Body      string `json:"body"`
	InReplyTo string `json:"in_reply_to"`
}

// NewEmailDraftTool creates a new EmailDraftTool.
func NewEmailDraftTool(drafts *channels.DraftStore) *EmailDraftTool {
	return &EmailDraftTool{drafts: drafts}
}

// Spec returns the tool specification.
func (t *EmailDraftTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {
    "to": {"type": "string", "description": "Recipient email address"},
    "subject": {"type": "string", "description": "Email subject line"},
    "body": {"type": "string", "description": "Email body text"},
    "in_reply_to": {"type": "string", "description": "Message-ID of the email being replied to (for threading)"}
  },
  "required": ["to", "subject", "body"]
}`)
	return ToolSpec{
		Name:             "email_draft",
		Description:      "Create an email draft for review before sending. Returns a draft ID and preview. The draft is NOT sent until email_send is called.",
		Parameters:       schema,
		RequiresApproval: false,
	}
}

// RequiresApproval returns false — creating a draft does not send anything.
func (t *EmailDraftTool) RequiresApproval(_ json.RawMessage) bool { return false }

// Execute creates a new email draft.
func (t *EmailDraftTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p EmailDraftParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid email_draft params: %w", err)
	}
	if p.To == "" || p.Subject == "" || p.Body == "" {
		return ErrOutput(ErrKindValidation, "to, subject, and body are required"), nil
	}

	id := t.drafts.Create("email", p.To, p.Subject, p.Body, p.InReplyTo)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Draft created: %s\n\n", id))
	sb.WriteString(fmt.Sprintf("To: %s\n", p.To))
	sb.WriteString(fmt.Sprintf("Subject: %s\n", p.Subject))
	if p.InReplyTo != "" {
		sb.WriteString(fmt.Sprintf("In-Reply-To: %s\n", p.InReplyTo))
	}
	sb.WriteString(fmt.Sprintf("\n--- Body ---\n%s\n", p.Body))
	sb.WriteString(fmt.Sprintf("\nUse email_send with draft_id=%q to send, or email_attach to add attachments.", id))

	return &ToolOutput{
		Content:  sb.String(),
		Metadata: map[string]string{"draft_id": id},
	}, nil
}
