// SPDX-License-Identifier: Apache-2.0

package actors

// E33 slice 2: an approval request that something deliberately gated is never
// answered by the humanoid on its own.
//   - A non-assistant humanoid holds (does not auto-yes) a request when the
//     policy gated it (always_gate / bash.deny) or when its skill file says
//     auto_approve: false. Only pane input releases it: its chat members are
//     not the owner, so a channel reply never decides it. A stray request on
//     an auto-approving humanoid is still approved (Problem 2;
//     TestTeamHumanoidStillAutoApprovesStrayRequest).
//   - A trust grant ("trust 30m") never releases a policy-gated request; it
//     is held for the owner's explicit "yes" like any other.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

const policyGatedDesc = "bash: rm -rf build [gated by policy rule bash.deny[0]]"

// newTeamHumanoidForTest is newAssistantForTest with the assistant profile
// switched off: same owner-only allowlist, adapter and NATS wiring.
func newTeamHumanoidForTest(t *testing.T, owner string) (*HumanoidActor, *captureAdapter, func(time.Duration) []natsEnvelope) {
	t.Helper()
	h, adapter, drain := newAssistantForTest(t, owner)
	h.profile = "team"
	if h.isAssistantProfile() {
		t.Fatal("test humanoid still runs the assistant profile")
	}
	return h, adapter, drain
}

func onlyDecision(t *testing.T, envs []natsEnvelope, reqID string) msg.ApprovalDecision {
	t.Helper()
	if len(envs) != 1 {
		t.Fatalf("want one approval response, got %d", len(envs))
	}
	var resp msg.MsgApprovalResponse
	if err := json.Unmarshal(envs[0].Payload, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.RequestID != reqID {
		t.Fatalf("response for %q, want %q", resp.RequestID, reqID)
	}
	return resp.Decision
}

func TestTeamHumanoidHoldsGatedRequest(t *testing.T) {
	no := false
	for _, tc := range []struct {
		name        string
		autoApprove *bool
		desc        string
	}{
		{"policy gate, auto_approve default", nil, policyGatedDesc},
		{"auto_approve false", &no, "bash: make deploy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, adapter, drain := newTeamHumanoidForTest(t, "owner-1")
			h.autoApprove = tc.autoApprove
			h.lastInbound = remoteDM("owner-1", "t1", "go")

			h.handleApprovalRequest(&msg.MsgApprovalRequest{RequestID: "req-g", Description: tc.desc})
			if _, ok := h.pendingApprovals["req-g"]; !ok {
				t.Fatal("gated request not held")
			}
			if got := drain(300 * time.Millisecond); len(got) != 0 {
				t.Fatalf("gated request answered on its own (%d responses)", len(got))
			}
			if len(adapter.sent) != 0 {
				t.Fatalf("a team humanoid must not post the approval to the chat: %+v", adapter.sent)
			}

			// An admitted chat member's "yes" is not a decision on a team humanoid.
			h.handleInboundMessage(remoteDM("owner-1", "t1", "yes"))
			if _, ok := h.pendingApprovals["req-g"]; !ok {
				t.Fatal("a channel reply released the held request")
			}
			for _, e := range drain(300 * time.Millisecond) {
				var resp msg.MsgApprovalResponse
				if json.Unmarshal(e.Payload, &resp) == nil && resp.RequestID == "req-g" {
					t.Fatalf("a channel reply answered the held request: %+v", resp)
				}
			}

			// Pane input (MsgAgenticPrompt with an approval pending) decides it.
			h.handleTextApproval("yes", nil)
			if d := onlyDecision(t, drain(time.Second), "req-g"); d != msg.DecisionYes {
				t.Fatalf("pane yes gave %q", d)
			}
		})
	}
}

func TestTrustGrantDoesNotReleasePolicyGatedRequest(t *testing.T) {
	h, _, drain := newAssistantForTest(t, "owner-1")
	h.lastInbound = assistantInbound("owner-1", "clean up")
	h.handleTrustCommand(assistantInbound("owner-1", "trust 30m"))
	drain(200 * time.Millisecond)

	h.handleApprovalRequest(&msg.MsgApprovalRequest{RequestID: "req-p", Description: policyGatedDesc})
	if _, ok := h.pendingApprovals["req-p"]; !ok {
		t.Fatal("trust grant released a policy-gated request")
	}
	if h.trustAutoApproved != 0 {
		t.Errorf("trustAutoApproved = %d, want 0", h.trustAutoApproved)
	}
	if got := drain(300 * time.Millisecond); len(got) != 0 {
		t.Fatalf("policy-gated request answered under trust (%d responses)", len(got))
	}

	// The grant still releases what policy did not gate.
	h.handleApprovalRequest(&msg.MsgApprovalRequest{RequestID: "req-ok", Description: "pane_send: npm run dev"})
	if d := onlyDecision(t, drain(time.Second), "req-ok"); d != msg.DecisionYes {
		t.Fatalf("ungated request under trust gave %q", d)
	}

	// The owner's explicit "yes" still releases the policy-gated one.
	h.handleTextApproval("yes", nil)
	if d := onlyDecision(t, drain(time.Second), "req-p"); d != msg.DecisionYes {
		t.Fatalf("owner yes gave %q", d)
	}
}
