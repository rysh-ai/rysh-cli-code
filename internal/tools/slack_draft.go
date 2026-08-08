package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
)

// SlackDraftTool creates a Slack reply draft for human review before sending.
// It reuses the shared DraftStore (To = channel ID, InReplyTo = thread_ts,
// Body = message text; Subject is unused for Slack).
type SlackDraftTool struct {
	drafts *channels.DraftStore
}

// SlackDraftParams holds the parameters for the slack_draft tool.
type SlackDraftParams struct {
	Channel  string `json:"channel"`
	ThreadTS string `json:"thread_ts"`
	Body     string `json:"body"`
}

// NewSlackDraftTool creates a new SlackDraftTool.
func NewSlackDraftTool(drafts *channels.DraftStore) *SlackDraftTool {
	return &SlackDraftTool{drafts: drafts}
}

// Spec returns the tool specification.
func (t *SlackDraftTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {
    "channel": {"type": "string", "description": "Channel ID to post into (from the message you are replying to)"},
    "thread_ts": {"type": "string", "description": "Thread timestamp to reply in-thread (from the message you are replying to). Omit to post a new top-level message."},
    "body": {"type": "string", "description": "The reply text"}
  },
  "required": ["channel", "body"]
}`)
	return ToolSpec{
		Name:             "slack_draft",
		Description:      "Create a Slack reply draft for the human to review before posting. Returns a draft ID and preview. The draft is NOT posted until slack_send is called.",
		Parameters:       schema,
		RequiresApproval: false,
	}
}

// RequiresApproval returns false — creating a draft does not post anything.
func (t *SlackDraftTool) RequiresApproval(_ json.RawMessage) bool { return false }

// Execute creates a new Slack reply draft.
func (t *SlackDraftTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p SlackDraftParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid slack_draft params: %w", err)
	}
	if p.Channel == "" || p.Body == "" {
		return ErrOutput(ErrKindValidation, "channel and body are required"), nil
	}

	id := t.drafts.Create("slack", p.Channel, "", p.Body, p.ThreadTS)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Draft created: %s\n\n", id))
	sb.WriteString(fmt.Sprintf("Channel: %s\n", p.Channel))
	if p.ThreadTS != "" {
		sb.WriteString(fmt.Sprintf("Thread: %s\n", p.ThreadTS))
	}
	sb.WriteString(fmt.Sprintf("\n--- Message ---\n%s\n", p.Body))
	sb.WriteString(fmt.Sprintf("\nShow this to the human. Once they approve, call slack_send with draft_id=%q.", id))

	return &ToolOutput{
		Content:  sb.String(),
		Metadata: map[string]string{"draft_id": id},
	}, nil
}
