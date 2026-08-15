// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"context"
	"reflect"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// genericGovInbound builds a Telegram inbound (a channel that uses the generic
// capture-as-draft governance path — no bespoke draft/send tools).
func genericGovInbound(content string) *msg.MsgHumanoidInboundMessage {
	return &msg.MsgHumanoidInboundMessage{
		ChannelType: "telegram",
		SenderID:    "999",
		SenderName:  "sam",
		ThreadID:    "999",
		Content:     content,
	}
}

// countingAdapter is a minimal ChannelAdapter that records the messages it was
// asked to Send, for asserting what actually reached the channel.
type countingAdapter struct {
	sent []channels.OutboundMessage
}

func (a *countingAdapter) Type() string                  { return "telegram" }
func (a *countingAdapter) Start(_ context.Context) error { return nil }
func (a *countingAdapter) Stop() error                   { return nil }
func (a *countingAdapter) Send(_ context.Context, o channels.OutboundMessage) error {
	a.sent = append(a.sent, o)
	return nil
}
func (a *countingAdapter) InboundCh() <-chan channels.InboundMessage { return nil }
func (a *countingAdapter) Status() msg.ChannelStatus                 { return msg.ChannelStatus{} }
func (a *countingAdapter) SetReplyMode(_ string)                     {}

// genericGovInboundThread is genericGovInbound with a distinct sender/thread,
// so a test can tell which inbound a posted reply was routed back to.
func genericGovInboundThread(thread, content string) *msg.MsgHumanoidInboundMessage {
	return &msg.MsgHumanoidInboundMessage{
		ChannelType: "telegram",
		SenderID:    thread,
		SenderName:  "sam",
		ThreadID:    thread,
		Content:     content,
	}
}

// newGenericGovHumanoid builds a human-governed telegram humanoid over the same
// embedded-NATS harness the FIFO queue tests use, with a countingAdapter
// standing in for the channel and a registered pane so banners are emitted.
func newGenericGovHumanoid(t *testing.T) (*HumanoidActor, *countingAdapter) {
	t.Helper()
	h := newHumanoidForQueueTest(t)
	stub := &countingAdapter{}
	h.name = "tg-bot"
	h.adapters = map[string]channels.ChannelAdapter{"telegram": stub}
	h.governance = map[string]string{"telegram": "human"}
	h.activeChatPaneID = "pane-1"
	return h, stub
}

// replyCompletes simulates the orchestrator streaming a reply for the inbound
// currently being processed and then reaching "done" — the exact path that
// captures a draft under generic human governance.
func replyCompletes(h *HumanoidActor, content string) {
	h.outboundBuffer.WriteString(content)
	h.flushOutboundOnCompletion("done")
	h.advanceInboundQueue()
}

// TestGenericDraftQueueSurvivesSecondInbound is the E-19 regression: under
// human governance on a generic channel, a second inbound whose reply completes
// while the first draft is still awaiting approval must NOT destroy the first
// draft. The owner never approved it and never discarded it, so both replies
// must still be postable — in arrival order, each back to its own thread.
//
// Against the single-slot `pendingGenericDraft` this fails: draft 2 overwrites
// draft 1, the first "send" posts "reply two" to thread t2, and the second
// "send" posts nothing at all.
func TestGenericDraftQueueSurvivesSecondInbound(t *testing.T) {
	h, stub := newGenericGovHumanoid(t)

	// First message arrives and its reply completes → draft 1 held for approval.
	h.handleInboundMessage(genericGovInboundThread("t1", "what's the eta?"))
	replyCompletes(h, "ETA is Friday.")
	if len(stub.sent) != 0 {
		t.Fatalf("human governance must not auto-send: %d message(s) reached the channel", len(stub.sent))
	}

	// Second message arrives and its reply completes while draft 1 is pending.
	h.handleInboundMessage(genericGovInboundThread("t2", "and the invoice?"))
	replyCompletes(h, "Invoice goes out Monday.")
	if len(stub.sent) != 0 {
		t.Fatalf("human governance must not auto-send: %d message(s) reached the channel", len(stub.sent))
	}

	// The owner now approves twice. Both replies must post, oldest first, each
	// routed back to the thread that asked for it.
	h.flushGenericDraft()
	h.flushGenericDraft()

	if len(stub.sent) != 2 {
		t.Fatalf("both approved drafts should post, got %d send(s): %+v", len(stub.sent), stub.sent)
	}
	if stub.sent[0].Content != "ETA is Friday." {
		t.Errorf("first send = %q, want the FIRST draft %q (it was overwritten)",
			stub.sent[0].Content, "ETA is Friday.")
	}
	if stub.sent[0].ThreadID != "t1" {
		t.Errorf("first send routed to thread %q, want t1", stub.sent[0].ThreadID)
	}
	if stub.sent[1].Content != "Invoice goes out Monday." {
		t.Errorf("second send = %q, want %q", stub.sent[1].Content, "Invoice goes out Monday.")
	}
	if stub.sent[1].ThreadID != "t2" {
		t.Errorf("second send routed to thread %q, want t2", stub.sent[1].ThreadID)
	}
}

// TestGenericDraftQueueDiscardDropsOnlyTheHead verifies "discard" removes just
// the draft the owner is looking at, leaving the rest of the queue intact —
// discarding one reply must not silently drop the others.
func TestGenericDraftQueueDiscardDropsOnlyTheHead(t *testing.T) {
	h, stub := newGenericGovHumanoid(t)

	h.handleInboundMessage(genericGovInboundThread("t1", "first"))
	replyCompletes(h, "first reply")
	h.handleInboundMessage(genericGovInboundThread("t2", "second"))
	replyCompletes(h, "second reply")

	h.discardGenericDraft() // drops "first reply" only
	h.flushGenericDraft()   // posts "second reply"

	if len(stub.sent) != 1 {
		t.Fatalf("expected exactly one send after discard+send, got %d: %+v", len(stub.sent), stub.sent)
	}
	if stub.sent[0].Content != "second reply" || stub.sent[0].ThreadID != "t2" {
		t.Errorf("surviving draft = %q on thread %q, want %q on t2",
			stub.sent[0].Content, stub.sent[0].ThreadID, "second reply")
	}
}

func TestIsToolGovernedChannel(t *testing.T) {
	for _, ct := range []string{"slack", "email", "whatsapp"} {
		if !isToolGovernedChannel(ct) {
			t.Errorf("%s should be tool-governed", ct)
		}
	}
	for _, ct := range []string{"telegram", "signal", "imessage", "phone", "discord", "chatbot"} {
		if isToolGovernedChannel(ct) {
			t.Errorf("%s should NOT be tool-governed (generic path)", ct)
		}
	}
}

// TestGenericGovernanceInit verifies a generic channel reads governance: from
// its contact block and defaults to ai — the same contract the three
// tool-governed channels already have.
func TestGenericGovernanceInit(t *testing.T) {
	h := NewHumanoidActor("tg-bot", "sp", map[string]msg.ChannelConfig{
		"telegram": {Governance: "human"},
	}, config.Config{}, nil, nil, nil, nil)
	if got := h.govMode("telegram"); got != "human" {
		t.Errorf("expected telegram governance human, got %q", got)
	}

	h2 := NewHumanoidActor("tg-bot2", "sp", map[string]msg.ChannelConfig{
		"telegram": {},
	}, config.Config{}, nil, nil, nil, nil)
	if got := h2.govMode("telegram"); got != "ai" {
		t.Errorf("expected default telegram governance ai, got %q", got)
	}
	// An unconfigured channel is ai by default too.
	if got := h2.govMode("signal"); got != "ai" {
		t.Errorf("expected unconfigured signal to be ai, got %q", got)
	}
}

// TestApplyGovernanceModeFlipsEveryChannel verifies the ##humanoid governance
// command now flips ALL channels — the tool-based ones (via their fields) and
// the generic ones (via the map) — and reports them sorted.
func TestApplyGovernanceModeFlipsEveryChannel(t *testing.T) {
	h := NewHumanoidActor("multi-bot", "sp", map[string]msg.ChannelConfig{
		"slack":    {},
		"telegram": {},
		"signal":   {},
	}, config.Config{}, nil, nil, nil, nil)

	switched := h.applyGovernanceMode("human")
	want := []string{"signal", "slack", "telegram"}
	if !reflect.DeepEqual(switched, want) {
		t.Fatalf("switched = %v, want %v", switched, want)
	}
	if h.govMode("slack") != "human" || h.govMode("telegram") != "human" || h.govMode("signal") != "human" {
		t.Errorf("all channels should be human: slack=%q telegram=%q signal=%q",
			h.govMode("slack"), h.govMode("telegram"), h.govMode("signal"))
	}

	// Flip back to ai.
	h.applyGovernanceMode("ai")
	if h.govMode("slack") != "ai" || h.govMode("telegram") != "ai" || h.govMode("signal") != "ai" {
		t.Errorf("all channels should be ai after flip back")
	}

	// A humanoid with no channels reports nothing switched.
	empty := NewHumanoidActor("empty", "sp", map[string]msg.ChannelConfig{}, config.Config{}, nil, nil, nil, nil)
	if got := empty.applyGovernanceMode("human"); len(got) != 0 {
		t.Errorf("empty humanoid should switch nothing, got %v", got)
	}
}

// TestGenericDraftFlushAndDiscard verifies the core capture-as-draft promise:
// a pending draft posts to its channel only on flush (the human's "send"), and
// discard drops it unsent. Both route through the same generic adapter path.
func TestGenericDraftFlushAndDiscard(t *testing.T) {
	stub := &countingAdapter{}
	inbound := genericGovInbound("what's the eta?")
	h := &HumanoidActor{
		name:          "tg-bot",
		adapters:      map[string]channels.ChannelAdapter{"telegram": stub},
		conversations: make(map[string]*ConversationContext),
		pendingGenericDrafts: []*genericDraft{{
			channelType: "telegram",
			content:     "ETA is Friday.",
			inbound:     inbound,
		}},
	}

	h.flushGenericDraft()
	if len(stub.sent) != 1 {
		t.Fatalf("flush should post exactly one message, got %d", len(stub.sent))
	}
	if stub.sent[0].Content != "ETA is Friday." {
		t.Errorf("posted content = %q", stub.sent[0].Content)
	}
	if stub.sent[0].RecipientID != inbound.SenderID || stub.sent[0].ThreadID != inbound.ThreadID {
		t.Errorf("draft routed to wrong recipient/thread: %+v", stub.sent[0])
	}
	if len(h.pendingGenericDrafts) != 0 {
		t.Error("draft should be cleared after flush")
	}

	// Discard: nothing more is sent, draft cleared.
	h.pendingGenericDrafts = []*genericDraft{{channelType: "telegram", content: "second", inbound: inbound}}
	h.discardGenericDraft()
	if len(stub.sent) != 1 {
		t.Errorf("discard must not send, total sends = %d", len(stub.sent))
	}
	if len(h.pendingGenericDrafts) != 0 {
		t.Error("draft should be cleared after discard")
	}
}
