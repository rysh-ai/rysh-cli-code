// SPDX-License-Identifier: Apache-2.0

package actors

// Tests for session-scoped trust grants (design 008 RA5). They reuse the
// spawn-less harness from humanoid_assistant_test.go: handlers driven directly
// on a HumanoidActor over an in-process NATS conn.

import (
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// lastSent returns the most recent outbound message body captured by the
// adapter, or "" when nothing was sent.
func lastSent(c *captureAdapter) string {
	if len(c.sent) == 0 {
		return ""
	}
	return c.sent[len(c.sent)-1].Content
}

func TestIsTrustCommand(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"trust 30m", true},
		{"trust off", true},
		{"trust", true},
		{"  TRUST 1h  ", true},
		// Only a leading "trust" is control input — otherwise an innocent
		// sentence would silently change the assistant's autonomy.
		{"can you trust this output?", false},
		{"please trust off", false},
		{"distrust", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isTrustCommand(c.in); got != c.want {
			t.Errorf("isTrustCommand(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestTrustGrantLifecycle(t *testing.T) {
	h, adapter, _ := newAssistantForTest(t, "owner-1")

	// Status before any grant: fail-closed.
	h.handleTrustCommand(assistantInbound("owner-1", "trust"))
	if h.trustActive() {
		t.Fatal("no grant should be active initially")
	}
	if got := lastSent(adapter); !strings.Contains(got, "trust is OFF") {
		t.Errorf("status reply = %q", got)
	}

	// Grant.
	h.handleTrustCommand(assistantInbound("owner-1", "trust 30m"))
	if !h.trustActive() {
		t.Fatal("grant should be active after `trust 30m`")
	}
	if got := lastSent(adapter); !strings.Contains(got, "trust granted") {
		t.Errorf("grant reply = %q", got)
	}

	// Revoke.
	h.handleTrustCommand(assistantInbound("owner-1", "trust off"))
	if h.trustActive() {
		t.Fatal("grant should be revoked after `trust off`")
	}
	if got := lastSent(adapter); !strings.Contains(got, "revoked") {
		t.Errorf("revoke reply = %q", got)
	}
}

// TestTrustGrantIsClamped: a grant can never be indefinite, and the clamp is
// stated rather than silently applied.
func TestTrustGrantIsClamped(t *testing.T) {
	h, adapter, _ := newAssistantForTest(t, "owner-1")
	h.handleTrustCommand(assistantInbound("owner-1", "trust 500h"))

	if d := time.Until(h.trustUntil); d > maxTrustWindow+time.Minute {
		t.Errorf("grant not clamped: %s remaining", d)
	}
	if got := lastSent(adapter); !strings.Contains(got, "clamped") {
		t.Errorf("clamp not reported to the owner: %q", got)
	}
}

func TestTrustGrantRejectsGarbage(t *testing.T) {
	h, adapter, _ := newAssistantForTest(t, "owner-1")
	h.handleTrustCommand(assistantInbound("owner-1", "trust forever"))
	if h.trustActive() {
		t.Error("unparseable duration must not grant trust")
	}
	if got := lastSent(adapter); !strings.Contains(got, "usage:") {
		t.Errorf("expected usage help, got %q", got)
	}
}

// TestTrustExpiryFailsClosed: an expired grant is not a grant.
func TestTrustExpiryFailsClosed(t *testing.T) {
	h, _, _ := newAssistantForTest(t, "owner-1")
	h.trustUntil = time.Now().Add(-time.Second)
	if h.trustActive() {
		t.Error("an expired grant must not be active")
	}
	req := &msg.MsgApprovalRequest{RequestID: "r1", Description: "rm -rf build/"}
	if h.trustAutoApprove(req) {
		t.Error("expired grant must not auto-approve")
	}
}

// TestTrustAutoApproveReleasesAndReports is the core behaviour: under a grant
// a held action is released, an approval response is published, and the owner
// is TOLD — a grant authorises a window, not silent action.
func TestTrustAutoApproveReleasesAndReports(t *testing.T) {
	h, adapter, drain := newAssistantForTest(t, "owner-1")
	h.lastInbound = assistantInbound("owner-1", "restart the dev server")
	h.handleTrustCommand(assistantInbound("owner-1", "trust 30m"))

	req := &msg.MsgApprovalRequest{RequestID: "req-42", Description: "pane_send: npm run dev"}
	h.handleApprovalRequest(req)

	// No pending hold — the action was released.
	if len(h.pendingApprovals) != 0 {
		t.Errorf("approval should not be held under a grant, pending=%v", h.pendingApprovals)
	}
	if h.trustAutoApproved != 1 {
		t.Errorf("trustAutoApproved = %d, want 1", h.trustAutoApproved)
	}

	// An approval response reached the orchestrator.
	envs := drain(500 * time.Millisecond)
	var approved bool
	for _, e := range envs {
		if strings.Contains(string(e.Payload), "req-42") && strings.Contains(string(e.Payload), "yes") {
			approved = true
		}
	}
	if !approved {
		t.Errorf("no approval response published, got %d envelope(s)", len(envs))
	}

	// And the owner was told what their grant released.
	if got := lastSent(adapter); !strings.Contains(got, "auto-approved under trust") ||
		!strings.Contains(got, "npm run dev") {
		t.Errorf("owner not informed of the released action: %q", got)
	}
}

// TestApprovalStillHeldWithoutTrust guards the PM3 default: with no grant, the
// assistant still fails closed and holds the request for a confirmation.
func TestApprovalStillHeldWithoutTrust(t *testing.T) {
	h, adapter, _ := newAssistantForTest(t, "owner-1")
	h.lastInbound = assistantInbound("owner-1", "delete the build dir")

	h.handleApprovalRequest(&msg.MsgApprovalRequest{RequestID: "req-9", Description: "bash: rm -rf build/"})

	if _, ok := h.pendingApprovals["req-9"]; !ok {
		t.Errorf("request must be held without a grant, pending=%v", h.pendingApprovals)
	}
	if got := lastSent(adapter); !strings.Contains(got, "approval required") {
		t.Errorf("owner not asked to confirm: %q", got)
	}
}

// TestTrustCommandIsNotSwallowedByPendingApproval is the ordering guarantee:
// while an approval is pending, an arbitrary reply is the approval DECISION —
// so "trust 30m" has to be recognised as control input first, or granting
// trust would instead read as rejecting the held action.
func TestTrustCommandIsNotSwallowedByPendingApproval(t *testing.T) {
	h, _, _ := newAssistantForTest(t, "owner-1")
	h.pendingApprovals = map[string]*pendingApproval{"req-held": {requestID: "req-held"}}

	h.handleInboundMessage(assistantInbound("owner-1", "trust 30m"))

	if !h.trustActive() {
		t.Fatal("`trust 30m` was consumed as an approval decision instead of granting trust")
	}
	if _, ok := h.pendingApprovals["req-held"]; !ok {
		t.Errorf("the held request should be untouched, pending=%v", h.pendingApprovals)
	}
}

// TestTrustCommandRequiresAdmittedSender is the security ordering guarantee:
// the trust parse sits AFTER the pairing gate, so a non-allowlisted sender
// cannot widen the assistant's autonomy.
func TestTrustCommandRequiresAdmittedSender(t *testing.T) {
	h, _, _ := newAssistantForTest(t, "owner-1")

	h.handleInboundMessage(assistantInbound("stranger-9", "trust 30m"))

	if h.trustActive() {
		t.Fatal("a non-allowlisted sender must not be able to grant trust")
	}
}
