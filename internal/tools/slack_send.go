// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
)

// SlackSendTool posts a Slack message, either from a draft or directly.
type SlackSendTool struct {
	adapter *channels.SlackAdapter
	drafts  *channels.DraftStore
	// humanGoverned reports whether the owning humanoid is currently in
	// draft-and-confirm mode. Read per call (not captured at construction) so
	// `##humanoid governance <name> ai|human` takes effect immediately.
	// nil ⇒ never human-governed.
	humanGoverned func() bool
}

// SlackSendParams holds the parameters for the slack_send tool.
type SlackSendParams struct {
	DraftID string `json:"draft_id"`
	// Direct send fields (used when draft_id is not provided).
	Channel  string `json:"channel"`
	ThreadTS string `json:"thread_ts"`
	Body     string `json:"body"`
}

// NewSlackSendTool creates a new SlackSendTool.
//
// humanGoverned may be nil (never gated). When it reports true, Execute refuses
// anything but an owner-approved draft — see the gate in Execute.
func NewSlackSendTool(
	adapter *channels.SlackAdapter,
	drafts *channels.DraftStore,
	humanGoverned func() bool,
) *SlackSendTool {
	return &SlackSendTool{adapter: adapter, drafts: drafts, humanGoverned: humanGoverned}
}

// Spec returns the tool specification.
func (t *SlackSendTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {
    "draft_id": {"type": "string", "description": "ID of a previously created draft to post. Preferred over direct send."},
    "channel": {"type": "string", "description": "Channel ID (only if not using draft_id)"},
    "thread_ts": {"type": "string", "description": "Thread timestamp to reply in-thread (only if not using draft_id)"},
    "body": {"type": "string", "description": "Message text (only if not using draft_id)"}
  }
}`)
	return ToolSpec{
		Name:             "slack_send",
		Description:      "Post a Slack message. Either provide a draft_id (from slack_draft) or channel+body. Only call this after the human has approved the reply.",
		Parameters:       schema,
		RequiresApproval: true,
	}
}

// RequiresApproval returns true — posting a message is a side-effecting operation.
func (t *SlackSendTool) RequiresApproval(_ json.RawMessage) bool { return true }

// Execute posts a Slack message.
func (t *SlackSendTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p SlackSendParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid slack_send params: %w", err)
	}

	// The draft-and-confirm gate, shared by all tool-governed channels — see
	// requireApprovedDraft (draft_gate.go) for why it lives in the tool and
	// not in the approval plumbing.
	if out := requireApprovedDraft(t.humanGoverned, t.drafts, p.DraftID, "slack_send", "slack_draft"); out != nil {
		return out, nil
	}

	var channel, threadTS, body string
	if p.DraftID != "" {
		draft, ok := t.drafts.Get(p.DraftID)
		if !ok {
			return ErrOutputf(ErrKindMissing, "draft %q not found", p.DraftID), nil
		}
		channel = draft.To
		threadTS = draft.InReplyTo
		body = draft.Body
		defer t.drafts.Delete(p.DraftID)
	} else {
		if p.Channel == "" || p.Body == "" {
			return ErrOutput(ErrKindValidation, "either draft_id or channel+body are required"), nil
		}
		channel = p.Channel
		threadTS = p.ThreadTS
		body = p.Body
	}

	// Convert markdown to Slack mrkdwn, matching the AI-governed reply path.
	body = channels.ToSlackMrkdwn(body)

	if err := t.adapter.Send(ctx, channels.OutboundMessage{
		RecipientID: channel,
		ThreadID:    threadTS,
		Content:     body,
	}); err != nil {
		return ErrOutputf(ErrKindInternal, "send failed: %v", err), nil
	}

	return &ToolOutput{
		Content:  fmt.Sprintf("Slack message posted to %s.", channel),
		Metadata: map[string]string{"channel": channel, "thread_ts": threadTS},
	}, nil
}
