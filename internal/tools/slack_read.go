// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
)

// SlackReadTool returns the full content of a received Slack message.
type SlackReadTool struct {
	adapter *channels.SlackAdapter
}

// SlackReadParams holds the parameters for the slack_read tool.
type SlackReadParams struct {
	ID string `json:"id"`
}

// NewSlackReadTool creates a new SlackReadTool.
func NewSlackReadTool(adapter *channels.SlackAdapter) *SlackReadTool {
	return &SlackReadTool{adapter: adapter}
}

// Spec returns the tool specification.
func (t *SlackReadTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {
    "id": {"type": "string", "description": "The short 4-char message ID from slack_list (e.g. \"a3f9\")"}
  },
  "required": ["id"]
}`)
	return ToolSpec{
		Name:             "slack_read",
		Description:      "Read the full content of a received Slack message by its ID (from slack_list).",
		Parameters:       schema,
		RequiresApproval: false,
	}
}

// RequiresApproval returns false — reading a message is read-only.
func (t *SlackReadTool) RequiresApproval(_ json.RawMessage) bool { return false }

// Execute returns the full content of a received Slack message.
func (t *SlackReadTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p SlackReadParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid slack_read params: %w", err)
	}
	if p.ID == "" {
		return ErrOutput(ErrKindValidation, "id is required"), nil
	}

	m, ok := t.adapter.GetMessage(p.ID)
	if !ok {
		return ErrOutputf(ErrKindMissing, "message %q not found (use slack_list to see available IDs)", p.ID), nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("ID: %s\n", m.ID))
	sb.WriteString(fmt.Sprintf("From: %s (%s)\n", m.Name, m.From))
	sb.WriteString(fmt.Sprintf("Channel: %s\n", m.Channel))
	sb.WriteString(fmt.Sprintf("Time: %s\n", m.Time.Format("2006-01-02 15:04:05")))
	sb.WriteString(fmt.Sprintf("\n--- Message ---\n%s\n--- End ---\n", m.Text))
	sb.WriteString(fmt.Sprintf("\nTo reply, use slack_draft with channel=%q and thread_ts=%q, then slack_send after the human approves.",
		m.Channel, m.ThreadTS))
	return &ToolOutput{
		Content:  sb.String(),
		Metadata: map[string]string{"channel": m.Channel, "thread_ts": m.ThreadTS, "from": m.From, "name": m.Name},
	}, nil
}
