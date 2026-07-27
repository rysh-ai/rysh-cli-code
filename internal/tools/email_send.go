package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
)

// EmailSendTool sends an email either from a draft or directly.
type EmailSendTool struct {
	adapter *channels.EmailAdapter
	drafts  *channels.DraftStore
}

// EmailSendParams holds the parameters for the email_send tool.
type EmailSendParams struct {
	DraftID string `json:"draft_id"`
	// Direct send fields (used when draft_id is not provided).
	To        string `json:"to"`
	Subject   string `json:"subject"`
	Body      string `json:"body"`
	InReplyTo string `json:"in_reply_to"`
}

// NewEmailSendTool creates a new EmailSendTool.
func NewEmailSendTool(adapter *channels.EmailAdapter, drafts *channels.DraftStore) *EmailSendTool {
	return &EmailSendTool{adapter: adapter, drafts: drafts}
}

// Spec returns the tool specification.
func (t *EmailSendTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {
    "draft_id": {"type": "string", "description": "ID of a previously created draft to send. Preferred over direct send."},
    "to": {"type": "string", "description": "Recipient email (only if not using draft_id)"},
    "subject": {"type": "string", "description": "Subject line (only if not using draft_id)"},
    "body": {"type": "string", "description": "Email body (only if not using draft_id)"},
    "in_reply_to": {"type": "string", "description": "Message-ID for email threading (only if not using draft_id)"}
  }
}`)
	return ToolSpec{
		Name:             "email_send",
		Description:      "Send an email. Either provide a draft_id to send a previously drafted email, or provide to/subject/body to send directly. Attachments can only be sent via drafts.",
		Parameters:       schema,
		RequiresApproval: true,
	}
}

// RequiresApproval returns true — sending email is a side-effecting operation.
func (t *EmailSendTool) RequiresApproval(_ json.RawMessage) bool { return true }

// Execute sends an email.
func (t *EmailSendTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p EmailSendParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid email_send params: %w", err)
	}

	var to, subject, body, inReplyTo string
	var attachments []channels.Attachment

	if p.DraftID != "" {
		draft, ok := t.drafts.Get(p.DraftID)
		if !ok {
			return ErrOutputf(ErrKindMissing, "draft %q not found", p.DraftID), nil
		}
		to = draft.To
		subject = draft.Subject
		body = draft.Body
		inReplyTo = draft.InReplyTo
		attachments = draft.Attachments
		// Delete draft after sending.
		defer t.drafts.Delete(p.DraftID)
	} else {
		if p.To == "" || p.Subject == "" || p.Body == "" {
			return ErrOutput(ErrKindValidation, "either draft_id or to+subject+body are required"), nil
		}
		to = p.To
		subject = p.Subject
		body = p.Body
		inReplyTo = p.InReplyTo
	}

	if err := t.adapter.SendEmail(to, subject, body, inReplyTo, attachments); err != nil {
		return ErrOutputf(ErrKindInternal, "send failed: %v", err), nil
	}

	return &ToolOutput{
		Content:  fmt.Sprintf("Email sent successfully.\nTo: %s\nSubject: %s", to, subject),
		Metadata: map[string]string{"to": to, "subject": subject},
	}, nil
}
