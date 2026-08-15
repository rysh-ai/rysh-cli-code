// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"context"
	"strings"
	"testing"
	"time"

	sharedmsg "github.com/rysh-ai/rysh-cli-shared/msg"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// slackGovInbound builds a Slack inbound message with the metadata the Socket
// Mode event loop attaches (channel, ts, thread_ts).
func slackGovInbound(content string) *msg.MsgHumanoidInboundMessage {
	return &msg.MsgHumanoidInboundMessage{
		ChannelType: "slack",
		SenderID:    "U1",
		SenderName:  "dana",
		ThreadID:    "1720000000.000100",
		Content:     content,
		Metadata: map[string]string{
			"channel":   "C0123ABC",
			"ts":        "1720000000.000100",
			"thread_ts": "",
		},
	}
}

func TestNewHumanoidActorSlackGovernanceInit(t *testing.T) {
	// Skill-file governance: human.
	h := NewHumanoidActor("gov-bot", "sp", map[string]msg.ChannelConfig{
		"slack": {Governance: "human"},
	}, config.Config{}, nil, nil, nil, nil)
	if h.slackGovernance != "human" {
		t.Errorf("expected slack governance human, got %q", h.slackGovernance)
	}
	if h.slackAdapter == nil {
		t.Error("expected pre-created slack adapter (shared message store for tools)")
	}

	// Default: ai.
	h2 := NewHumanoidActor("gov-bot2", "sp", map[string]msg.ChannelConfig{
		"slack": {},
	}, config.Config{}, nil, nil, nil, nil)
	if h2.slackGovernance != "ai" {
		t.Errorf("expected default slack governance ai, got %q", h2.slackGovernance)
	}
}

func TestBuildSlackGovernedPrompt(t *testing.T) {
	h := &HumanoidActor{lastInbound: slackGovInbound("is staging frozen on fridays?")}
	h.slackGovernance = "human"

	// "send" confirmations route to slack_send with the existing draft.
	for _, cmd := range []string{"send", "yes", "SEND IT", " ok "} {
		p := h.buildSlackGovernedPrompt(cmd)
		if !strings.Contains(p, "slack_send") || strings.Contains(p, "slack_draft with") {
			t.Errorf("confirmation %q should instruct slack_send only: %s", cmd, p)
		}
	}

	// Instructions inject the inbound context with channel + thread routing.
	p := h.buildSlackGovernedPrompt("reply and mention the release freeze")
	for _, want := range []string{"C0123ABC", "1720000000.000100", "slack_draft",
		"Do NOT call slack_send", "is staging frozen on fridays?"} {
		if !strings.Contains(p, want) {
			t.Errorf("instruction prompt missing %q:\n%s", want, p)
		}
	}
}

func TestBuildInboundSlackDraftPrompt(t *testing.T) {
	h := &HumanoidActor{}
	p := h.buildInboundSlackDraftPrompt(slackGovInbound("how do I roll back?"))
	for _, want := range []string{"HUMAN-GOVERNED", "slack_draft", "C0123ABC",
		"MUST NOT call slack_send", "how do I roll back?"} {
		if !strings.Contains(p, want) {
			t.Errorf("auto-draft prompt missing %q:\n%s", want, p)
		}
	}
}

// sendCountingAdapter is a ChannelAdapter stub that counts Send calls.
type sendCountingAdapter struct {
	sends int
}

func (a *sendCountingAdapter) Type() string                  { return "slack" }
func (a *sendCountingAdapter) Start(_ context.Context) error { return nil }
func (a *sendCountingAdapter) Stop() error                   { return nil }
func (a *sendCountingAdapter) Send(_ context.Context, _ channels.OutboundMessage) error {
	a.sends++
	return nil
}
func (a *sendCountingAdapter) InboundCh() <-chan channels.InboundMessage { return nil }
func (a *sendCountingAdapter) Status() msg.ChannelStatus                 { return msg.ChannelStatus{} }
func (a *sendCountingAdapter) SetReplyMode(_ string)                     {}

// TestSlackHumanModeSuppressesStepFlow verifies the "nothing posts until
// approved" promise covers the progress-step flow too: in human-governed mode
// forwardStepToChannel must not send even step titles to Slack, while ai mode
// forwards them as before.
func TestSlackHumanModeSuppressesStepFlow(t *testing.T) {
	stub := &sendCountingAdapter{}
	h := &HumanoidActor{
		lastInbound:     slackGovInbound("q"),
		outboundPending: true,
		slackGovernance: "human",
		adapters:        map[string]channels.ChannelAdapter{"slack": stub},
	}
	step := &msg.MsgAgenticStep{Kind: sharedmsg.StepRunStart, Title: "run started"}

	h.forwardStepToChannel(step)
	if stub.sends != 0 {
		t.Errorf("human mode must not forward step lines to Slack, got %d sends", stub.sends)
	}

	h.slackGovernance = "ai"
	h.forwardStepToChannel(step)
	if stub.sends != 1 {
		t.Errorf("ai mode should forward step lines to Slack, got %d sends", stub.sends)
	}
}

// TestSlackHumanGovernedInboundFiresDraftPrompt verifies the money-shot flow:
// a Slack inbound in human mode goes through the FIFO queue and fires a
// draft-only prompt at the LLM inbox (no auto-reply prompt), with the outbound
// flush armed so the humanGoverned predicate can suppress it on completion.
func TestSlackHumanGovernedInboundFiresDraftPrompt(t *testing.T) {
	h := newHumanoidForQueueTest(t)
	h.slackGovernance = "human"

	sub, err := h.nc.SubscribeSync(h.llmPromptExecInbox)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	h.handleInboundMessage(slackGovInbound("what's the install command?"))

	raw, err := sub.NextMsg(3 * time.Second)
	if err != nil {
		t.Fatalf("no prompt reached the LLM inbox: %v", err)
	}
	payload := string(raw.Data)
	if !strings.Contains(payload, "HUMAN-GOVERNED: prepare a reply DRAFT") {
		t.Errorf("expected the draft-only prompt, got: %s", payload)
	}
	if !strings.Contains(payload, "C0123ABC") {
		t.Errorf("draft prompt should carry the channel for reply routing: %s", payload)
	}
	if !h.outboundPending {
		t.Error("outboundPending should be armed (suppressed later by humanGoverned)")
	}
	if h.lastInbound == nil || h.lastInbound.ChannelType != "slack" {
		t.Error("lastInbound should be retained for the typed-send confirmation")
	}

	// AI mode: the same inbound must NOT use the draft-only prompt.
	h2 := newHumanoidForQueueTest(t)
	h2.slackGovernance = "ai"
	sub2, err := h2.nc.SubscribeSync(h2.llmPromptExecInbox)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() { _ = sub2.Unsubscribe() }()

	h2.handleInboundMessage(slackGovInbound("what's the install command?"))
	raw2, err := sub2.NextMsg(3 * time.Second)
	if err != nil {
		t.Fatalf("no prompt reached the LLM inbox in ai mode: %v", err)
	}
	if strings.Contains(string(raw2.Data), "HUMAN-GOVERNED: prepare a reply DRAFT") {
		t.Error("ai mode must not fire the draft-only prompt")
	}
}
