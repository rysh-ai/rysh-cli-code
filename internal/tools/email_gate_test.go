package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// The email draft-and-confirm gate.
//
// email_send regressed exactly the way slack_send once did (defect 9): the
// "draft-and-confirm" guarantee was prompt text plus RequiresApproval, and a
// humanoid runs headless with auto-approve on, which short-circuits the
// approval check before RequiresApproval is consulted. These tests pin the
// enforcement in the tool (shared requireApprovedDraft gate), where no prompt
// and no flag can route around it. They mirror slack_gate_test.go on purpose:
// the three channels share one gate and must share one contract.

func gatedEmailSendTool(t *testing.T, humanMode bool) (*EmailSendTool, *channels.DraftStore) {
	t.Helper()
	drafts := channels.NewDraftStore()
	tool := NewEmailSendTool(
		channels.NewEmailAdapter(msg.ChannelConfig{}),
		drafts,
		func() bool { return humanMode },
	)
	return tool, drafts
}

func runEmail(t *testing.T, tool *EmailSendTool, params string) *ToolOutput {
	t.Helper()
	out, err := tool.Execute(context.Background(), json.RawMessage(params))
	if err != nil {
		t.Fatalf("Execute returned a transport error: %v", err)
	}
	return out
}

func TestEmailSend_HumanMode_RefusesDirectSend(t *testing.T) {
	tool, _ := gatedEmailSendTool(t, true)

	out := runEmail(t, tool, `{"to":"a@example.com","subject":"hi","body":"sent without asking"}`)
	if out.Error == "" {
		t.Fatal("a direct to+subject+body send must be refused under human governance")
	}
	if !strings.Contains(out.Error, "draft_id") {
		t.Fatalf("the refusal should tell the model what to do instead, got: %s", out.Error)
	}
}

func TestEmailSend_HumanMode_RefusesUnapprovedDraft(t *testing.T) {
	tool, drafts := gatedEmailSendTool(t, true)
	id := drafts.Create("email", "a@example.com", "hi", "drafted but not confirmed", "")

	out := runEmail(t, tool, `{"draft_id":"`+id+`"}`)
	if out.Error == "" {
		t.Fatal("an unapproved draft must not reach SMTP — this is the whole gate")
	}
	if !strings.Contains(out.Error, "not been approved") {
		t.Fatalf("refusal should name the reason, got: %s", out.Error)
	}
}

func TestEmailSend_HumanMode_RefusesUnknownDraft(t *testing.T) {
	tool, _ := gatedEmailSendTool(t, true)

	out := runEmail(t, tool, `{"draft_id":"draft-does-not-exist"}`)
	if out.Error == "" {
		t.Fatal("an unknown draft id must be refused, not treated as approval")
	}
}

// TestEmailSend_HumanMode_ApprovedDraftPassesTheGate proves the gate is a gate
// and not a wall: once the owner approves, the send proceeds past the check.
// The adapter has no SMTP host configured, so it fails at the transport —
// which is exactly how we know the gate itself let it through.
func TestEmailSend_HumanMode_ApprovedDraftPassesTheGate(t *testing.T) {
	tool, drafts := gatedEmailSendTool(t, true)
	id := drafts.Create("email", "a@example.com", "hi", "approved reply", "")
	if !drafts.Approve(id) {
		t.Fatal("Approve should find the draft it was just given")
	}

	out := runEmail(t, tool, `{"draft_id":"`+id+`"}`)
	for _, gateMsg := range []string{"not been approved", "requires the draft_id"} {
		if strings.Contains(out.Error, gateMsg) {
			t.Fatalf("an approved draft must clear the gate, got: %s", out.Error)
		}
	}
}

// TestEmailSend_HumanMode_ApprovedDraftIsConsumed pins exactly-once semantics:
// the draft is deleted after the send attempt, so replaying the same draft_id
// cannot send twice.
func TestEmailSend_HumanMode_ApprovedDraftIsConsumed(t *testing.T) {
	tool, drafts := gatedEmailSendTool(t, true)
	id := drafts.Create("email", "a@example.com", "hi", "approved reply", "")
	drafts.Approve(id)

	runEmail(t, tool, `{"draft_id":"`+id+`"}`)

	replay := runEmail(t, tool, `{"draft_id":"`+id+`"}`)
	if replay.Error == "" || !strings.Contains(replay.Error, "not found") {
		t.Fatalf("a consumed draft must not be sendable again, got: %s", replay.Error)
	}
}

func TestEmailSend_AIMode_IsUngated(t *testing.T) {
	tool, _ := gatedEmailSendTool(t, false)

	out := runEmail(t, tool, `{"to":"a@example.com","subject":"hi","body":"autonomous reply"}`)
	if strings.Contains(out.Error, "human governance is on") {
		t.Fatalf("ai mode must not be gated, got: %s", out.Error)
	}
}

// TestEmailSend_NilGateIsUngated covers the default construction used by
// non-humanoid callers.
func TestEmailSend_NilGateIsUngated(t *testing.T) {
	tool := NewEmailSendTool(channels.NewEmailAdapter(msg.ChannelConfig{}), channels.NewDraftStore(), nil)

	out := runEmail(t, tool, `{"to":"a@example.com","subject":"s","body":"x"}`)
	if strings.Contains(out.Error, "human governance is on") {
		t.Fatalf("a nil gate must not gate, got: %s", out.Error)
	}
}

// TestEmailSend_GateIsReadPerCall pins the runtime-switch behaviour: flipping
// `##humanoid governance` must take effect on the next call, not at respawn.
func TestEmailSend_GateIsReadPerCall(t *testing.T) {
	human := false
	drafts := channels.NewDraftStore()
	tool := NewEmailSendTool(
		channels.NewEmailAdapter(msg.ChannelConfig{}), drafts, func() bool { return human },
	)

	if out := runEmail(t, tool, `{"to":"a@example.com","subject":"s","body":"x"}`); strings.Contains(out.Error, "human governance is on") {
		t.Fatal("should be ungated while ai mode is active")
	}

	human = true // ##humanoid governance <name> human

	out := runEmail(t, tool, `{"to":"a@example.com","subject":"s","body":"x"}`)
	if out.Error == "" || !strings.Contains(out.Error, "human governance is on") {
		t.Fatalf("switching to human mode must gate the very next call, got: %s", out.Error)
	}
}

// TestEmailSend_GateHoldsUnderHeadlessAutoApprove documents WHY the gate lives
// in Execute: a headless humanoid auto-approves tool calls, meaning Execute is
// reached without any human in the loop even though RequiresApproval is true.
// Calling Execute directly is exactly what the executor does after
// auto-approve — and the gate must still refuse.
func TestEmailSend_GateHoldsUnderHeadlessAutoApprove(t *testing.T) {
	tool, drafts := gatedEmailSendTool(t, true)
	if !tool.RequiresApproval(nil) {
		t.Fatal("precondition: email_send declares RequiresApproval — the point is that this flag alone is not enforcement")
	}
	id := drafts.Create("email", "a@example.com", "hi", "unapproved", "")

	out := runEmail(t, tool, `{"draft_id":"`+id+`"}`)
	if out.Error == "" {
		t.Fatal("auto-approved execution must NOT bypass the owner-approval gate")
	}
	if !strings.Contains(out.Error, "not been approved") {
		t.Fatalf("the refusal must come from the approval gate, not the transport, got: %s", out.Error)
	}
}
