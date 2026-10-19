// SPDX-License-Identifier: Apache-2.0

package tools

// F-47: the only approval request that carries Choices is ask_user's question,
// and choice_selected is how a client answers it (ApprovalOverlay, which the
// phone also shows). ask_user used to read only Reason, so the choice index
// was dropped and the model was handed whatever the client smuggled there.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// askAndAnswer runs ask_user over a real in-process NATS bus, answers the
// question it publishes with resp (RequestID filled in), and returns the
// tool's output.
func askAndAnswer(t *testing.T, options []string, resp msg.MsgApprovalResponse) *ToolOutput {
	t.Helper()
	nc := newTestNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())
	const paneID = "pane-ask"

	sub, err := nc.Subscribe(msg.T("pane", paneID, "approval", "request"), func(m *nats.Msg) {
		var env msg.NATSEnvelope
		if json.Unmarshal(m.Data, &env) != nil || env.TypeTag != msg.TagApprovalRequest {
			return
		}
		var req msg.MsgApprovalRequest
		if json.Unmarshal(env.Payload, &req) != nil {
			return
		}
		if len(req.Choices) != len(options) {
			t.Errorf("request carries %d choices, want %d", len(req.Choices), len(options))
		}
		r := resp
		r.RequestID = req.RequestID
		_ = pub.Send(msg.T("pane", paneID, "approval", "response"), &r)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	_ = nc.Flush()

	params, _ := json.Marshal(AskUserParams{Question: "Which one?", Options: options})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := NewAskUserTool(nc, pub, paneID).Execute(ctx, params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return out
}

func TestAskUserChoiceSelectedReturnsTheChosenOption(t *testing.T) {
	out := askAndAnswer(t, []string{"red", "green", "blue"}, msg.MsgApprovalResponse{
		Decision:  msg.DecisionChoiceSelected,
		ChoiceIdx: 1,
	})
	if out.Content != "green" {
		t.Fatalf("choice_selected idx 1: model got %q, want %q", out.Content, "green")
	}
}

// Index 0 is omitted on the wire (omitempty) and must still mean the first
// option, not "no answer".
func TestAskUserChoiceSelectedIndexZero(t *testing.T) {
	out := askAndAnswer(t, []string{"red", "green"}, msg.MsgApprovalResponse{
		Decision: msg.DecisionChoiceSelected,
	})
	if out.Content != "red" {
		t.Fatalf("choice_selected idx 0: model got %q, want %q", out.Content, "red")
	}
}

// Clients before this fix put the zero-based index in Reason ("1") and sent no
// ChoiceIdx. The model was handed the bare string "1" — read against the
// one-based "[1] red" list in the question, the wrong option.
func TestAskUserChoiceSelectedLegacyIndexInReason(t *testing.T) {
	out := askAndAnswer(t, []string{"red", "green", "blue"}, msg.MsgApprovalResponse{
		Decision: msg.DecisionChoiceSelected,
		Reason:   "1",
	})
	if out.Content != "green" {
		t.Fatalf("legacy reason \"1\": model got %q, want %q", out.Content, "green")
	}
}

// An option whose label is itself a number must not be mistaken for a legacy
// index: the new client sends the label in Reason alongside ChoiceIdx.
func TestAskUserChoiceSelectedNumericLabel(t *testing.T) {
	out := askAndAnswer(t, []string{"1", "2", "3"}, msg.MsgApprovalResponse{
		Decision: msg.DecisionChoiceSelected,
		Reason:   "1",
	})
	if out.Content != "1" {
		t.Fatalf("numeric label \"1\" at idx 0: model got %q, want %q", out.Content, "1")
	}
}

func TestAskUserChoiceSelectedOutOfRange(t *testing.T) {
	out := askAndAnswer(t, []string{"red"}, msg.MsgApprovalResponse{
		Decision:  msg.DecisionChoiceSelected,
		ChoiceIdx: 5,
	})
	if out.Error == "" {
		t.Fatalf("out-of-range choice must be an error, got %+v", out)
	}
}

// Free-text answers are unchanged: Reason is the answer.
func TestAskUserFreeTextAnswer(t *testing.T) {
	out := askAndAnswer(t, nil, msg.MsgApprovalResponse{
		Decision: msg.DecisionNoWithExplanation,
		Reason:   "use postgres",
	})
	if out.Content != "use postgres" {
		t.Fatalf("free text: got %q", out.Content)
	}
}

// F-92: every client answers a question with the gate's vocabulary — the
// overlay's Approve/Reject (desktop and phone), the app's y/Y/n keys and the
// TUI's y/Y/n/Esc all send yes / yes_always / no with no reason. ask_user read
// only Reason, so all three reached the model as "(user provided empty
// response)": it could not tell yes from no from silence.
func TestAskUserDecisionsReachTheModel(t *testing.T) {
	cases := []struct {
		name string
		resp msg.MsgApprovalResponse
		want string
	}{
		{"yes", msg.MsgApprovalResponse{Decision: msg.DecisionYes}, "yes"},
		{"yes_always", msg.MsgApprovalResponse{Decision: msg.DecisionYesAlways}, "yes"},
		{"no", msg.MsgApprovalResponse{Decision: msg.DecisionNo}, "(user declined to answer)"},
		// A typed answer travels as yes + reason (the question overlay's answer
		// field) or, from older clients and the TUI's N, as no_with_explanation.
		{"yes with text", msg.MsgApprovalResponse{Decision: msg.DecisionYes, Reason: "use postgres"}, "use postgres"},
		{"no_with_explanation", msg.MsgApprovalResponse{Decision: msg.DecisionNoWithExplanation, Reason: "use sqlite"}, "use sqlite"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := askAndAnswer(t, nil, c.resp)
			if out.Content != c.want {
				t.Fatalf("%s: model got %q, want %q", c.name, out.Content, c.want)
			}
		})
	}
}
