// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// A human-governed channel never sends for an MCP client: the message is held
// in the owner's draft queue, and only their "send" posts it (design 067).
func TestHumanoidMCPSendHumanChannelOnlyDrafts(t *testing.T) {
	h, stub := newGenericGovHumanoid(t)
	h.active = true

	r := h.handleMCPSend(&msg.MsgHumanoidMCPSend{ChannelType: "telegram", RecipientID: "42", Content: "status: green"})
	if r.Error != "" || r.Outcome != "drafted" || r.Governance != "human" || r.Queued != 1 {
		t.Fatalf("reply = %+v, want drafted/human/1", r)
	}
	if len(stub.sent) != 0 {
		t.Fatalf("a human-governed MCP send reached the channel: %+v", stub.sent)
	}

	// The owner approves: now, and only now, it posts — to the recipient asked for.
	h.flushGenericDraft()
	if len(stub.sent) != 1 || stub.sent[0].RecipientID != "42" || stub.sent[0].Content != "status: green" {
		t.Fatalf("after owner send: %+v", stub.sent)
	}
}

func TestHumanoidMCPSendAIChannelSendsOnce(t *testing.T) {
	h, stub := newGenericGovHumanoid(t)
	h.active = true
	h.governance = map[string]string{"telegram": "ai"}

	r := h.handleMCPSend(&msg.MsgHumanoidMCPSend{ChannelType: "telegram", RecipientID: "42", Content: "hello", ThreadID: "7"})
	if r.Error != "" || r.Outcome != "sent" || r.Governance != "ai" {
		t.Fatalf("reply = %+v", r)
	}
	if len(stub.sent) != 1 || stub.sent[0].ThreadID != "7" {
		t.Fatalf("sent = %+v", stub.sent)
	}
	if len(h.pendingGenericDrafts) != 0 {
		t.Fatalf("an ai send must not leave a draft")
	}
}

func TestHumanoidMCPSendRefusals(t *testing.T) {
	h, stub := newGenericGovHumanoid(t)
	h.active = true
	for _, m := range []*msg.MsgHumanoidMCPSend{
		{ChannelType: "slack", RecipientID: "C1", Content: "x"}, // no such channel
		{ChannelType: "telegram", Content: "x"},                 // no recipient
		{ChannelType: "telegram", RecipientID: "42"},            // empty
	} {
		if r := h.handleMCPSend(m); r.Error == "" {
			t.Fatalf("%+v: want an error, got %+v", m, r)
		}
	}
	h.active = false
	if r := h.handleMCPSend(&msg.MsgHumanoidMCPSend{ChannelType: "telegram", RecipientID: "42", Content: "x"}); r.Error == "" {
		t.Fatal("a deactivated humanoid must refuse")
	}
	if len(stub.sent)+len(h.pendingGenericDrafts) != 0 {
		t.Fatalf("a refused send acted: sent=%d drafts=%d", len(stub.sent), len(h.pendingGenericDrafts))
	}
	_ = channels.OutboundMessage{}
}
