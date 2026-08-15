// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// SessionHistoryTool retrieves the conversation transcript from another pane's LLMPromptExecutionActor.
type SessionHistoryTool struct {
	nc      *nats.Conn
	session string
}

// SessionHistoryParams holds the parameters for requesting conversation history.
type SessionHistoryParams struct {
	PaneID string `json:"pane_id"`          // target pane ID
	LastN  int    `json:"last_n,omitempty"` // number of recent turns to retrieve (default 20)
}

// NewSessionHistoryTool creates a SessionHistoryTool.
func NewSessionHistoryTool(nc *nats.Conn, session string) *SessionHistoryTool {
	return &SessionHistoryTool{nc: nc, session: session}
}

// Spec returns the tool specification for the LLM.
func (t *SessionHistoryTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {
    "pane_id": {"type": "string", "description": "The UUID of the pane whose conversation history to retrieve"},
    "last_n": {"type": "integer", "description": "Number of recent conversation turns to return (default 20, max 100)"}
  },
  "required": ["pane_id"]
}`)
	return ToolSpec{
		Name:             "session_history",
		Description:      "Retrieve the conversation transcript (LLM conversation history) from another pane's agent. Shows the reasoning chain, tool calls, and results — not just terminal output. Use this to understand what another agent has been doing and thinking.",
		Parameters:       schema,
		RequiresApproval: false,
	}
}

// RequiresApproval always returns false — read-only.
func (t *SessionHistoryTool) RequiresApproval(_ json.RawMessage) bool {
	return false
}

// Execute requests the conversation history from the target pane's LLMPromptExecutionActor.
func (t *SessionHistoryTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p SessionHistoryParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid session_history params: %w", err)
	}

	if p.PaneID == "" {
		return ErrOutput(ErrKindValidation, "pane_id is required"), nil
	}

	lastN := p.LastN
	if lastN <= 0 {
		lastN = 20
	}
	if lastN > 100 {
		lastN = 100
	}

	if t.nc == nil {
		return ErrOutput(ErrKindMissing, "NATS connection not available"), nil
	}

	// Send a request to the pane's agentic inbox asking for conversation history.
	subject := msg.T("pane", p.PaneID, "llm_prompt_execution", "inbox")
	reqMsg := &msg.MsgGetConversationHistory{
		LastN: lastN,
	}
	reqPayload, _ := json.Marshal(reqMsg)
	envelope := msg.NATSEnvelope{
		TypeTag: msg.TagGetConversationHistory,
		ReplyTo: nats.NewInbox(),
		Payload: reqPayload,
	}
	data, err := json.Marshal(envelope)
	if err != nil {
		return ErrOutputf(ErrKindInternal, "failed to marshal request: %v", err), nil
	}

	// Subscribe to the reply inbox before publishing.
	replyCh := make(chan *nats.Msg, 1)
	sub, err := t.nc.ChanSubscribe(envelope.ReplyTo, replyCh)
	if err != nil {
		return ErrOutputf(ErrKindInternal, "failed to subscribe to reply: %v", err), nil
	}
	defer func() { _ = sub.Unsubscribe() }()

	if err := t.nc.Publish(subject, data); err != nil {
		return ErrOutputf(ErrKindTransient, "failed to send request: %v", err), nil
	}

	// Wait for reply with timeout.
	select {
	case replyMsg := <-replyCh:
		var respEnvelope msg.NATSEnvelope
		if err := json.Unmarshal(replyMsg.Data, &respEnvelope); err != nil {
			return ErrOutputf(ErrKindValidation, "invalid response: %v", err), nil
		}

		var histReply msg.MsgConversationHistoryReply
		if err := json.Unmarshal(respEnvelope.Payload, &histReply); err != nil {
			return ErrOutputf(ErrKindInternal, "failed to parse history: %v", err), nil
		}

		return t.formatHistory(p.PaneID, histReply.Turns), nil

	case <-time.After(5 * time.Second):
		return &ToolOutput{
			Content: fmt.Sprintf("Pane %s did not respond. It may not have an active agentic session, or may not exist.", p.PaneID),
		}, nil

	case <-ctx.Done():
		return ErrOutput(ErrKindCancelled, "request cancelled"), nil
	}
}

// formatHistory renders the conversation turns into a human-readable transcript.
func (t *SessionHistoryTool) formatHistory(paneID string, turns []msg.ConversationTurnInfo) *ToolOutput {
	if len(turns) == 0 {
		return &ToolOutput{
			Content: fmt.Sprintf("Pane %s has no conversation history (agent may not have been used yet).", paneID),
		}
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Conversation history for pane %s (%d turns):\n", paneID, len(turns)))
	sb.WriteString(strings.Repeat("=", 60) + "\n\n")

	for i, turn := range turns {
		switch turn.Role {
		case "user":
			sb.WriteString(fmt.Sprintf("[%d] USER:\n%s\n\n", i+1, turn.Content))
		case "assistant":
			sb.WriteString(fmt.Sprintf("[%d] ASSISTANT:\n", i+1))
			if turn.Content != "" {
				sb.WriteString(turn.Content + "\n")
			}
			for _, tc := range turn.ToolCalls {
				sb.WriteString(fmt.Sprintf("  -> tool_call: %s(%s)\n", tc.Name, truncateStr(string(tc.Input), 100)))
			}
			sb.WriteString("\n")
		case "tool":
			sb.WriteString(fmt.Sprintf("[%d] TOOL RESULT (id: %s):\n", i+1, turn.ToolCallID))
			content := turn.Content
			if len(content) > 500 {
				content = content[:500] + "... (truncated)"
			}
			if turn.IsError {
				sb.WriteString(fmt.Sprintf("  ERROR: %s\n\n", content))
			} else {
				sb.WriteString(fmt.Sprintf("  %s\n\n", content))
			}
		}
	}

	return &ToolOutput{
		Content: sb.String(),
		Metadata: map[string]string{
			"pane_id":    paneID,
			"turn_count": fmt.Sprintf("%d", len(turns)),
		},
	}
}
