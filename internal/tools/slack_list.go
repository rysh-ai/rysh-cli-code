// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
)

// SlackListTool lists recent inbound Slack messages from the adapter's
// in-memory history (populated by the Socket Mode event loop).
type SlackListTool struct {
	adapter *channels.SlackAdapter
}

// SlackListParams holds the parameters for the slack_list tool.
type SlackListParams struct {
	Count int `json:"count"`
}

// NewSlackListTool creates a new SlackListTool.
func NewSlackListTool(adapter *channels.SlackAdapter) *SlackListTool {
	return &SlackListTool{adapter: adapter}
}

// Spec returns the tool specification.
func (t *SlackListTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {
    "count": {"type": "integer", "description": "Number of recent messages to list (default 10, max 50)"}
  }
}`)
	return ToolSpec{
		Name:             "slack_list",
		Description:      "List recent inbound Slack messages. Returns a short ID, sender, channel, time, and a snippet for each. Use slack_read with the ID to see full content.",
		Parameters:       schema,
		RequiresApproval: false,
	}
}

// RequiresApproval returns false — listing messages is read-only.
func (t *SlackListTool) RequiresApproval(_ json.RawMessage) bool { return false }

// Execute lists recent inbound Slack messages.
func (t *SlackListTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p SlackListParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid slack_list params: %w", err)
	}
	if p.Count <= 0 {
		p.Count = 10
	}
	if p.Count > 50 {
		p.Count = 50
	}

	msgs := t.adapter.RecentMessages(p.Count)
	if len(msgs) == 0 {
		return &ToolOutput{Content: "No Slack messages received yet."}, nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Recent Slack messages (%d, newest last):\n\n", len(msgs)))
	for _, m := range msgs {
		sb.WriteString(fmt.Sprintf("ID: %s\nFrom: %s (%s) in %s\nTime: %s\nText: %s\n---\n",
			m.ID, m.Name, m.From, m.Channel, m.Time.Format("2006-01-02 15:04"), runeSnippet(m.Text, 100)))
	}
	return &ToolOutput{Content: sb.String()}, nil
}
