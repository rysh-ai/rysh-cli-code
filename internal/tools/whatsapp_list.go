// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
)

// WhatsAppListTool lists recent inbound WhatsApp messages from the adapter's
// in-memory history (the Cloud API has no server-side "list messages" endpoint).
type WhatsAppListTool struct {
	adapter *channels.WhatsAppAdapter
}

// WhatsAppListParams holds the parameters for the whatsapp_list tool.
type WhatsAppListParams struct {
	Count int `json:"count"`
}

// NewWhatsAppListTool creates a new WhatsAppListTool.
func NewWhatsAppListTool(adapter *channels.WhatsAppAdapter) *WhatsAppListTool {
	return &WhatsAppListTool{adapter: adapter}
}

// Spec returns the tool specification.
func (t *WhatsAppListTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {
    "count": {"type": "integer", "description": "Number of recent messages to list (default 10, max 50)"}
  }
}`)
	return ToolSpec{
		Name:             "whatsapp_list",
		Description:      "List recent inbound WhatsApp messages. Returns a short ID, sender, name, time, and a snippet for each. Use whatsapp_read with the ID to see full content.",
		Parameters:       schema,
		RequiresApproval: false,
	}
}

// RequiresApproval returns false — listing messages is read-only.
func (t *WhatsAppListTool) RequiresApproval(_ json.RawMessage) bool { return false }

// Execute lists recent inbound WhatsApp messages.
func (t *WhatsAppListTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p WhatsAppListParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid whatsapp_list params: %w", err)
	}
	if p.Count <= 0 {
		p.Count = 10
	}
	if p.Count > 50 {
		p.Count = 50
	}

	msgs := t.adapter.RecentMessages(p.Count)
	if len(msgs) == 0 {
		return &ToolOutput{Content: "No WhatsApp messages received yet."}, nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Recent WhatsApp messages (%d, newest last):\n\n", len(msgs)))
	for _, m := range msgs {
		sb.WriteString(fmt.Sprintf("ID: %s\nFrom: %s (%s)\nTime: %s\nText: %s\n---\n",
			m.ID, m.Name, m.From, m.Time.Format("2006-01-02 15:04"), runeSnippet(m.Text, 100)))
	}
	return &ToolOutput{Content: sb.String()}, nil
}

// runeSnippet truncates s to at most n runes (UTF-8 safe), appending an ellipsis
// when truncated.
func runeSnippet(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
