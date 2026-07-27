package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
)

// EmailListTool lists recent emails from an IMAP inbox.
type EmailListTool struct {
	adapter *channels.EmailAdapter
}

// EmailListParams holds the parameters for the email_list tool.
type EmailListParams struct {
	Count  int    `json:"count"`
	Search string `json:"search"`
}

// NewEmailListTool creates a new EmailListTool.
func NewEmailListTool(adapter *channels.EmailAdapter) *EmailListTool {
	return &EmailListTool{adapter: adapter}
}

// Spec returns the tool specification.
func (t *EmailListTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {
    "count": {"type": "integer", "description": "Number of recent emails to list (default 10, max 50)"},
    "search": {"type": "string", "description": "Optional subject search filter"}
  }
}`)
	return ToolSpec{
		Name:             "email_list",
		Description:      "List recent emails from the inbox. Returns a short ID, From, Subject, Date, and a body snippet for each. Use the ID with email_read.",
		Parameters:       schema,
		RequiresApproval: false,
	}
}

// RequiresApproval returns false — listing emails is read-only.
func (t *EmailListTool) RequiresApproval(_ json.RawMessage) bool { return false }

// Execute lists recent emails from the inbox.
func (t *EmailListTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p EmailListParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid email_list params: %w", err)
	}
	if p.Count <= 0 {
		p.Count = 10
	}
	if p.Count > 50 {
		p.Count = 50
	}

	summaries, err := t.adapter.ListEmails(p.Count, p.Search)
	if err != nil {
		return ErrOutputf(ErrKindInternal, "email_list failed: %v", err), nil
	}

	if len(summaries) == 0 {
		return &ToolOutput{Content: "No emails found."}, nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d emails:\n\n", len(summaries)))
	for _, s := range summaries {
		sb.WriteString(fmt.Sprintf("ID: %s\nFrom: %s\nSubject: %s\nDate: %s\nSnippet: %s\n---\n",
			s.ID, s.From, s.Subject, s.Date, s.Snippet))
	}
	return &ToolOutput{Content: sb.String()}, nil
}
