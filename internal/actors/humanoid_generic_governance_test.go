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
	}, config.Config{}, nil, nil, nil)
	if got := h.govMode("telegram"); got != "human" {
		t.Errorf("expected telegram governance human, got %q", got)
	}

	h2 := NewHumanoidActor("tg-bot2", "sp", map[string]msg.ChannelConfig{
		"telegram": {},
	}, config.Config{}, nil, nil, nil)
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
	}, config.Config{}, nil, nil, nil)

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
	empty := NewHumanoidActor("empty", "sp", map[string]msg.ChannelConfig{}, config.Config{}, nil, nil, nil)
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
		pendingGenericDraft: &genericDraft{
			channelType: "telegram",
			content:     "ETA is Friday.",
			inbound:     inbound,
		},
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
	if h.pendingGenericDraft != nil {
		t.Error("draft should be cleared after flush")
	}

	// Discard: nothing more is sent, draft cleared.
	h.pendingGenericDraft = &genericDraft{channelType: "telegram", content: "second", inbound: inbound}
	h.discardGenericDraft()
	if len(stub.sent) != 1 {
		t.Errorf("discard must not send, total sends = %d", len(stub.sent))
	}
	if h.pendingGenericDraft != nil {
		t.Error("draft should be cleared after discard")
	}
}
