// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
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
	// send publishes the question (pub.Send). A field so a test can answer
	// from inside the publish, which is how the publish-before-subscribe race
	// is made deterministic.
	send func(subject string, message interface{}) error
	// timeout is the interactive window (askUserTimeout).
	timeout time.Duration
}

// AskUserParams holds the parameters for the ask_user tool.
type AskUserParams struct {
	Question string   `json:"question"`
	Options  []string `json:"options,omitempty"`
}

// NewAskUserTool creates a new AskUserTool with NATS access for the given pane.
func NewAskUserTool(nc *nats.Conn, pub *msg.NATSPublisher, paneID string) *AskUserTool {
	return &AskUserTool{
		nc:      nc,
		pub:     pub,
		paneID:  paneID,
		send:    pub.Send,
		timeout: askUserTimeout,
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

	reqID := uuid.New().String()

	// Subscribe for the answer BEFORE publishing the question. Publishing first
	// lost any answer that arrived in between — the server drops a message no
	// one is subscribed to — and the call then sat out its whole window
	// (ORDER-12). The deferred unsubscribe covers every path below.
	answers, unsubscribe, err := t.subscribeAnswer(reqID)
	if err != nil {
		return &ToolOutput{
			Content:  "(failed to subscribe for user response)",
			Metadata: map[string]string{"question": p.Question},
		}, nil
	}
	defer unsubscribe()

	// Send approval request with question type
	approvalSubject := msg.T("pane", t.paneID, "approval", "request")
	if err := t.send(approvalSubject, &msg.MsgApprovalRequest{
		RequestID:   reqID,
		ToolCallID:  reqID,
		Type:        msg.ApprovalTypeQuestion,
		Description: description.String(),
		Choices:     choices,
	}); err != nil {
		// A question that never left cannot be answered; do not wait the
		// whole window for it.
		return ErrOutput(ErrKindTransient, fmt.Sprintf("could not send the question to the user: %v", err)), nil
	}

	// Also emit the question to pane output for TUI display
	_ = t.pub.SendPaneAIOutput(t.paneID, "\n"+description.String()+"\n> ")

	// Wait for response
	resp, answer, timedOut := t.waitForAnswer(ctx, answers)

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

	if resp != nil {
		switch resp.Decision {
		case msg.DecisionChoiceSelected:
			idx, ok := chosenIndex(resp, p.Options)
			if !ok {
				return ErrOutput(ErrKindValidation, fmt.Sprintf(
					"user picked option %d but the question offered %d", idx+1, len(p.Options))), nil
			}
			answer = p.Options[idx]
		case msg.DecisionYes, msg.DecisionYesAlways:
			// F-92: a gate's Approve (overlay button, y/Y key) answering a
			// question is a "yes"; with text it is the typed answer.
			if resp.Reason == "" {
				answer = "yes"
			}
		case msg.DecisionNo:
			// Reject / n / Esc: the user declined, which is not the same as
			// saying nothing — the model must be able to tell them apart.
			answer = "(user declined to answer)"
		}
		// no_with_explanation: Reason is the typed answer, as before.
	}

	return &ToolOutput{
		Content: answer,
		Metadata: map[string]string{
			"question": p.Question,
		},
	}, nil
}

// chosenIndex resolves a choice_selected answer to an index into options.
// ChoiceIdx is authoritative (zero-based; omitted on the wire when 0). Clients
// before F-47 sent no ChoiceIdx and put the zero-based index in Reason as a
// string; that is honoured only when Reason is not itself one of the labels,
// which current clients send there. ok is false when the index is out of range.
func chosenIndex(resp *msg.MsgApprovalResponse, options []string) (idx int, ok bool) {
	idx = resp.ChoiceIdx
	if idx == 0 && resp.Reason != "" && !slices.Contains(options, resp.Reason) {
		if n, err := strconv.Atoi(resp.Reason); err == nil {
			idx = n
		}
	}
	return idx, idx >= 0 && idx < len(options)
}

// subscribeAnswer opens the subscription for the answer to requestID and
// returns its channel and the unsubscribe. It is flushed before returning, so the
// server knows the subscription before the question goes out — an answer from
// another connection cannot beat it either.
func (t *AskUserTool) subscribeAnswer(requestID string) (<-chan *msg.MsgApprovalResponse, func(), error) {
	subject := msg.T("pane", t.paneID, "approval", "response")
	ch := make(chan *msg.MsgApprovalResponse, 1)

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
			select {
			case ch <- &resp:
			default:
			}
		}
	})
	if err != nil {
		return nil, nil, err
	}
	_ = t.nc.FlushTimeout(answerSubscribeFlush)
	return ch, func() { _ = sub.Unsubscribe() }, nil
}

// answerSubscribeFlush bounds the round trip that registers the subscription.
const answerSubscribeFlush = 2 * time.Second

// waitForAnswer waits for the user's answer on ch. It returns the response (nil
// when none arrived), its Reason as the answer text, and true when the
// interactive window elapsed with no answer (the caller escalates to an
// awaiting-info pause instead of discarding the question).
func (t *AskUserTool) waitForAnswer(ctx context.Context, ch <-chan *msg.MsgApprovalResponse) (*msg.MsgApprovalResponse, string, bool) {
	select {
	case resp := <-ch:
		if resp.Reason == "" {
			return resp, "(user provided empty response)", false
		}
		return resp, resp.Reason, false
	case <-time.After(t.timeout):
		return nil, "", true
	case <-ctx.Done():
		return nil, "(cancelled)", false
	}
}
