// SPDX-License-Identifier: Apache-2.0

package actors

// E33 G1 guard: making 1:1 DMs "addressed" (no observe_only) must not open the
// door. The pairing gate runs first, so a DM from a sender who is not on the
// allowlist is still held as a pairing request and never reaches the LLM,
// while the owner's DM is served.

import (
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func dmInbound(sender string) *msg.MsgHumanoidInboundMessage {
	return &msg.MsgHumanoidInboundMessage{
		ChannelType: "telegram",
		SenderID:    sender,
		SenderName:  sender,
		ThreadID:    sender,
		Content:     "hi",
		// What the adapters now emit for a 1:1 DM: addressed, no observe_only.
		Metadata: map[string]string{"direct_message": "true", "chat_type": "private"},
	}
}

func TestDirectMessageFromStrangerStillHeld(t *testing.T) {
	h, nc := newHumanoidForPairingTest(t, map[string]msg.ChannelConfig{
		"telegram": {Enabled: true, ReplyMode: "mentions", PairingPolicy: "request", Allowlist: []string{"424242"}},
	})
	pairingSub, err := nc.SubscribeSync(msg.T("humanoid", "gate-bot", "pairing"))
	if err != nil {
		t.Fatal(err)
	}
	llmSub, err := nc.SubscribeSync(h.llmPromptExecInbox)
	if err != nil {
		t.Fatal(err)
	}
	h.mergeDeclaredAllowlists() // Started does this at spawn

	h.handleInboundMessage(dmInbound("999000"))
	if h.inboundProcessing || len(h.inboundQueue) != 0 || h.lastInbound != nil {
		t.Fatal("a stranger's DM leaked into the inbound path")
	}
	envs := drainSubject(t, pairingSub, 500*time.Millisecond)
	if len(envs) != 1 || envs[0].TypeTag != msg.TagChannelPairRequest {
		t.Fatalf("want exactly one pairing request for the stranger's DM, got %d", len(envs))
	}
	if got := drainSubject(t, llmSub, 200*time.Millisecond); len(got) != 0 {
		t.Fatalf("a stranger's DM reached the LLM (%d messages)", len(got))
	}

	// The owner's DM is admitted and served.
	h.handleInboundMessage(dmInbound("424242"))
	if !h.inboundProcessing || h.lastInbound == nil || h.lastInbound.SenderID != "424242" {
		t.Fatal("the owner's DM was not served")
	}
}
