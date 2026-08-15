// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

const askUserTimeout = 5 * time.Minute

// AskUserTool asks the user a clarifying question and waits for their answer.
type AskUserTool struct {
	nc     *nats.Conn
	pub    *msg.NATSPublisher
	paneID string
}

// AskUserParams holds the parameters for the ask_user tool.
type AskUserParams struct {
	Question string   `json:"question"`
	Options  []string `json:"options,omitempty"`
}

// NewAskUserTool creates a new AskUserTool with NATS access for the given pane.
func NewAskUserTool(nc *nats.Conn, pub *msg.NATSPublisher, paneID string) *AskUserTool {
	return &AskUserTool{
		nc:     nc,
		pub:    pub,
		paneID: paneID,
	}
}

// Spec returns the tool specification for the LLM.
func (t *AskUserTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {
    "question": {"type": "string", "description": "The question to ask the user"},
    "options": {"type": "array", "items": {"type": "string"}, "description": "Optional list of choices for multiple choice questions"}
  },
  "required": ["question"]
}`)
	return ToolSpec{
		Name:             "ask_user",
		Description:      "Ask the user a clarifying question and wait for their response. Use this when you need more information to proceed.",
		Parameters:       schema,
		RequiresApproval: false,
	}
}

// RequiresApproval always returns false — this tool IS the user interaction.
func (t *AskUserTool) RequiresApproval(_ json.RawMessage) bool {
	return false
}

// Execute publishes a question to the user and waits for their answer.
func (t *AskUserTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p AskUserParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid params: %w", err)
	}

	if p.Question == "" {
		return ErrOutput(ErrKindValidation, "question is required"), nil
	}

	// Build description with options if provided
	var description strings.Builder
	description.WriteString(p.Question)
	if len(p.Options) > 0 {
		description.WriteString("\n\nOptions:\n")
		for i, opt := range p.Options {
			description.WriteString(fmt.Sprintf("  [%d] %s\n", i+1, opt))
		}
	}

	// Build choices for the approval request
	var choices []msg.Choice
	for _, opt := range p.Options {
		choices = append(choices, msg.Choice{
			Label:       opt,
			Description: opt,
		})
	}

	// Send approval request with question type
	reqID := uuid.New().String()
	approvalSubject := msg.T("pane", t.paneID, "approval", "request")
	_ = t.pub.Send(approvalSubject, &msg.MsgApprovalRequest{
		RequestID:   reqID,
		ToolCallID:  reqID,
		Type:        msg.ApprovalTypeQuestion,
		Description: description.String(),
		Choices:     choices,
	})

	// Also emit the question to pane output for TUI display
	_ = t.pub.SendPaneAIOutput(t.paneID, "\n"+description.String()+"\n> ")

	// Wait for response
	answer, timedOut := t.waitForAnswer(ctx, reqID)

	if timedOut {
		// Escalate instead of discarding the question: the pause_on_timeout
		// metadata tells the orchestrator to PAUSE the run awaiting the
		// user's answer (StoppedReasonAwaitingInfo). The user can reply
		// hours later — their next prompt resumes from the checkpoint with
		// this question still pending in the transcript.
		return &ToolOutput{
			Content: "No response within the interactive window. The session will pause until the user replies; " +
				"their answer will arrive as the next user message — do not repeat the question.",
			Metadata: map[string]string{
				"question":         p.Question,
				"pause_on_timeout": "true",
			},
		}, nil
	}

	return &ToolOutput{
		Content: answer,
		Metadata: map[string]string{
			"question": p.Question,
		},
	}, nil
}

// waitForAnswer subscribes and waits for the user's answer. The second return
// is true when the interactive window elapsed with no answer (the caller
// escalates to an awaiting-info pause instead of discarding the question).
func (t *AskUserTool) waitForAnswer(ctx context.Context, requestID string) (string, bool) {
	subject := msg.T("pane", t.paneID, "approval", "response")
	ch := make(chan string, 1)

	sub, err := t.nc.Subscribe(subject, func(m *nats.Msg) {
		var env msg.NATSEnvelope
		if err := json.Unmarshal(m.Data, &env); err != nil {
			return
		}
		if env.TypeTag != msg.TagApprovalResponse {
			return
		}
		var resp msg.MsgApprovalResponse
		if err := json.Unmarshal(env.Payload, &resp); err != nil {
			return
		}
		if resp.RequestID == requestID {
			ch <- resp.Reason
		}
	})
	if err != nil {
		return "(failed to subscribe for user response)", false
	}
	defer func() { _ = sub.Unsubscribe() }()

	select {
	case answer := <-ch:
		if answer == "" {
			return "(user provided empty response)", false
		}
		return answer, false
	case <-time.After(askUserTimeout):
		return "", true
	case <-ctx.Done():
		return "(cancelled)", false
	}
}
