// SPDX-License-Identifier: Apache-2.0

package actors

// E33 G8: setup must not loosen approvals. These prove, on the humanoid side,
// that a tool call triggered by a REMOTE channel message is gated exactly like
// local input:
//   - auto_approve: false → the executor is told to gate (autoApproveTools is
//     what Started passes to SetAutoApproveAll), the request is held, routed
//     to the owner over the chat, and released only by the owner's "yes" on
//     that thread;
//   - a policy-gated request (always_gate / bash.deny) is held on the
//     assistant whatever auto_approve says (the orchestrator half — a policy
//     gate beats headless auto-approve — is rysh-shared
//     agentic TestDecideApproval_PolicyOverrides);
//   - governance: human still holds a reply to an addressed DM as a draft.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func remoteDM(sender, thread, content string) *msg.MsgHumanoidInboundMessage {
	return &msg.MsgHumanoidInboundMessage{
		ChannelType: "telegram",
		SenderID:    sender,
		SenderName:  sender,
		ThreadID:    thread,
		Content:     content,
		Metadata:    map[string]string{"direct_message": "true", "chat_type": "private"},
	}
}

func TestRemoteDMToolCallGatedWhenAutoApproveFalse(t *testing.T) {
	h, adapter, drainResp := newAssistantForTest(t, "owner-1")
	no := false
	h.autoApprove = &no
	if h.autoApproveTools() {
		t.Fatal("auto_approve: false must make the executor gate (SetAutoApproveAll(false))")
	}

	// The run was triggered by the owner's DM on thread t1.
	h.lastInbound = remoteDM("owner-1", "t1", "deploy the fix")
	h.handleApprovalRequest(&msg.MsgApprovalRequest{RequestID: "req-r1", Description: "bash: make deploy"})
	if _, ok := h.pendingApprovals["req-r1"]; !ok {
		t.Fatal("remote-triggered tool call not held for approval")
	}
	if got := drainResp(300 * time.Millisecond); len(got) != 0 {
		t.Fatalf("remote-triggered tool call auto-approved (%d responses)", len(got))
	}
	if len(adapter.sent) != 1 || adapter.sent[0].RecipientID != "owner-1" || adapter.sent[0].ThreadID != "t1" {
		t.Fatalf("approval notice not routed to the owner's thread: %+v", adapter.sent)
	}

	// A stranger's "yes" is held by the pairing gate and releases nothing.
	h.handleInboundMessage(remoteDM("stranger", "t9", "yes"))
	if _, ok := h.pendingApprovals["req-r1"]; !ok {
		t.Fatal("a stranger's yes cleared the approval")
	}
	if got := drainResp(200 * time.Millisecond); len(got) != 0 {
		t.Fatalf("a stranger's yes released the approval (%d responses)", len(got))
	}

	// The owner's "yes" on that thread is the decision.
	h.handleInboundMessage(remoteDM("owner-1", "t1", "yes"))
	envs := drainResp(time.Second)
	if len(envs) != 1 {
		t.Fatalf("want one approval response, got %d", len(envs))
	}
	var resp msg.MsgApprovalResponse
	if err := json.Unmarshal(envs[0].Payload, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.RequestID != "req-r1" || resp.Decision != msg.DecisionYes {
		t.Fatalf("response = %+v, want req-r1 approved", resp)
	}
}

func TestPolicyGatedRequestHeldWhateverAutoApprove(t *testing.T) {
	yes, no := true, false
	for _, aa := range []*bool{nil, &yes, &no} {
		h, _, drainResp := newAssistantForTest(t, "owner-1")
		h.autoApprove = aa
		h.lastInbound = remoteDM("owner-1", "t1", "clean up")
		h.handleApprovalRequest(&msg.MsgApprovalRequest{
			RequestID:   "req-p",
			Description: "bash: rm -rf build [gated by policy rule bash.deny[0]]",
		})
		if _, ok := h.pendingApprovals["req-p"]; !ok {
			t.Errorf("auto_approve=%v: policy-gated request not held", aa)
		}
		if got := drainResp(300 * time.Millisecond); len(got) != 0 {
			t.Errorf("auto_approve=%v: policy-gated request auto-approved", aa)
		}
	}
}

func TestHumanGovernanceHoldsAddressedDMReplyAsDraft(t *testing.T) {
	h, stub := newGenericGovHumanoid(t)
	h.handleInboundMessage(remoteDM("999", "999", "what's the status?"))
	replyCompletes(h, "All green.")
	if len(stub.sent) != 0 {
		t.Fatalf("governance: human auto-sent a reply to a DM (%d sends)", len(stub.sent))
	}
	h.flushGenericDraft() // the owner's explicit release
	if len(stub.sent) != 1 || stub.sent[0].Content != "All green." {
		t.Fatalf("released draft = %+v", stub.sent)
	}
}
