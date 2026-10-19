// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// ORDER-12: the request used to be published BEFORE the answer subscription
// was opened, so an answer that arrived in between was dropped by the server
// (no subscriber yet) and the call sat out its whole window. These tests make
// that DETERMINISTIC rather than load-dependent: the publish hook answers from
// inside the publish and flushes, so the server has routed the answer before
// Execute moves on. No sleeps.

func raceTool(t *testing.T, paneID string) (*AskUserTool, func() int) {
	t.Helper()
	nc := newTestNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())
	return NewAskUserTool(nc, pub, paneID), nc.NumSubscriptions
}

// answerDuringPublish makes the tool's publish answer the question at once.
func answerDuringPublish(t *testing.T, tool *AskUserTool, resp msg.MsgApprovalResponse) {
	t.Helper()
	tool.send = func(subject string, m interface{}) error {
		if err := tool.pub.Send(subject, m); err != nil {
			return err
		}
		req, ok := m.(*msg.MsgApprovalRequest)
		if !ok {
			t.Fatalf("published %T, want *msg.MsgApprovalRequest", m)
		}
		r := resp
		r.RequestID = req.RequestID
		if err := tool.pub.Send(msg.T("pane", tool.paneID, "approval", "response"), &r); err != nil {
			return err
		}
		// A round trip on the same connection: once it returns, the server has
		// processed the answer — delivered it, or dropped it for want of a
		// subscriber.
		return tool.nc.Flush()
	}
}

func askParams(options ...string) json.RawMessage {
	b, _ := json.Marshal(AskUserParams{Question: "Which one?", Options: options})
	return b
}

func TestAskUserReceivesAnAnswerSentDuringThePublish(t *testing.T) {
	tool, _ := raceTool(t, "pane-race")
	answerDuringPublish(t, tool, msg.MsgApprovalResponse{Decision: msg.DecisionChoiceSelected, ChoiceIdx: 2})
	// The context only bounds a FAILING run; it is not what makes it fail.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := tool.Execute(ctx, askParams("red", "green", "blue"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out.Content != "blue" {
		t.Fatalf("an answer sent during the publish was lost: model got %q, want %q", out.Content, "blue")
	}
}

// No path may leak the answer subscription: answer, timeout, ctx cancel, and a
// publish that fails.
func TestAskUserLeavesNoSubscriptionBehind(t *testing.T) {
	t.Run("answer", func(t *testing.T) {
		tool, subs := raceTool(t, "p-answer")
		before := subs()
		answerDuringPublish(t, tool, msg.MsgApprovalResponse{Decision: msg.DecisionChoiceSelected})
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if out, _ := tool.Execute(ctx, askParams("a", "b")); out == nil || out.Content != "a" {
			t.Fatalf("answer path: got %+v", out)
		}
		if n := subs(); n != before {
			t.Errorf("answer path left %d subscriptions, want %d", n, before)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		tool, subs := raceTool(t, "p-timeout")
		before := subs()
		tool.timeout = 50 * time.Millisecond
		out, _ := tool.Execute(context.Background(), askParams("a"))
		if out == nil || out.Metadata["pause_on_timeout"] != "true" {
			t.Fatalf("timeout path: got %+v", out)
		}
		if n := subs(); n != before {
			t.Errorf("timeout path left %d subscriptions, want %d", n, before)
		}
	})
	t.Run("ctx cancel", func(t *testing.T) {
		tool, subs := raceTool(t, "p-cancel")
		before := subs()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if out, _ := tool.Execute(ctx, askParams("a")); out == nil || out.Content != "(cancelled)" {
			t.Fatalf("cancel path: got %+v", out)
		}
		if n := subs(); n != before {
			t.Errorf("cancel path left %d subscriptions, want %d", n, before)
		}
	})
	t.Run("publish error", func(t *testing.T) {
		tool, subs := raceTool(t, "p-puberr")
		before := subs()
		tool.send = func(string, interface{}) error { return errors.New("bus is down") }
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		out, err := tool.Execute(ctx, askParams("a"))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		// A question that never left must not wait out the window for an answer.
		if out == nil || !strings.Contains(out.Error, "bus is down") || out.ErrorKind != ErrKindTransient {
			t.Fatalf("publish-error path: got %+v; want the send error reported as a transient error", out)
		}
		if n := subs(); n != before {
			t.Errorf("publish-error path left %d subscriptions, want %d", n, before)
		}
	})
}
