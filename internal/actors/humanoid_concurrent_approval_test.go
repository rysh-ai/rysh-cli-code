// SPDX-License-Identifier: Apache-2.0

package actors

// Regression tests for X5: concurrent chat threads must not share a single
// tool-approval slot. Before the fix, HumanoidActor held one pending approval
// (approvalPendingRequestID) and routed via the global lastInbound, so a second
// approval overwrote the first's id (orphaning its run until timeout), a reply
// on any thread resolved whichever request was most recent, and an orphaned
// run's done/error cleared another thread's still-pending approval.
//
// These drive the handlers directly on a HumanoidActor over in-process NATS,
// the same spawn-less pattern as humanoid_assistant_test.go / humanoid_queue_test.go.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// assistantInboundThread is assistantInbound with an explicit ThreadID, so a
// single owner can drive two concurrent conversations (thread-A / thread-B).
func assistantInboundThread(sender, thread, content string) *msg.MsgHumanoidInboundMessage {
	return &msg.MsgHumanoidInboundMessage{
		ChannelType: "telegram",
		SenderID:    sender,
		SenderName:  sender,
		ThreadID:    thread,
		Content:     content,
	}
}

// TestAssistantConcurrentApprovalsResolveByThread is the core X5 proof. Two runs
// on two threads each raise an approval; both are HELD simultaneously (neither
// overwrites the other), each notice routes back to the thread that triggered
// it, and a reply on one thread resolves that thread's request — not the other's
// and not "the most recent one".
func TestAssistantConcurrentApprovalsResolveByThread(t *testing.T) {
	h, adapter, drainResp := newAssistantForTest(t, "owner-1")

	// Thread A's run pauses on an approval.
	h.lastInbound = assistantInboundThread("owner-1", "thread-A", "book the flight")
	h.handleApprovalRequest(&msg.MsgApprovalRequest{
		RequestID: "req-A", OrchestratorID: "orch-A",
		Description: "bash: curl -X POST https://air…",
	})

	// Thread B's run pauses on its own approval. Under the old single-slot state
	// this overwrote req-A; now both are held.
	h.lastInbound = assistantInboundThread("owner-1", "thread-B", "email the landlord")
	h.handleApprovalRequest(&msg.MsgApprovalRequest{
		RequestID: "req-B", OrchestratorID: "orch-B",
		Description: "email_send",
	})

	if len(h.pendingApprovals) != 2 {
		t.Fatalf("want both approvals held, got %d: %+v", len(h.pendingApprovals), h.pendingApprovals)
	}
	// Neither run has been answered yet.
	if got := drainResp(200 * time.Millisecond); len(got) != 0 {
		t.Fatalf("no approval should be resolved yet; published %+v", got)
	}
	// Each notice routed back over the thread that triggered its own run.
	if len(adapter.sent) != 2 {
		t.Fatalf("want two approval notices, got %d: %+v", len(adapter.sent), adapter.sent)
	}
	if adapter.sent[0].ThreadID != "thread-A" || adapter.sent[1].ThreadID != "thread-B" {
		t.Fatalf("notices routed to wrong threads: [0]=%q [1]=%q, want thread-A then thread-B",
			adapter.sent[0].ThreadID, adapter.sent[1].ThreadID)
	}

	// The owner approves on THREAD B first — the *newer* request. This must
	// resolve req-B, not the oldest-held req-A, and must leave req-A untouched.
	// (Resolving the newer thread first is deliberate: a thread-blind resolver
	// that just answers the oldest pending would wrongly close req-A here.)
	h.handleInboundMessage(assistantInboundThread("owner-1", "thread-B", "yes"))
	respB := drainOneApproval(t, drainResp)
	if respB.RequestID != "req-B" || respB.Decision != msg.DecisionYes {
		t.Fatalf("thread-B reply resolved %+v, want req-B approved", respB)
	}
	if _, ok := h.pendingApprovals["req-A"]; !ok {
		t.Fatal("resolving thread-B orphaned thread-A's approval (X5 defect 1)")
	}
	if _, ok := h.pendingApprovals["req-B"]; ok {
		t.Fatal("req-B not cleared after its own decision")
	}

	// The owner then rejects on THREAD A; req-A resolves with its own id.
	h.handleInboundMessage(assistantInboundThread("owner-1", "thread-A", "no"))
	respA := drainOneApproval(t, drainResp)
	if respA.RequestID != "req-A" || respA.Decision != msg.DecisionNo {
		t.Fatalf("thread-A reply resolved %+v, want req-A rejected", respA)
	}
	if len(h.pendingApprovals) != 0 {
		t.Fatalf("approvals not fully drained: %+v", h.pendingApprovals)
	}
}

// TestAssistantReplyOnUnheldThreadDoesNotReleaseAnother covers the cross-thread
// routing hazard: an owner message on a thread that has NO pending approval must
// not be consumed as the decision for an approval held on a DIFFERENT thread. It
// falls through to the normal FIFO queue as an ordinary prompt instead.
func TestAssistantReplyOnUnheldThreadDoesNotReleaseAnother(t *testing.T) {
	h, _, drainResp := newAssistantForTest(t, "owner-1")

	// Thread A is in flight and pauses on an approval.
	h.handleInboundMessage(assistantInboundThread("owner-1", "thread-A", "book the flight"))
	if !h.inboundProcessing {
		t.Fatal("thread-A should be the in-flight run")
	}
	h.handleApprovalRequest(&msg.MsgApprovalRequest{
		RequestID: "req-A", OrchestratorID: "orch-A", Description: "bash: curl A",
	})

	// The owner sends an unrelated "yes" on thread B, which holds no approval.
	h.handleInboundMessage(assistantInboundThread("owner-1", "thread-B", "yes"))

	// req-A survives — thread B's message did NOT release it — and nothing was
	// published to the orchestrator's approval subject.
	if _, ok := h.pendingApprovals["req-A"]; !ok {
		t.Fatal("thread-B reply released thread-A's approval (X5 cross-thread leak)")
	}
	if got := drainResp(200 * time.Millisecond); len(got) != 0 {
		t.Fatalf("resolved an approval for a thread that had none: %+v", got)
	}
	// Thread B queued as an ordinary prompt behind the in-flight run.
	if len(h.inboundQueue) != 1 || h.inboundQueue[0].ThreadID != "thread-B" {
		t.Fatalf("thread-B should be queued as a normal prompt, queue=%+v", h.inboundQueue)
	}
}

// TestApprovalCleanupScopedToOrchestrator covers X5 defect 3: when an orphaned
// run finally completes (done/error) it must clear only ITS OWN held approval,
// keyed by the per-run OrchestratorID — never another concurrent run's.
func TestApprovalCleanupScopedToOrchestrator(t *testing.T) {
	h, _, _ := newAssistantForTest(t, "owner-1")

	h.lastInbound = assistantInboundThread("owner-1", "thread-A", "x")
	h.handleApprovalRequest(&msg.MsgApprovalRequest{RequestID: "req-A", OrchestratorID: "orch-A", Description: "A"})
	h.lastInbound = assistantInboundThread("owner-1", "thread-B", "y")
	h.handleApprovalRequest(&msg.MsgApprovalRequest{RequestID: "req-B", OrchestratorID: "orch-B", Description: "B"})

	// Run A gives up (its approval was never answered) and reports done/error.
	h.clearApprovalsForOrchestrator("orch-A")
	if _, ok := h.pendingApprovals["req-A"]; ok {
		t.Fatal("req-A not cleared by its own run's completion")
	}
	if _, ok := h.pendingApprovals["req-B"]; !ok {
		t.Fatal("req-B cleared by a different run's completion (X5 defect 3)")
	}

	// A status without a run id must sweep nothing (fail-safe, not fail-open).
	h.clearApprovalsForOrchestrator("")
	if _, ok := h.pendingApprovals["req-B"]; !ok {
		t.Fatal("blank orchestrator id swept a pending approval")
	}
}

// drainOneApproval reads exactly one MsgApprovalResponse off the drain window.
func drainOneApproval(t *testing.T, drain func(time.Duration) []natsEnvelope) msg.MsgApprovalResponse {
	t.Helper()
	envs := drain(time.Second)
	if len(envs) != 1 {
		t.Fatalf("want exactly one approval response, got %d: %+v", len(envs), envs)
	}
	var resp msg.MsgApprovalResponse
	if err := json.Unmarshal(envs[0].Payload, &resp); err != nil {
		t.Fatalf("decode approval response: %v", err)
	}
	return resp
}
