// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
)

// E33 follow-up 3 (founder order 2026-10-28): when the model call fails, the
// owner must hear about it in the chat. The orchestrator streams its error as
// output type "error", which the humanoid never buffers for the channel, so a
// failed run reached flushOutboundOnCompletion("error") with an empty buffer
// and sent nothing — the founder's live test looked like a bot that ignores
// messages.

// aiTelegramHumanoid is an AI-governed telegram humanoid with a reply pending
// for one inbound from the owner.
func aiTelegramHumanoid(t *testing.T) (*HumanoidActor, *countingAdapter) {
	t.Helper()
	h := newHumanoidForQueueTest(t)
	stub := &countingAdapter{}
	h.name = "assistant"
	h.adapters = map[string]channels.ChannelAdapter{"telegram": stub}
	h.governance = map[string]string{"telegram": "ai"}
	h.lastInbound = genericGovInbound("hi")
	h.outboundPending = true
	return h, stub
}

func assertErrorNotice(t *testing.T, content string) {
	t.Helper()
	for _, want := range []string{"couldn't answer", "doctor"} {
		if !strings.Contains(content, want) {
			t.Errorf("reply %q does not contain %q", content, want)
		}
	}
	if strings.Contains(content, "[LLM error") {
		t.Errorf("reply %q carries the raw model error; it belongs in the pane, not the chat", content)
	}
}

func TestE33ModelErrorSendsOwnerNotice(t *testing.T) {
	h, stub := aiTelegramHumanoid(t)
	h.flushOutboundOnCompletion("error")

	if len(stub.sent) != 1 {
		t.Fatalf("sent %d messages after a failed model call, want 1 notice", len(stub.sent))
	}
	got := stub.sent[0]
	if got.RecipientID != "999" || got.ThreadID != "999" {
		t.Errorf("notice went to %q/%q, want the owner's chat 999/999", got.RecipientID, got.ThreadID)
	}
	assertErrorNotice(t, got.Content)
	if h.outboundPending {
		t.Error("outboundPending still set after the failed run was flushed")
	}
}

func TestE33ModelErrorAfterPartialReplyFlagsIt(t *testing.T) {
	h, stub := aiTelegramHumanoid(t)
	h.outboundBuffer.WriteString("Here is the first half")
	h.flushOutboundOnCompletion("error")

	if len(stub.sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(stub.sent))
	}
	if !strings.HasPrefix(stub.sent[0].Content, "Here is the first half") {
		t.Errorf("partial reply lost: %q", stub.sent[0].Content)
	}
	assertErrorNotice(t, stub.sent[0].Content)
}

func TestE33SuccessfulEmptyRunSendsNothing(t *testing.T) {
	h, stub := aiTelegramHumanoid(t)
	h.flushOutboundOnCompletion("done")
	if len(stub.sent) != 0 {
		t.Fatalf("a run that finished with no text sent %d messages, want 0", len(stub.sent))
	}
}

// Human governance holds every reply for the owner at the pane, where the run's
// error is already shown; nothing goes to the chat on its own.
func TestE33ModelErrorUnderHumanGovernanceSendsNothing(t *testing.T) {
	h, stub := aiTelegramHumanoid(t)
	h.governance["telegram"] = "human"
	h.flushOutboundOnCompletion("error")
	if len(stub.sent) != 0 {
		t.Fatalf("human governance sent %d messages to the chat on an error, want 0", len(stub.sent))
	}
}
