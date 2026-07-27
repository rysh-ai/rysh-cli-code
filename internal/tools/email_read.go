package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
)

// EmailReadTool reads the full content of a single email by UID.
type EmailReadTool struct {
	adapter *channels.EmailAdapter
}

// EmailReadParams holds the parameters for the email_read tool. Prefer id (the
// short handle from email_list); uid is accepted as a fallback.
type EmailReadParams struct {
	ID  string `json:"id"`
	UID int    `json:"uid"`
}

// NewEmailReadTool creates a new EmailReadTool.
func NewEmailReadTool(adapter *channels.EmailAdapter) *EmailReadTool {
	return &EmailReadTool{adapter: adapter}
}

// Spec returns the tool specification.
func (t *EmailReadTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {
    "id": {"type": "string", "description": "The short ID of the email from email_list (e.g. \"a3f9\")"},
    "uid": {"type": "integer", "description": "The numeric UID (fallback if you don't have the short ID)"}
  }
}`)
	return ToolSpec{
		Name:             "email_read",
		Description:      "Read the full content of an email by its short ID (from email_list) or its UID. Returns headers, body, and attachment list.",
		Parameters:       schema,
		RequiresApproval: false,
	}
}

// RequiresApproval returns false — reading emails is read-only.
func (t *EmailReadTool) RequiresApproval(_ json.RawMessage) bool { return false }

// Execute reads a single email by UID.
func (t *EmailReadTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p EmailReadParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid email_read params: %w", err)
	}
	uid := p.UID
	if id := strings.TrimSpace(p.ID); id != "" {
		if resolved, ok := t.adapter.ResolveShortID(id); ok {
			uid = resolved
		} else {
			return ErrOutputf(ErrKindMissing, "no email with id %q (run email_list first)", id), nil
		}
	}
	if uid <= 0 {
		return ErrOutput(ErrKindValidation, "provide an id (from email_list) or a positive uid"), nil
	}

	detail, err := t.adapter.ReadEmail(uid)
	if err != nil {
		return ErrOutputf(ErrKindInternal, "email_read failed: %v", err), nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("ID: %s\n", detail.ID))
	sb.WriteString(fmt.Sprintf("From: %s\n", detail.From))
	sb.WriteString(fmt.Sprintf("To: %s\n", detail.To))
	sb.WriteString(fmt.Sprintf("Subject: %s\n", detail.Subject))
	sb.WriteString(fmt.Sprintf("Date: %s\n", detail.Date))
	sb.WriteString(fmt.Sprintf("Message-ID: %s\n", detail.MessageID))
	if detail.InReplyTo != "" {
		sb.WriteString(fmt.Sprintf("In-Reply-To: %s\n", detail.InReplyTo))
	}
	if len(detail.Attachments) > 0 {
		sb.WriteString(fmt.Sprintf("\nAttachments (%d):\n", len(detail.Attachments)))
		for _, a := range detail.Attachments {
			sb.WriteString(fmt.Sprintf("  - %s (%s, %d bytes)\n", a.Filename, a.ContentType, a.Size))
		}
	}
	sb.WriteString(fmt.Sprintf("\n--- Body ---\n%s\n", detail.Body))

	return &ToolOutput{Content: sb.String()}, nil
}
